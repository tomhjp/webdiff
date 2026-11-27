package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// githubSender sends the branch of a sandbox checkout to GitHub, with
// the git credentials of the desktop. It fetches the commits from the
// sandbox into a bare repo in the cache directory of the desktop, and
// pushes them from there. The user's own checkouts do not change.
//
// The bare repo stays between sends, so that each send fetches only
// the new objects. It is a cache: it is safe to delete it.
type githubSender struct {
	upstream *url.URL
	// urlFormat is the URL to push to. "{repo}" in it changes to the
	// owner/name of the GitHub repo.
	urlFormat string
	// cacheDir holds a bare repo for each GitHub repo, at
	// <cacheDir>/<owner>/<name>.git.
	cacheDir string

	// mu makes sure that only one send runs at a time.
	mu sync.Mutex
}

// gitHubSendRequest is the body of POST /_local/github/send.
type gitHubSendRequest struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	// Branch is the name of the branch on GitHub.
	Branch string `json:"branch"`
	// Head is the SHA of HEAD that the diff page showed. The send
	// fails if HEAD is different, so that only reviewed commits go to
	// GitHub.
	Head string `json:"head"`
}

// gitHubSendResponse is the response of POST /_local/github/send.
type gitHubSendResponse struct {
	// Repo is the owner/name of the GitHub repo.
	Repo   string `json:"repo"`
	Branch string `json:"branch"`
	Head   string `json:"head"`
	// CompareURL is the GitHub page that compares the branch with the
	// main branch.
	CompareURL string `json:"compareURL"`
}

func (s *githubSender) handleSend(w http.ResponseWriter, r *http.Request) {
	var req gitHubSendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad JSON: "+err.Error(), nil)
		return
	}
	// Pushing a large repo for the first time can take a long time.
	// Do not stop when the browser goes away, because then the next
	// send must do the same work again.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Minute)
	defer cancel()
	src := &httpCheckout{
		upstream: s.upstream,
		kind:     req.Kind,
		name:     req.Name,
	}
	res, err := s.send(ctx, src, req.Branch, req.Head)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// checkout is a checkout on the sandbox that the sender can read.
type checkout interface {
	// info returns the gitHubInfo of the checkout.
	info(ctx context.Context) (gitHubInfo, error)
	// fetchURL returns the URL that git fetches the checkout from.
	fetchURL() string
}

// send pushes HEAD of src to branch on GitHub. If head is not empty,
// HEAD of src must be head.
func (s *githubSender) send(ctx context.Context, src checkout, branch, head string) (gitHubSendResponse, error) {
	branch = strings.TrimSpace(branch)
	if err := checkBranchName(branch); err != nil {
		return gitHubSendResponse{}, err
	}
	info, err := src.info(ctx)
	if err != nil {
		return gitHubSendResponse{}, fmt.Errorf("read the checkout on the sandbox: %w", err)
	}
	if head != "" && info.Head != head {
		return gitHubSendResponse{}, errors.New("HEAD changed; look at the new commits and try again")
	}
	if branch == info.Main {
		return gitHubSendResponse{}, fmt.Errorf("cannot send to the main branch %q", branch)
	}
	repo, err := gitHubRepo(info.Remote)
	if err != nil {
		return gitHubSendResponse{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.cacheRepo(ctx, repo)
	if err != nil {
		return gitHubSendResponse{}, err
	}
	if err := c.fetchCheckout(src, info); err != nil {
		return gitHubSendResponse{}, err
	}

	// The branch can contain amended commits, so replace the branch
	// on GitHub.
	if _, err := c.git("-c", "push.negotiate=true", "push", "--quiet", s.gitHubURL(repo), "+"+info.Head+":refs/heads/"+branch); err != nil {
		return gitHubSendResponse{}, fmt.Errorf("push to GitHub: %w", err)
	}
	return gitHubSendResponse{
		Repo:       repo,
		Branch:     branch,
		Head:       info.Head,
		CompareURL: compareURL(repo, info.Main, branch),
	}, nil
}

// gitHubURL returns the URL of the GitHub repo repo, an owner/name.
func (s *githubSender) gitHubURL(repo string) string {
	return strings.ReplaceAll(s.urlFormat, "{repo}", repo)
}

// cacheRepo is the bare repo of the sender for one GitHub repo.
type cacheRepo struct {
	ctx context.Context
	dir string
}

// cacheRepo returns the bare repo for the GitHub repo repo, and makes
// it if it is not there. The caller must hold s.mu.
func (s *githubSender) cacheRepo(ctx context.Context, repo string) (*cacheRepo, error) {
	c := &cacheRepo{
		ctx: ctx,
		dir: filepath.Join(s.cacheDir, filepath.FromSlash(repo)+".git"),
	}
	if _, err := os.Stat(filepath.Join(c.dir, "HEAD")); err != nil {
		if err := os.MkdirAll(c.dir, 0700); err != nil {
			return nil, err
		}
		if _, err := c.git("init", "--bare", "-q"); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// git runs git in the cache repo, and returns the output with no
// trailing space.
func (c *cacheRepo) git(args ...string) (string, error) {
	cmd := exec.CommandContext(c.ctx, "git", append([]string{"-C", c.dir}, args...)...)
	// Fail, and do not wait for a password on the terminal of the
	// client. Credential helpers continue to work.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}

// fetchCheckout fetches HEAD of src to refs/webdiff/head, and the main
// branch to refs/webdiff/main. The main branch lets a push to GitHub
// find the commits that GitHub already has. It returns an error if
// HEAD is not info.Head.
func (c *cacheRepo) fetchCheckout(src checkout, info gitHubInfo) error {
	fetch := []string{"fetch", "--no-tags", "--no-write-fetch-head", src.fetchURL(), "+HEAD:refs/webdiff/head"}
	if info.MainRef != "" {
		fetch = append(fetch, "+"+info.MainRef+":refs/webdiff/main")
	}
	if _, err := c.git(fetch...); err != nil {
		return fmt.Errorf("fetch from the sandbox: %w", err)
	}
	head, err := c.git("rev-parse", "refs/webdiff/head")
	if err != nil {
		return err
	}
	if head != info.Head {
		return errors.New("HEAD changed during the send; look at the new commits and try again")
	}
	return nil
}

// httpCheckout is a checkout that the sandbox serves at /api/git/ and
// /api/github/info.
type httpCheckout struct {
	upstream   *url.URL
	kind, name string
}

func (h *httpCheckout) fetchURL() string {
	u := *h.upstream
	u.Path = "/api/git/" + h.kind + "/" + h.name
	return u.String()
}

func (h *httpCheckout) info(ctx context.Context) (gitHubInfo, error) {
	u := *h.upstream
	u.Path = "/api/github/info"
	u.RawQuery = url.Values{
		"kind": {h.kind},
		"name": {h.name},
	}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return gitHubInfo{}, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return gitHubInfo{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return gitHubInfo{}, err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &e) == nil && e.Error != "" {
			return gitHubInfo{}, errors.New(e.Error)
		}
		return gitHubInfo{}, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var info gitHubInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return gitHubInfo{}, err
	}
	if info.Head == "" {
		return gitHubInfo{}, errors.New("the sandbox did not report HEAD")
	}
	return info, nil
}

// checkBranchName returns an error if branch is not a valid name for a
// new branch.
func checkBranchName(branch string) error {
	if branch == "" {
		return errors.New("type a branch name")
	}
	if strings.HasPrefix(branch, "-") || branch == "HEAD" ||
		exec.Command("git", "check-ref-format", "refs/heads/"+branch).Run() != nil {
		return fmt.Errorf("%q is not a valid branch name", branch)
	}
	return nil
}

// gitHubRepo returns the owner/name of the GitHub repo of the remote
// URL remote. It accepts URLs of GitHub, for example
// "git@github.com:owner/name.git" and "https://github.com/owner/name",
// and also URLs of mirrors and proxies of GitHub that keep the
// owner/name path, for example "git://proxy.example/owner/name.git".
// It uses the last two segments of the path.
func gitHubRepo(remote string) (string, error) {
	p := remote
	if u, err := url.Parse(remote); err == nil && u.Scheme != "" && u.Host != "" {
		p = u.Path
	} else if i := strings.Index(remote, ":"); i > 0 && !strings.Contains(remote[:i], "/") {
		// The scp form, for example "git@github.com:owner/name.git".
		p = remote[i+1:]
	}
	p = strings.TrimSuffix(strings.Trim(p, "/"), ".git")
	parts := strings.Split(p, "/")
	if len(parts) < 2 || parts[len(parts)-2] == "" || parts[len(parts)-1] == "" {
		return "", fmt.Errorf("cannot find the GitHub repo of remote %q", remote)
	}
	return parts[len(parts)-2] + "/" + parts[len(parts)-1], nil
}

// compareURL returns the GitHub page of repo that compares branch with
// the main branch.
func compareURL(repo, main, branch string) string {
	return "https://github.com/" + repo + "/compare/" + escapeRef(main) + "..." + escapeRef(branch)
}

// escapeRef escapes each segment of the branch name ref for a URL
// path, and keeps the slashes.
func escapeRef(ref string) string {
	parts := strings.Split(ref, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return path.Join(parts...)
}
