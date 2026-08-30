package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// syncRefName is the ref every sync builds/transfers. Living under
// refs/webdiff/ keeps it out of `git log --branches` and ordinary
// tab-completion, while still being a normal ref the smart-http
// protocol can fetch/push.
const syncRefName = "refs/webdiff/sync"

// syncCache holds a long-lived per-repo index file used by buildSyncRef.
// Reusing the index across calls keeps `git add -A` warm; rebuilding
// only when HEAD moves mirrors the diffEnv pattern in webdiff.go. The
// objects produced by add/write-tree go into the repo's real
// .git/objects (no GIT_OBJECT_DIRECTORY override), which is the
// load-bearing difference from diffEnv: we need those blobs visible to
// `git push` over smart-http.
type syncCache struct {
	mu  sync.Mutex
	dir string
}

var (
	syncCachesMu sync.Mutex
	syncCaches   = map[string]*syncCache{}
)

func getSyncCache(repoAbs string) *syncCache {
	syncCachesMu.Lock()
	defer syncCachesMu.Unlock()
	if c, ok := syncCaches[repoAbs]; ok {
		return c
	}
	h := sha256.Sum256([]byte(repoAbs))
	c := &syncCache{dir: filepath.Join(cacheRoot, "sync-indexes", hex.EncodeToString(h[:8]))}
	syncCaches[repoAbs] = c
	return c
}

// syncRefBuild is the JSON returned by /api/sync/build-ref and used by
// the desktop to fetch+apply.
type syncRefBuild struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Commit string `json:"commit"` // refs/webdiff/sync target
	Head   string `json:"head"`   // HEAD SHA at build time
	Branch string `json:"branch"` // full ref, e.g. "refs/heads/main"; empty if detached
	Parent string `json:"parent"` // parent repo name; non-empty only for worktrees
}

// buildSyncRef captures the repo's current state (HEAD + post-`git add
// -A` working tree) as a single commit and updates refs/webdiff/sync
// to point at it. The result is what a desktop-side `git fetch` will
// pull, or what `git push` from the desktop into the sandbox will
// match. Returns enough metadata for the receiver to reconstruct the
// HEAD reference (branch vs detached) after applying.
func buildSyncRef(ref repoRef) (syncRefBuild, error) {
	out := syncRefBuild{Kind: ref.kind, Name: ref.name}

	headOut, err := exec.Command("git", "-C", ref.abs, "rev-parse", "HEAD").Output()
	if err != nil {
		return out, fmt.Errorf("rev-parse HEAD: %w", err)
	}
	out.Head = strings.TrimSpace(string(headOut))

	if br, err := exec.Command("git", "-C", ref.abs, "symbolic-ref", "-q", "HEAD").Output(); err == nil {
		out.Branch = strings.TrimSpace(string(br))
	}

	if ref.kind == "worktrees" {
		if parent, ok := worktreeParentRepo(ref.abs); ok {
			out.Parent = parent
		}
	}

	c := getSyncCache(ref.abs)
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := os.MkdirAll(c.dir, 0700); err != nil {
		return out, err
	}
	indexFile := filepath.Join(c.dir, "index")
	headFile := filepath.Join(c.dir, "HEAD")

	rebuild := true
	if prev, err := os.ReadFile(headFile); err == nil && strings.TrimSpace(string(prev)) == out.Head {
		if _, err := os.Stat(indexFile); err == nil {
			rebuild = false
		}
	}

	env := append(os.Environ(), "GIT_INDEX_FILE="+indexFile)

	if rebuild {
		// HEAD moved (or first run). Re-seed the index from HEAD so
		// `git add -A` writes only the delta. read-tree discards
		// stat-cache info, so we only run it on rebuild.
		_ = os.Remove(indexFile)
		cmd := exec.Command("git", "-C", ref.abs, "read-tree", out.Head)
		cmd.Env = env
		if o, err := cmd.CombinedOutput(); err != nil {
			return out, fmt.Errorf("read-tree %s: %w: %s", out.Head, err, o)
		}
		if err := os.WriteFile(headFile, []byte(out.Head), 0600); err != nil {
			return out, err
		}
	}

	cmd := exec.Command("git", "-C", ref.abs, "add", "-A")
	cmd.Env = env
	if o, err := cmd.CombinedOutput(); err != nil {
		return out, fmt.Errorf("add -A: %w: %s", err, o)
	}

	treeOut, err := runEnv(env, "git", "-C", ref.abs, "write-tree")
	if err != nil {
		return out, fmt.Errorf("write-tree: %w", err)
	}
	tree := strings.TrimSpace(treeOut)

	msg := "webdiff sync " + time.Now().UTC().Format(time.RFC3339Nano)
	commitOut, err := runEnv(env, "git", "-C", ref.abs, "commit-tree", tree, "-p", out.Head, "-m", msg)
	if err != nil {
		return out, fmt.Errorf("commit-tree: %w", err)
	}
	out.Commit = strings.TrimSpace(commitOut)

	if err := exec.Command("git", "-C", ref.abs, "update-ref", syncRefName, out.Commit).Run(); err != nil {
		return out, fmt.Errorf("update-ref %s: %w", syncRefName, err)
	}
	return out, nil
}

// applySyncRef reproduces the sender's state on this repo: HEAD points
// at `head` (via `branch` if non-empty, else detached), working tree
// matches the syncRef tree, index matches HEAD. After this runs, `git
// status` here shows the same uncommitted diff that the sender saw.
func applySyncRef(ref repoRef, head, branch, syncRef string) error {
	git := func(args ...string) error {
		cmd := exec.Command("git", append([]string{"-C", ref.abs}, args...)...)
		if o, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, o)
		}
		return nil
	}

	// 1) Make HEAD point where the sender's HEAD pointed. The branch ref
	//    was already updated to `head` by the preceding fetch/push. For
	//    detached, we point HEAD at the commit directly.
	if branch != "" {
		if err := git("symbolic-ref", "HEAD", branch); err != nil {
			return err
		}
	} else {
		if err := git("update-ref", "--no-deref", "HEAD", head); err != nil {
			return err
		}
	}
	// 2) Worktree+index to HEAD (clean state).
	if err := git("reset", "--hard", "HEAD"); err != nil {
		return err
	}
	// 3) Overlay the sync tree onto worktree+index. read-tree --reset
	//    -u removes worktree files that aren't in the tree, which is
	//    what reproduces sender-side deletions.
	if err := git("read-tree", "--reset", "-u", syncRef); err != nil {
		return err
	}
	// 4) Drop receiver-only untracked files. `git clean -fd` honors
	//    .gitignore, so node_modules/build artifacts on the receiver
	//    survive.
	if err := git("clean", "-fd"); err != nil {
		return err
	}
	// 5) Index back to HEAD's tree, worktree unchanged — `git status`
	//    now shows the same diff the sender saw.
	if err := git("reset"); err != nil {
		return err
	}
	return nil
}

func runEnv(env []string, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return string(out), fmt.Errorf("%w: %s", err, ee.Stderr)
		}
		return string(out), err
	}
	return string(out), nil
}

// --- HTTP handlers (sandbox side) ---

func registerSyncRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/sync/build-ref", handleBuildSyncRef)
	mux.HandleFunc("/api/sync/apply-ref", handleApplySyncRef)
	mux.HandleFunc("/api/git/", handleGitHTTPBackend)
}

func handleBuildSyncRef(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct{ Kind, Name string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	ref, ok := resolveRepoRef("/" + req.Kind + "/" + req.Name + "/")
	if !ok || !isGitRepo(ref.abs) {
		http.Error(w, "unknown repo", http.StatusNotFound)
		return
	}
	res, err := buildSyncRef(ref)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func handleApplySyncRef(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Kind, Name, Head, Branch, Commit string
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	ref, ok := resolveRepoRef("/" + req.Kind + "/" + req.Name + "/")
	if !ok || !isGitRepo(ref.abs) {
		http.Error(w, "unknown repo", http.StatusNotFound)
		return
	}
	if req.Head == "" || req.Commit == "" {
		http.Error(w, "head and commit required", http.StatusBadRequest)
		return
	}
	if err := applySyncRef(ref, req.Head, req.Branch, req.Commit); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleGitHTTPBackend wraps the stock `git http-backend` CGI program
// so the desktop can run `git fetch`/`git push` against
// /api/git/{repos,worktrees}/<name>/.... Auth has already happened in
// the safeweb/auth.middleware layer by the time we get here.
//
// The receivepack/denyCurrentBranch knobs are flipped lazily per repo
// — first request enables them. Without those, http-backend refuses
// the push or refuses to update the checked-out branch.
func handleGitHTTPBackend(w http.ResponseWriter, r *http.Request) {
	const prefix = "/api/git/"
	rest := strings.TrimPrefix(r.URL.Path, prefix)
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		http.NotFound(w, r)
		return
	}
	kind, name := parts[0], parts[1]
	gitPath := "/"
	if len(parts) == 3 && parts[2] != "" {
		gitPath = "/" + parts[2]
	}
	ref, ok := resolveRepoRef("/" + kind + "/" + name + "/")
	if !ok || !isGitRepo(ref.abs) {
		http.NotFound(w, r)
		return
	}
	if err := ensureGitHTTPConfig(ref.abs); err != nil {
		log.Printf("git http config %s: %v", ref.abs, err)
		http.Error(w, "git config", http.StatusInternalServerError)
		return
	}

	// PATH_INFO has to be the basename under projectRoot, not the URL
	// name: a worktree's slug flattens `<repo>/<leaf>` into one segment,
	// so the two differ and http-backend would look for a repo that
	// isn't there.
	projectRoot := filepath.Dir(ref.abs)

	env := append(os.Environ(),
		"GIT_PROJECT_ROOT="+projectRoot,
		"GIT_HTTP_EXPORT_ALL=1",
		"PATH_INFO=/"+filepath.Base(ref.abs)+gitPath,
		"REQUEST_METHOD="+r.Method,
		"QUERY_STRING="+r.URL.RawQuery,
		"CONTENT_TYPE="+r.Header.Get("Content-Type"),
		"REMOTE_USER=webdiff",
		"REMOTE_ADDR="+r.RemoteAddr,
		"HTTP_GIT_PROTOCOL="+r.Header.Get("Git-Protocol"),
	)
	if cl := r.Header.Get("Content-Length"); cl != "" {
		env = append(env, "CONTENT_LENGTH="+cl)
	}
	if enc := r.Header.Get("Content-Encoding"); enc != "" {
		env = append(env, "HTTP_CONTENT_ENCODING="+enc)
	}

	cmd := exec.Command("git", "http-backend")
	cmd.Env = env
	cmd.Stdin = r.Body
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := cmd.Start(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := streamCGI(w, stdout); err != nil {
		log.Printf("git http-backend %s %s: %v", r.Method, r.URL.Path, err)
	}
	if err := cmd.Wait(); err != nil {
		log.Printf("git http-backend %s %s wait: %v", r.Method, r.URL.Path, err)
	}
}

// streamCGI parses the CGI response on r — header lines, blank line,
// then the body — and writes it to w. The Status: header maps to the
// HTTP status code; everything else is set as a normal response
// header.
func streamCGI(w http.ResponseWriter, r io.Reader) error {
	br := bufio.NewReader(r)
	status := http.StatusOK
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return fmt.Errorf("read CGI header: %w", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if strings.EqualFold(k, "Status") {
			code := v
			if i := strings.Index(code, " "); i > 0 {
				code = code[:i]
			}
			if c, err := strconv.Atoi(code); err == nil {
				status = c
			}
			continue
		}
		w.Header().Add(k, v)
	}
	w.WriteHeader(status)
	_, err := io.Copy(w, br)
	return err
}

var (
	gitHTTPConfigMu   sync.Mutex
	gitHTTPConfigDone = map[string]bool{}
)

func ensureGitHTTPConfig(repoAbs string) error {
	gitHTTPConfigMu.Lock()
	if gitHTTPConfigDone[repoAbs] {
		gitHTTPConfigMu.Unlock()
		return nil
	}
	gitHTTPConfigDone[repoAbs] = true
	gitHTTPConfigMu.Unlock()

	for _, pair := range [][2]string{
		{"http.receivepack", "true"},
		// Push will be touching the checked-out branch; the apply step
		// resets worktree+index right after, so the default
		// "refuse to update checked-out branch" guard isn't useful here.
		{"receive.denyCurrentBranch", "ignore"},
	} {
		if err := exec.Command("git", "-C", repoAbs, "config", pair[0], pair[1]).Run(); err != nil {
			return fmt.Errorf("git config %s=%s: %w", pair[0], pair[1], err)
		}
	}
	return nil
}

// errNoRepo is returned by desktop-side helpers when the requested
// repo isn't on disk yet.
var errNoRepo = errors.New("repo not found")
