package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// syncEngine drives bidirectional repo sync between the desktop and a
// single upstream sandbox. One instance per `webdiff client` process —
// the runClient wiring constructs it after parsing flags and shares it
// with the /_local/sync handlers (and, in later phases, the turn-event
// subscriber and fsnotify watcher).
type syncEngine struct {
	upstream      *url.URL
	root          string // local mirror of sandbox rootDir (regular repos)
	worktreesRoot string // local mirror of sandbox worktreesRoot

	mu       sync.Mutex
	locks    map[string]*sync.Mutex // per-repo serialization, keyed by kind|name
	lastSync map[string]time.Time   // wall-clock of the last completed sync per key
	inFlight map[string]bool        // set while a sync is running

	hc *http.Client
}

func newSyncEngine(upstream *url.URL, root, worktreesRoot string) *syncEngine {
	return &syncEngine{
		upstream:      upstream,
		root:          root,
		worktreesRoot: worktreesRoot,
		locks:         map[string]*sync.Mutex{},
		lastSync:      map[string]time.Time{},
		inFlight:      map[string]bool{},
		// No timeout: pack uploads can run long on first sync of a
		// large repo. The git CLI's own watchdogs are the cap.
		hc: &http.Client{},
	}
}

// localPath returns the desktop-side absolute path for (kind, name).
// Existence is not checked for repos; a worktree's slug flattens
// `<repo>/<leaf>` into one segment, so it can only be turned back into a
// path by matching what's on disk — a worktree we haven't mirrored yet
// returns errNoRepo, and pullRepo falls back to localWorktreeCreatePath.
func (e *syncEngine) localPath(kind, name string) (string, error) {
	if !validRepoName(name) {
		return "", fmt.Errorf("invalid repo name %q", name)
	}
	switch kind {
	case "repos":
		return filepath.Join(e.root, name), nil
	case "worktrees":
		repos, err := os.ReadDir(e.worktreesRoot)
		if err != nil {
			return "", err
		}
		for _, repo := range repos {
			if !repo.IsDir() || strings.HasPrefix(repo.Name(), ".") {
				continue
			}
			leaf, ok := strings.CutPrefix(name, repo.Name()+"-")
			if !ok {
				continue
			}
			abs := filepath.Join(e.worktreesRoot, repo.Name(), leaf)
			if info, err := os.Stat(abs); err == nil && info.IsDir() {
				return abs, nil
			}
		}
		return "", fmt.Errorf("%w: worktree %q not mirrored locally", errNoRepo, name)
	default:
		return "", fmt.Errorf("unknown kind %q", kind)
	}
}

// localWorktreeCreatePath is where a not-yet-mirrored worktree should be
// created, splitting the slug on the parent repo name the sandbox
// reported. Only the pull path can use this — the slug alone is
// ambiguous about where the repo name ends.
func (e *syncEngine) localWorktreeCreatePath(name, parent string) (string, error) {
	if parent == "" {
		return "", fmt.Errorf("sandbox didn't report parent repo for worktree %q", name)
	}
	leaf, ok := strings.CutPrefix(name, parent+"-")
	if !ok || leaf == "" {
		return "", fmt.Errorf("worktree %q is not under reported parent %q", name, parent)
	}
	if !validRepoName(parent) || !validRepoName(leaf) {
		return "", fmt.Errorf("invalid worktree name %q/%q", parent, leaf)
	}
	return filepath.Join(e.worktreesRoot, parent, leaf), nil
}

func validRepoName(name string) bool {
	if name == "" || strings.HasPrefix(name, ".") || strings.ContainsAny(name, "/\\") {
		return false
	}
	return true
}

func (e *syncEngine) lock(kind, name string) *sync.Mutex {
	key := kind + "|" + name
	e.mu.Lock()
	defer e.mu.Unlock()
	m, ok := e.locks[key]
	if !ok {
		m = &sync.Mutex{}
		e.locks[key] = m
	}
	return m
}

func (e *syncEngine) markInFlight(kind, name string, v bool) {
	key := kind + "|" + name
	e.mu.Lock()
	defer e.mu.Unlock()
	if v {
		e.inFlight[key] = true
	} else {
		delete(e.inFlight, key)
		e.lastSync[key] = time.Now()
	}
}

// recentlySynced reports whether (kind, name) is currently syncing or
// finished a sync within `within`. The watcher uses this to suppress
// the fsnotify event burst that follows a pull — every file we just
// wrote causes an event the watcher would otherwise treat as a local
// edit and push back.
func (e *syncEngine) recentlySynced(kind, name string, within time.Duration) bool {
	key := kind + "|" + name
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.inFlight[key] {
		return true
	}
	if last, ok := e.lastSync[key]; ok && time.Since(last) < within {
		return true
	}
	return false
}

// gitURL is what we hand to `git fetch`/`git push` for (kind, name).
// We talk to the sandbox directly (not via the desktop's reverse
// proxy) — the desktop is itself a Tailscale peer with identity, so
// auth.middleware on the sandbox accepts it.
func (e *syncEngine) gitURL(kind, name string) string {
	u := *e.upstream
	u.Path = "/api/git/" + kind + "/" + name
	return u.String()
}

// --- /_local/sync handlers ---

type syncReq struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

func (e *syncEngine) handleSyncPush(w http.ResponseWriter, r *http.Request) {
	e.handleSync(w, r, e.pushRepo)
}

func (e *syncEngine) handleSyncPull(w http.ResponseWriter, r *http.Request) {
	e.handleSync(w, r, e.pullRepo)
}

func (e *syncEngine) handleSync(w http.ResponseWriter, r *http.Request, do func(kind, name string) error) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req syncReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := do(req.Kind, req.Name); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- pull: sandbox → desktop ---

func (e *syncEngine) pullRepo(kind, name string) error {
	m := e.lock(kind, name)
	m.Lock()
	defer m.Unlock()
	e.markInFlight(kind, name, true)
	defer e.markInFlight(kind, name, false)

	// 1. Ask the sandbox to build a sync ref and tell us where HEAD is.
	build, err := e.remoteBuild(kind, name)
	if err != nil {
		return fmt.Errorf("remote build-ref: %w", err)
	}

	localAbs, err := e.localPath(kind, name)
	if errors.Is(err, errNoRepo) && kind == "worktrees" {
		localAbs, err = e.localWorktreeCreatePath(name, build.Parent)
	}
	if err != nil {
		return err
	}

	// 2. Ensure a local repo exists to fetch into. For regular repos we
	//    `git clone` if missing; for worktrees we `git worktree add`
	//    once the parent is present locally.
	if err := e.ensureLocalRepo(kind, name, localAbs, build); err != nil {
		return fmt.Errorf("ensure local repo: %w", err)
	}

	// 3. Fetch the sandbox's branch ref + sync ref into the local repo.
	gitURL := e.gitURL(kind, name)
	fetchArgs := []string{"-C", localAbs, "fetch", gitURL,
		"+" + syncRefName + ":" + syncRefName,
	}
	if build.Branch != "" {
		fetchArgs = append(fetchArgs, "+"+build.Branch+":"+build.Branch)
	}
	if out, err := exec.Command("git", fetchArgs...).CombinedOutput(); err != nil {
		return fmt.Errorf("git fetch: %w: %s", err, out)
	}

	// 4. Apply locally — same routine the sandbox would run on apply.
	ref := repoRef{abs: localAbs, kind: kind, name: name}
	if err := applySyncRef(ref, build.Head, build.Branch, syncRefName); err != nil {
		return fmt.Errorf("apply locally: %w", err)
	}
	return nil
}

func (e *syncEngine) remoteBuild(kind, name string) (syncRefBuild, error) {
	body, _ := json.Marshal(syncReq{Kind: kind, Name: name})
	u := *e.upstream
	u.Path = "/api/sync/build-ref"
	req, err := http.NewRequest("POST", u.String(), bytes.NewReader(body))
	if err != nil {
		return syncRefBuild{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.hc.Do(req)
	if err != nil {
		return syncRefBuild{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		buf := make([]byte, 1024)
		n, _ := resp.Body.Read(buf)
		return syncRefBuild{}, fmt.Errorf("status %d: %s", resp.StatusCode, string(buf[:n]))
	}
	var out syncRefBuild
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return syncRefBuild{}, err
	}
	return out, nil
}

// ensureLocalRepo creates the local repo (or worktree) if it doesn't
// already exist. For worktrees the parent repo must already be present
// locally — that's the natural ordering since the parent's sync (which
// always happens via /repos/<parent>) will have run first.
func (e *syncEngine) ensureLocalRepo(kind, name, localAbs string, build syncRefBuild) error {
	if isLocalGitRepo(localAbs) {
		return nil
	}
	switch kind {
	case "repos":
		if err := os.MkdirAll(filepath.Dir(localAbs), 0700); err != nil {
			return err
		}
		out, err := exec.Command("git", "clone", e.gitURL(kind, name), localAbs).CombinedOutput()
		if err != nil {
			return fmt.Errorf("git clone: %w: %s", err, out)
		}
		return nil
	case "worktrees":
		if build.Parent == "" {
			return fmt.Errorf("sandbox didn't report parent repo for worktree %q", name)
		}
		parentAbs := filepath.Join(e.root, build.Parent)
		if !isLocalGitRepo(parentAbs) {
			return fmt.Errorf("worktree %q needs parent repo %q present at %s first", name, build.Parent, parentAbs)
		}
		// Fetch the branch into the parent so `worktree add` has it.
		if build.Branch != "" {
			out, err := exec.Command("git", "-C", parentAbs, "fetch", e.gitURL("repos", build.Parent),
				"+"+build.Branch+":"+build.Branch).CombinedOutput()
			if err != nil {
				return fmt.Errorf("fetch branch into parent: %w: %s", err, out)
			}
		}
		// Detach as a starting point; applySyncRef will fix HEAD up
		// to the right branch/commit after we fetch sync refs into it.
		branchOrHead := build.Head
		if build.Branch != "" {
			branchOrHead = strings.TrimPrefix(build.Branch, "refs/heads/")
		}
		if err := os.MkdirAll(filepath.Dir(localAbs), 0700); err != nil {
			return err
		}
		out, err := exec.Command("git", "-C", parentAbs, "worktree", "add", localAbs, branchOrHead).CombinedOutput()
		if err != nil {
			return fmt.Errorf("git worktree add: %w: %s", err, out)
		}
		return nil
	default:
		return fmt.Errorf("unknown kind %q", kind)
	}
}

func isLocalGitRepo(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, ".git"))
	if err != nil {
		return false
	}
	// .git is a directory in regular repos, a file in worktrees.
	_ = info
	return true
}

// --- push: desktop → sandbox ---

func (e *syncEngine) pushRepo(kind, name string) error {
	m := e.lock(kind, name)
	m.Lock()
	defer m.Unlock()
	e.markInFlight(kind, name, true)
	defer e.markInFlight(kind, name, false)

	localAbs, err := e.localPath(kind, name)
	if err != nil {
		return err
	}
	if !isLocalGitRepo(localAbs) {
		return fmt.Errorf("%w: %s", errNoRepo, localAbs)
	}

	ref := repoRef{abs: localAbs, kind: kind, name: name}
	build, err := buildSyncRef(ref)
	if err != nil {
		return fmt.Errorf("build local sync ref: %w", err)
	}

	gitURL := e.gitURL(kind, name)
	pushArgs := []string{"-C", localAbs, "push", gitURL,
		"+" + syncRefName + ":" + syncRefName,
	}
	if build.Branch != "" {
		pushArgs = append(pushArgs, "+"+build.Branch+":"+build.Branch)
	}
	if out, err := exec.Command("git", pushArgs...).CombinedOutput(); err != nil {
		return fmt.Errorf("git push: %w: %s", err, out)
	}

	if err := e.remoteApply(kind, name, build); err != nil {
		return fmt.Errorf("remote apply-ref: %w", err)
	}
	return nil
}

func (e *syncEngine) remoteApply(kind, name string, b syncRefBuild) error {
	body, _ := json.Marshal(struct {
		Kind, Name, Head, Branch, Commit string
	}{kind, name, b.Head, b.Branch, b.Commit})
	u := *e.upstream
	u.Path = "/api/sync/apply-ref"
	req, err := http.NewRequest("POST", u.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		buf := make([]byte, 1024)
		n, _ := resp.Body.Read(buf)
		return fmt.Errorf("status %d: %s", resp.StatusCode, string(buf[:n]))
	}
	return nil
}
