package main

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/tomhjp/webdiff/internal/repostats"
)

// registerGitRoutes adds the endpoints that the desktop client
// (`webdiff client`) uses to send a branch to GitHub.
func registerGitRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/git/", handleGitHTTPBackend)
	mux.HandleFunc("/api/github/info", handleGitHubInfo)
	mux.HandleFunc("/api/commits/message", handleCommitMessage)
}

// gitHubInfo is the response of /api/github/info. The desktop client
// uses it to send the branch of a checkout to GitHub.
type gitHubInfo struct {
	// Head is the SHA of HEAD.
	Head string `json:"head"`
	// Remote is the fetch URL of the default remote, for example
	// "git@github.com:owner/repo.git". The client finds the GitHub
	// repo from it.
	Remote string `json:"remote"`
	// Main is the name of the main branch, "main" or "master".
	Main string `json:"main"`
	// MainRef is the full name of the ref of the main branch that the
	// diff page compares with, for example "refs/remotes/origin/main".
	// It is empty if there is no main branch.
	MainRef string `json:"mainRef"`
}

// handleGitHubInfo returns the gitHubInfo of the checkout that the
// query parameters kind and name identify.
func handleGitHubInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed", nil)
		return
	}
	q := r.URL.Query()
	ref, ok := resolveRepoRef("/" + q.Get("kind") + "/" + q.Get("name") + "/")
	if !ok || !isGitRepo(ref.abs) {
		writeJSONError(w, http.StatusNotFound, "unknown repo", nil)
		return
	}
	head, err := gitRun(r.Context(), ref.abs, "", nil, "rev-parse", "HEAD")
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error(), nil)
		return
	}
	remote, ok := defaultRemote(ref.abs)
	if !ok {
		writeJSONError(w, http.StatusConflict, "the repo has no remote", nil)
		return
	}
	url, err := gitRun(r.Context(), ref.abs, "", nil, "remote", "get-url", remote)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error(), nil)
		return
	}
	info := gitHubInfo{
		Head:   head,
		Remote: url,
		Main:   mainBranch(ref.abs, remote),
	}
	// Use the first ref that exists, as branchBase does.
	for _, candidate := range repostats.MainRefs(remote) {
		if _, err := gitRun(r.Context(), ref.abs, "", nil, "rev-parse", "--verify", "--quiet", candidate); err == nil {
			info.MainRef = candidate
			break
		}
	}
	writeJSON(w, http.StatusOK, info)
}

// handleGitHTTPBackend wraps the stock `git http-backend` CGI program,
// so the desktop client can run `git fetch` against
// /api/git/{repos,worktrees}/<name>/.... The auth middleware has
// already accepted the request. Only fetch is permitted: pushes are
// refused, so the endpoint cannot change a repo.
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
	if strings.Contains(gitPath, "receive-pack") || strings.Contains(r.URL.RawQuery, "receive-pack") {
		http.Error(w, "push is not permitted", http.StatusForbidden)
		return
	}
	ref, ok := resolveRepoRef("/" + kind + "/" + name + "/")
	if !ok || !isGitRepo(ref.abs) {
		http.NotFound(w, r)
		return
	}

	// PATH_INFO has to be the basename under projectRoot, not the URL
	// name: a worktree's slug flattens `<repo>/<leaf>` into one segment,
	// so the two differ and http-backend would look for a repo that
	// isn't there.
	projectRoot := filepath.Dir(ref.abs)

	// No REMOTE_USER: http-backend permits a push when it is set.
	env := append(os.Environ(),
		"GIT_PROJECT_ROOT="+projectRoot,
		"GIT_HTTP_EXPORT_ALL=1",
		"PATH_INFO=/"+filepath.Base(ref.abs)+gitPath,
		"REQUEST_METHOD="+r.Method,
		"QUERY_STRING="+r.URL.RawQuery,
		"CONTENT_TYPE="+r.Header.Get("Content-Type"),
		"REMOTE_ADDR="+r.RemoteAddr,
		"HTTP_GIT_PROTOCOL="+r.Header.Get("Git-Protocol"),
	)
	if cl := r.Header.Get("Content-Length"); cl != "" {
		env = append(env, "CONTENT_LENGTH="+cl)
	}
	if enc := r.Header.Get("Content-Encoding"); enc != "" {
		env = append(env, "HTTP_CONTENT_ENCODING="+enc)
	}

	cmd := exec.Command("git", "-c", "http.receivepack=false", "http-backend")
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
