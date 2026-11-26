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
	res, err := s.send(ctx, req)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *githubSender) send(ctx context.Context, req gitHubSendRequest) (gitHubSendResponse, error) {
	branch := strings.TrimSpace(req.Branch)
	if err := checkBranchName(branch); err != nil {
		return gitHubSendResponse{}, err
	}
	info, err := s.info(ctx, req.Kind, req.Name)
	if err != nil {
		return gitHubSendResponse{}, fmt.Errorf("read the checkout on the sandbox: %w", err)
	}
	if req.Head != "" && info.Head != req.Head {
		return gitHubSendResponse{}, errors.New("HEAD changed after the page loaded; reload the page and look at the new commits")
	}
	if branch == info.Main {
		return gitHubSendResponse{}, fmt.Errorf("cannot send to the main branch %q", branch)
	}
	repo, err := gitHubRepo(info.Remote)
	if err != nil {
		return gitHubSendResponse{}, err
	}
	pushURL := strings.ReplaceAll(s.urlFormat, "{repo}", repo)

	s.mu.Lock()
	defer s.mu.Unlock()

	dir := filepath.Join(s.cacheDir, filepath.FromSlash(repo)+".git")
	git := func(args ...string) error {
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
		// Fail, and do not wait for a password on the terminal of
		// the client. Credential helpers continue to work.
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err != nil {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return gitHubSendResponse{}, err
		}
		if err := git("init", "--bare", "-q"); err != nil {
			return gitHubSendResponse{}, err
		}
	}

	// Fetch the main branch too, so that the push can find the
	// commits that GitHub already has.
	src := *s.upstream
	src.Path = "/api/git/" + req.Kind + "/" + req.Name
	fetch := []string{"fetch", "--no-tags", "--no-write-fetch-head", src.String(), "+HEAD:refs/webdiff/head"}
	if info.MainRef != "" {
		fetch = append(fetch, "+"+info.MainRef+":refs/webdiff/main")
	}
	if err := git(fetch...); err != nil {
		return gitHubSendResponse{}, fmt.Errorf("fetch from the sandbox: %w", err)
	}
	got, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "refs/webdiff/head").Output()
	if err != nil {
		return gitHubSendResponse{}, fmt.Errorf("rev-parse: %w", err)
	}
	if head := strings.TrimSpace(string(got)); head != info.Head {
		return gitHubSendResponse{}, errors.New("HEAD changed during the send; reload the page")
	}

	// The branch can contain amended commits, so replace the branch
	// on GitHub.
	if err := git("-c", "push.negotiate=true", "push", "--quiet", pushURL, "+"+info.Head+":refs/heads/"+branch); err != nil {
		return gitHubSendResponse{}, fmt.Errorf("push to GitHub: %w", err)
	}
	return gitHubSendResponse{
		Repo:       repo,
		Branch:     branch,
		Head:       info.Head,
		CompareURL: compareURL(repo, info.Main, branch),
	}, nil
}

// info returns the gitHubInfo of a checkout from the sandbox.
func (s *githubSender) info(ctx context.Context, kind, name string) (gitHubInfo, error) {
	u := *s.upstream
	u.Path = "/api/github/info"
	u.RawQuery = url.Values{
		"kind": {kind},
		"name": {name},
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
