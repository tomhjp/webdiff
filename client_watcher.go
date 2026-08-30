package main

import (
	"context"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// startLocalWatcher watches the desktop-side repo mirrors and pushes
// any local edits back to the sandbox. Runs for the lifetime of the
// client process; logs and continues on internal errors rather than
// killing the binary.
//
// Sync-induced events are suppressed by querying the engine for
// recent-sync state, so a pull's file writes don't immediately
// trigger a push.
func startLocalWatcher(ctx context.Context, engine *syncEngine) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("fsnotify NewWatcher: %v — auto-push disabled", err)
		return
	}
	lw := &localWatcher{
		engine:   engine,
		w:        w,
		watching: map[string]repoIdent{}, // dir → which repo it belongs to
		timers:   map[string]*time.Timer{},
	}
	if err := lw.rescan(); err != nil {
		log.Printf("watcher initial scan: %v", err)
	}
	go lw.run(ctx)
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := lw.rescan(); err != nil {
					log.Printf("watcher rescan: %v", err)
				}
			}
		}
	}()
}

type repoIdent struct {
	kind string
	name string
	abs  string
}

type localWatcher struct {
	engine *syncEngine
	w      *fsnotify.Watcher

	mu       sync.Mutex
	watching map[string]repoIdent   // watched dir path → owning repo
	timers   map[string]*time.Timer // repo key → debounce timer
}

const (
	debounceWindow    = 500 * time.Millisecond
	suppressionWindow = 2 * time.Second
)

// skipDir lists directory basenames we never recurse into when adding
// watches. .git is excluded entirely because:
//   - .git/objects has high churn during git operations (esp. the
//     packfile writes that happen during push/fetch).
//   - .git/refs/webdiff/sync is updated by buildSyncRef itself, which
//     would deadlock-style loop us back into another push.
//
// We rely on the working-tree changes that accompany git operations
// (e.g. `git checkout` rewrites the tracked files) to surface as
// regular file events.
var skipDir = map[string]bool{
	".git":         true,
	"node_modules": true,
	"target":       true,
	"build":        true,
	"dist":         true,
	".next":        true,
	".cache":       true,
	"vendor":       true,
}

func (lw *localWatcher) rescan() error {
	if err := lw.scanRoot("repos", lw.engine.root); err != nil {
		return err
	}
	return lw.scanWorktrees()
}

func (lw *localWatcher) scanRoot(kind, root string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, ent := range entries {
		if !ent.IsDir() || strings.HasPrefix(ent.Name(), ".") {
			continue
		}
		abs := filepath.Join(root, ent.Name())
		if !isLocalGitRepo(abs) {
			continue
		}
		ident := repoIdent{kind: kind, name: ent.Name(), abs: abs}
		if err := lw.addRepoWatches(ident); err != nil {
			log.Printf("watcher add %s/%s: %v", kind, ent.Name(), err)
		}
	}
	return nil
}

// scanWorktrees is scanRoot one level deeper: worktrees mirror the
// sandbox's `<worktreesRoot>/<repo>/<leaf>` layout, and the sync key is
// the flattened "<repo>-<leaf>" slug the sandbox uses in its URLs.
func (lw *localWatcher) scanWorktrees() error {
	repos, err := os.ReadDir(lw.engine.worktreesRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, repo := range repos {
		if !repo.IsDir() || strings.HasPrefix(repo.Name(), ".") {
			continue
		}
		leaves, err := os.ReadDir(filepath.Join(lw.engine.worktreesRoot, repo.Name()))
		if err != nil {
			continue
		}
		for _, leaf := range leaves {
			if !leaf.IsDir() || strings.HasPrefix(leaf.Name(), ".") {
				continue
			}
			abs := filepath.Join(lw.engine.worktreesRoot, repo.Name(), leaf.Name())
			if !isLocalGitRepo(abs) {
				continue
			}
			name := repo.Name() + "-" + leaf.Name()
			ident := repoIdent{kind: "worktrees", name: name, abs: abs}
			if err := lw.addRepoWatches(ident); err != nil {
				log.Printf("watcher add worktrees/%s: %v", name, err)
			}
		}
	}
	return nil
}

func (lw *localWatcher) addRepoWatches(ident repoIdent) error {
	return filepath.WalkDir(ident.abs, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // tolerate transient errors mid-walk
		}
		if !d.IsDir() {
			return nil
		}
		name := d.Name()
		if path != ident.abs && skipDir[name] {
			return filepath.SkipDir
		}
		lw.mu.Lock()
		_, already := lw.watching[path]
		if !already {
			if err := lw.w.Add(path); err == nil {
				lw.watching[path] = ident
			} else {
				lw.mu.Unlock()
				log.Printf("fsnotify add %s: %v", path, err)
				return nil
			}
		}
		lw.mu.Unlock()
		return nil
	})
}

func (lw *localWatcher) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			_ = lw.w.Close()
			return
		case err, ok := <-lw.w.Errors:
			if !ok {
				return
			}
			log.Printf("fsnotify error: %v", err)
		case ev, ok := <-lw.w.Events:
			if !ok {
				return
			}
			lw.handleEvent(ev)
		}
	}
}

func (lw *localWatcher) handleEvent(ev fsnotify.Event) {
	dir := filepath.Dir(ev.Name)
	lw.mu.Lock()
	ident, ok := lw.watching[dir]
	lw.mu.Unlock()
	if !ok {
		return
	}

	// A new directory inside a watched repo needs watches too. Be
	// permissive: even renames produce CREATE events on the new path.
	if ev.Op&fsnotify.Create != 0 {
		if info, err := os.Stat(ev.Name); err == nil && info.IsDir() {
			base := filepath.Base(ev.Name)
			if !skipDir[base] {
				_ = lw.addRepoWatches(repoIdent{kind: ident.kind, name: ident.name, abs: ident.abs})
			}
		}
	}

	// REMOVE on a watched directory means fsnotify already dropped the
	// watch; clean our map so a future re-create rewatches it.
	if ev.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
		lw.mu.Lock()
		delete(lw.watching, ev.Name)
		lw.mu.Unlock()
	}

	lw.kick(ident)
}

func (lw *localWatcher) kick(ident repoIdent) {
	key := ident.kind + "|" + ident.name
	lw.mu.Lock()
	if t, ok := lw.timers[key]; ok {
		t.Reset(debounceWindow)
		lw.mu.Unlock()
		return
	}
	lw.timers[key] = time.AfterFunc(debounceWindow, func() {
		lw.mu.Lock()
		delete(lw.timers, key)
		lw.mu.Unlock()
		if lw.engine.recentlySynced(ident.kind, ident.name, suppressionWindow) {
			return
		}
		log.Printf("auto-push %s/%s", ident.kind, ident.name)
		if err := lw.engine.pushRepo(ident.kind, ident.name); err != nil {
			log.Printf("auto-push %s/%s: %v", ident.kind, ident.name, err)
		}
	})
	lw.mu.Unlock()
}
