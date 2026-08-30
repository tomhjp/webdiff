// Package repostats discovers Git checkouts, watches them for changes, and
// maintains fresh working and branch diff summaries in memory.
package repostats

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/tomhjp/webdiff/internal/gitindex"
)

type Kind string

const (
	Repo     Kind = "repos"
	Worktree Kind = "worktrees"
)

type Summary struct{ Files, Ins, Del int }

type Target struct {
	Kind       Kind
	Name       string
	Path       string
	ParentRepo string
	Broken     bool
	Branch     string
	Working    Summary
	BranchDiff Summary
}

type Config struct {
	RootDir, WorktreesRoot string
	Debounce, MaxDebounce  time.Duration
	Workers                int
	Reconcile, FullRefresh time.Duration
}

type state struct {
	target                    Target
	dirty, queued, refreshing bool
	firstDirty                time.Time
	timer                     *time.Timer
	err                       error
}

type Manager struct {
	cfg     Config
	indexes *gitindex.Cache
	watcher *fsnotify.Watcher

	mu      sync.Mutex
	targets map[string]*state
	watched map[string]map[string]bool // directory -> target keys
	jobs    chan string
	changed chan struct{}
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

func New(cfg Config, indexes *gitindex.Cache) (*Manager, error) {
	if cfg.Debounce == 0 {
		cfg.Debounce = 500 * time.Millisecond
	}
	if cfg.MaxDebounce == 0 {
		cfg.MaxDebounce = 3 * time.Second
	}
	if cfg.Workers <= 0 {
		cfg.Workers = 2
	}
	if cfg.Reconcile == 0 {
		cfg.Reconcile = 30 * time.Second
	}
	if cfg.FullRefresh == 0 {
		cfg.FullRefresh = 10 * time.Minute
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	return &Manager{cfg: cfg, indexes: indexes, watcher: w, targets: map[string]*state{}, watched: map[string]map[string]bool{}, jobs: make(chan string, 1024), changed: make(chan struct{})}, nil
}

func key(kind Kind, name string) string { return string(kind) + "|" + name }

// Start discovers and refreshes every target before returning, then keeps the
// registry and summaries current until ctx is cancelled.
func (m *Manager) Start(parent context.Context) error {
	// Discover before installing the run context: reconcile only queues
	// newly found targets once workers exist, avoiding a large startup tree
	// blocking on the bounded job channel.
	if err := m.reconcile(); err != nil {
		return err
	}
	m.ctx, m.cancel = context.WithCancel(parent)
	for i := 0; i < m.cfg.Workers; i++ {
		m.wg.Add(1)
		go m.worker()
	}
	m.wg.Add(1)
	go m.runWatcher()
	m.wg.Add(1)
	go m.runPeriodic()
	m.mu.Lock()
	for _, s := range m.targets {
		m.queueLocked(s)
	}
	m.mu.Unlock()
	_, err := m.Current(m.ctx)
	return err
}

func (m *Manager) Close() error {
	if m.cancel != nil {
		m.cancel()
	}
	_ = m.watcher.Close()
	m.wg.Wait()
	return nil
}

// Current returns one coherent, fresh snapshot. A read flushes pending
// debounce timers and waits for dirty or active refreshes to finish.
func (m *Manager) Current(ctx context.Context) ([]Target, error) {
	for {
		m.mu.Lock()
		waiting := false
		for _, s := range m.targets {
			if s.dirty && !s.refreshing {
				m.queueLocked(s)
			}
			if s.dirty || s.queued || s.refreshing {
				waiting = true
			}
		}
		if !waiting {
			out := make([]Target, 0, len(m.targets))
			var errs []error
			for _, s := range m.targets {
				out = append(out, s.target)
				if s.err != nil && !s.target.Broken {
					errs = append(errs, fmt.Errorf("%s/%s: %w", s.target.Kind, s.target.Name, s.err))
				}
			}
			slices.SortFunc(out, func(a, b Target) int {
				if a.Kind != b.Kind {
					return strings.Compare(string(a.Kind), string(b.Kind))
				}
				return strings.Compare(a.Name, b.Name)
			})
			m.mu.Unlock()
			return out, errors.Join(errs...)
		}
		changed := m.changed
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		}
	}
}

func (m *Manager) Lookup(kind Kind, name string) (Target, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.targets[key(kind, name)]
	if s == nil {
		return Target{}, false
	}
	return s.target, true
}

// RefreshDiscovery synchronously reconciles target topology. Worktree API
// handlers call this after explicit create/remove operations.
func (m *Manager) RefreshDiscovery(ctx context.Context) error {
	if err := m.reconcile(); err != nil {
		return err
	}
	_, err := m.Current(ctx)
	return err
}

func (m *Manager) signalLocked() { close(m.changed); m.changed = make(chan struct{}) }

func (m *Manager) markDirtyLocked(s *state) {
	if !s.dirty {
		s.dirty = true
		s.firstDirty = time.Now()
	}
	if s.queued || s.refreshing {
		m.signalLocked()
		return
	}
	delay := m.cfg.Debounce
	if left := m.cfg.MaxDebounce - time.Since(s.firstDirty); left < delay {
		delay = left
	}
	if delay < 0 {
		delay = 0
	}
	if s.timer != nil {
		s.timer.Stop()
	}
	s.timer = time.AfterFunc(delay, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if s.dirty && !s.queued && !s.refreshing {
			m.queueLocked(s)
		}
	})
	m.signalLocked()
}

func (m *Manager) queueLocked(s *state) {
	if s.queued || s.refreshing {
		return
	}
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	s.queued = true
	m.signalLocked()
	select {
	case m.jobs <- key(s.target.Kind, s.target.Name):
	case <-m.ctx.Done():
	}
}

func (m *Manager) worker() {
	defer m.wg.Done()
	for {
		select {
		case <-m.ctx.Done():
			return
		case k := <-m.jobs:
			m.mu.Lock()
			s := m.targets[k]
			if s == nil || !s.queued {
				m.mu.Unlock()
				continue
			}
			s.queued = false
			s.refreshing = true
			s.dirty = false
			m.signalLocked()
			target := s.target
			m.mu.Unlock()
			updated, err := m.calculate(m.ctx, target)
			m.mu.Lock()
			if current := m.targets[k]; current == s {
				s.refreshing = false
				s.err = err
				if err == nil {
					s.target = updated
				}
				if s.dirty {
					m.markDirtyLocked(s)
				}
				m.signalLocked()
			}
			m.mu.Unlock()
		}
	}
}

func (m *Manager) calculate(ctx context.Context, t Target) (Target, error) {
	if t.Broken {
		return t, nil
	}
	env, release, err := m.indexes.Acquire(ctx, t.Path, string(t.Kind)+"-"+t.Name)
	if err != nil {
		return t, err
	}
	defer release()
	output := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = t.Path
		cmd.Env = env
		b, e := cmd.Output()
		return strings.TrimSpace(string(b)), e
	}
	branch, err := output("rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return t, err
	}
	base := "HEAD"
	var refs []string
	if remotes, e := output("remote"); e == nil {
		rs := strings.Fields(remotes)
		remote := ""
		for _, r := range rs {
			if r == "origin" {
				remote = r
				break
			}
		}
		if remote == "" && len(rs) == 1 {
			remote = rs[0]
		}
		if remote != "" {
			refs = append(refs, remote+"/main")
		}
	}
	refs = append(refs, "origin/main", "main")
	for _, ref := range refs {
		if b, e := output("merge-base", "HEAD", ref); e == nil && b != "" {
			base = b
			break
		}
	}
	working, err := stat(ctx, env, t.Path, "HEAD")
	if err != nil {
		return t, err
	}
	branchStat, err := stat(ctx, env, t.Path, base)
	if err != nil {
		return t, err
	}
	t.Branch = branch
	t.Working = working
	t.BranchDiff = branchStat
	return t, nil
}

func stat(ctx context.Context, env []string, dir, base string) (Summary, error) {
	cmd := exec.CommandContext(ctx, "git", "diff", "--cached", "--shortstat", base)
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		return Summary{}, err
	}
	var s Summary
	for _, part := range strings.Split(strings.TrimSpace(string(out)), ",") {
		part = strings.TrimSpace(part)
		var n int
		if _, e := fmt.Sscanf(part, "%d", &n); e != nil {
			continue
		}
		switch {
		case strings.Contains(part, "file"):
			s.Files = n
		case strings.Contains(part, "insertion"):
			s.Ins = n
		case strings.Contains(part, "deletion"):
			s.Del = n
		}
	}
	return s, nil
}

func (m *Manager) runWatcher() {
	defer m.wg.Done()
	for {
		select {
		case <-m.ctx.Done():
			return
		case err, ok := <-m.watcher.Errors:
			if !ok {
				return
			}
			log.Printf("repo stats watcher: %v; refreshing all targets", err)
			m.markAllDirty()
		case ev, ok := <-m.watcher.Events:
			if !ok {
				return
			}
			m.handleEvent(ev)
		}
	}
}

func (m *Manager) handleEvent(ev fsnotify.Event) {
	dir := filepath.Dir(ev.Name)
	m.mu.Lock()
	owners := m.watched[dir]
	var states []*state
	for k := range owners {
		if s := m.targets[k]; s != nil {
			states = append(states, s)
		}
	}
	m.mu.Unlock()
	for _, s := range states {
		if ev.Op&fsnotify.Create != 0 {
			if info, e := os.Stat(ev.Name); e == nil && info.IsDir() && !skipDir[info.Name()] {
				_ = m.addTree(s.target, ev.Name)
			}
		}
		m.mu.Lock()
		if m.targets[key(s.target.Kind, s.target.Name)] == s {
			m.markDirtyLocked(s)
		}
		m.mu.Unlock()
	}
}
func (m *Manager) markAllDirty() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.targets {
		m.markDirtyLocked(s)
	}
}

func (m *Manager) runPeriodic() {
	defer m.wg.Done()
	reconcile := time.NewTicker(m.cfg.Reconcile)
	full := time.NewTicker(m.cfg.FullRefresh)
	defer reconcile.Stop()
	defer full.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-reconcile.C:
			if err := m.reconcile(); err != nil {
				log.Printf("repo stats reconcile: %v", err)
			}
		case <-full.C:
			m.markAllDirty()
		}
	}
}

var skipDir = map[string]bool{".git": true, "node_modules": true, "target": true, "build": true, "dist": true, ".next": true, ".cache": true, "vendor": true}

func (m *Manager) addWatch(path, k string) {
	path = filepath.Clean(path)
	m.mu.Lock()
	owners := m.watched[path]
	if owners == nil {
		if err := m.watcher.Add(path); err != nil {
			m.mu.Unlock()
			return
		}
		owners = map[string]bool{}
		m.watched[path] = owners
	}
	owners[k] = true
	m.mu.Unlock()
}
func (m *Manager) addTree(t Target, root string) error {
	k := key(t.Kind, t.Name)
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if path != root && skipDir[d.Name()] {
			return filepath.SkipDir
		}
		m.addWatch(path, k)
		return nil
	})
}
func (m *Manager) addGitWatches(t Target) {
	k := key(t.Kind, t.Name)
	resolve := func(arg string) string {
		out, e := exec.Command("git", "-C", t.Path, "rev-parse", arg).Output()
		if e != nil {
			return ""
		}
		p := strings.TrimSpace(string(out))
		if !filepath.IsAbs(p) {
			p = filepath.Join(t.Path, p)
		}
		return filepath.Clean(p)
	}
	for _, arg := range []string{"--git-dir", "--git-common-dir"} {
		if p := resolve(arg); p != "" {
			m.addWatch(p, k)
			_ = filepath.WalkDir(filepath.Join(p, "refs"), func(path string, d fs.DirEntry, err error) error {
				if err == nil && d.IsDir() {
					m.addWatch(path, k)
				}
				return nil
			})
		}
	}
}

func (m *Manager) reconcile() error {
	found := map[string]Target{}
	entries, err := os.ReadDir(m.cfg.RootDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		p := filepath.Join(m.cfg.RootDir, e.Name())
		if _, statErr := os.Stat(filepath.Join(p, ".git")); statErr == nil {
			t := Target{Kind: Repo, Name: e.Name(), Path: p}
			found[key(t.Kind, t.Name)] = t
		}
	}
	parents, _ := os.ReadDir(m.cfg.WorktreesRoot)
	for _, p := range parents {
		if !p.IsDir() || strings.HasPrefix(p.Name(), ".") {
			continue
		}
		leaves, _ := os.ReadDir(filepath.Join(m.cfg.WorktreesRoot, p.Name()))
		for _, l := range leaves {
			if !l.IsDir() || strings.HasPrefix(l.Name(), ".") {
				continue
			}
			path := filepath.Join(m.cfg.WorktreesRoot, p.Name(), l.Name())
			_, gitErr := os.Stat(filepath.Join(path, ".git"))
			t := Target{Kind: Worktree, Name: p.Name() + "-" + l.Name(), Path: path, ParentRepo: p.Name(), Broken: gitErr != nil}
			found[key(t.Kind, t.Name)] = t
		}
	}
	var added []Target
	m.mu.Lock()
	for k, t := range found {
		if s := m.targets[k]; s == nil {
			m.targets[k] = &state{target: t, dirty: true, firstDirty: time.Now()}
			added = append(added, t)
		} else {
			s.target.Path = t.Path
			s.target.ParentRepo = t.ParentRepo
			s.target.Broken = t.Broken
		}
	}
	for k, s := range m.targets {
		if _, ok := found[k]; !ok {
			if s.timer != nil {
				s.timer.Stop()
			}
			delete(m.targets, k)
		}
	}
	m.signalLocked()
	m.mu.Unlock()
	for _, t := range added {
		if !t.Broken {
			_ = m.addTree(t, t.Path)
			m.addGitWatches(t)
		}
		// Reconciliation runs after startup as well as before it. New
		// targets need a job immediately; Current can then wait for their
		// first result instead of waiting for an unrelated filesystem event.
		m.mu.Lock()
		if s := m.targets[key(t.Kind, t.Name)]; s != nil && m.ctx != nil {
			m.queueLocked(s)
		}
		m.mu.Unlock()
	}
	return nil
}
