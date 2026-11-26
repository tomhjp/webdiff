package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitHubRepo(t *testing.T) {
	tests := []struct {
		remote, want string
	}{
		{"git@github.com:owner/name.git", "owner/name"},
		{"https://github.com/owner/name", "owner/name"},
		{"https://github.com/owner/name.git/", "owner/name"},
		{"ssh://git@github.com/owner/name.git", "owner/name"},
		{"git://rogitproxy.corp.ts.net/tailscale/corp.git", "tailscale/corp"},
		{"/tmp/x/github/owner/name.git", "owner/name"},
	}
	for _, tt := range tests {
		got, err := gitHubRepo(tt.remote)
		if err != nil || got != tt.want {
			t.Errorf("gitHubRepo(%q) = %q, %v, want %q", tt.remote, got, err, tt.want)
		}
	}
	for _, bad := range []string{"", "https://github.com/", "git@github.com:name"} {
		if got, err := gitHubRepo(bad); err == nil {
			t.Errorf("gitHubRepo(%q) = %q, want an error", bad, got)
		}
	}
}

func TestCompareURL(t *testing.T) {
	got := compareURL("owner/name", "main", "tomhjp/fix#1")
	want := "https://github.com/owner/name/compare/main...tomhjp/fix%231"
	if got != want {
		t.Errorf("compareURL = %q, want %q", got, want)
	}
}

func TestClientGuard(t *testing.T) {
	self := netip.MustParseAddr("100.64.0.1")
	g := &clientGuard{
		hosts:   allowedHosts("webdiff", []string{"100.64.0.1:80"}),
		selfIPs: []netip.Addr{self},
		next: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusTeapot)
		}),
	}
	tests := []struct {
		name   string
		method string
		path   string
		remote string
		host   string
		origin string
		ctype  string
		want   int
	}{
		{"page", "GET", "/", "100.64.0.1:5000", "webdiff", "", "", http.StatusTeapot},
		{"loopback", "GET", "/", "127.0.0.1:5000", "webdiff", "", "", http.StatusTeapot},
		{"ip host", "GET", "/", "100.64.0.1:5000", "100.64.0.1", "", "", http.StatusTeapot},
		{"other device", "GET", "/", "100.64.0.2:5000", "webdiff", "", "", http.StatusForbidden},
		{"rebinding", "GET", "/", "100.64.0.1:5000", "evil.example", "", "", http.StatusMisdirectedRequest},
		{"send", "POST", "/_local/github/send", "100.64.0.1:5000", "webdiff", "http://webdiff", "application/json", http.StatusTeapot},
		{"send cross-site", "POST", "/_local/github/send", "100.64.0.1:5000", "webdiff", "http://evil.example", "application/json", http.StatusForbidden},
		{"send no origin", "POST", "/_local/github/send", "100.64.0.1:5000", "webdiff", "", "application/json", http.StatusForbidden},
		{"send text", "POST", "/_local/github/send", "100.64.0.1:5000", "webdiff", "http://webdiff", "text/plain", http.StatusUnsupportedMediaType},
		{"send get", "GET", "/_local/github/send", "100.64.0.1:5000", "webdiff", "", "", http.StatusMethodNotAllowed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(tt.method, "http://"+tt.host+tt.path, nil)
			r.RemoteAddr = tt.remote
			if tt.origin != "" {
				r.Header.Set("Origin", tt.origin)
			}
			if tt.ctype != "" {
				r.Header.Set("Content-Type", tt.ctype)
			}
			w := httptest.NewRecorder()
			g.ServeHTTP(w, r)
			if w.Code != tt.want {
				t.Errorf("status = %d, want %d", w.Code, tt.want)
			}
		})
	}
}

// TestSendToGitHub sends the branch of a repo through a sandbox server
// to a bare repo that stands in for GitHub.
func TestSendToGitHub(t *testing.T) {
	repo, git, shas := newTestRepo(t)
	oldRoot, oldWorktrees := rootDir, worktreesRoot
	rootDir = filepath.Dir(repo)
	worktreesRoot = filepath.Join(rootDir, "wt")
	t.Cleanup(func() {
		rootDir, worktreesRoot = oldRoot, oldWorktrees
	})
	github := filepath.Join(filepath.Dir(rootDir), "github")

	mux := http.NewServeMux()
	registerGitRoutes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	upstream, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	s := &githubSender{
		upstream:  upstream,
		urlFormat: github + "/{repo}.git",
		cacheDir:  t.TempDir(),
	}
	// A change in the working tree is not sent.
	if err := os.WriteFile(filepath.Join(repo, "dirty"), []byte("x\n"), 0600); err != nil {
		t.Fatal(err)
	}
	send := func(branch, head string) (gitHubSendResponse, error) {
		return s.send(context.Background(), gitHubSendRequest{
			Kind:   "repos",
			Name:   "proj",
			Branch: branch,
			Head:   head,
		})
	}
	res, err := send("tomhjp/feature", shas[2])
	if err != nil {
		t.Fatal(err)
	}
	if res.CompareURL != "https://github.com/owner/proj/compare/main...tomhjp/feature" || res.Repo != "owner/proj" {
		t.Errorf("response = %+v", res)
	}
	remote := filepath.Join(github, "owner", "proj.git")
	out, err := exec.Command("git", "-C", remote, "rev-parse", "refs/heads/tomhjp/feature").Output()
	if err != nil || strings.TrimSpace(string(out)) != shas[2] {
		t.Errorf("GitHub branch = %q, %v, want %s", out, err, shas[2])
	}

	// A second send after a reword replaces the branch.
	rewritten, err := rewordCommit(context.Background(), repo, shas[0], "first\n")
	if err != nil {
		t.Fatal(err)
	}
	head := rewritten[shas[2]]
	if _, err := send("tomhjp/feature", shas[2]); err == nil {
		t.Errorf("send with an old HEAD: no error")
	}
	if _, err := send("tomhjp/feature", head); err != nil {
		t.Fatal(err)
	}
	out, _ = exec.Command("git", "-C", remote, "rev-parse", "refs/heads/tomhjp/feature").Output()
	if strings.TrimSpace(string(out)) != head {
		t.Errorf("GitHub branch = %q, want %s", out, head)
	}

	for _, bad := range []string{"", "main", "-x", "a..b"} {
		if _, err := send(bad, head); err == nil {
			t.Errorf("send to branch %q: no error", bad)
		}
	}

	// A managed worktree sends its own HEAD.
	wt := filepath.Join(worktreesRoot, "proj", "other")
	git("worktree", "add", "-q", "-b", "other", wt, "HEAD")
	if out, err := exec.Command("git", "-C", wt, "commit", "-q", "--allow-empty", "-m", "other").CombinedOutput(); err != nil {
		t.Fatalf("commit: %v: %s", err, out)
	}
	out, _ = exec.Command("git", "-C", wt, "rev-parse", "HEAD").Output()
	wtHead := strings.TrimSpace(string(out))
	if _, err := s.send(context.Background(), gitHubSendRequest{
		Kind:   "worktrees",
		Name:   "proj-other",
		Branch: "tomhjp/other",
		Head:   wtHead,
	}); err != nil {
		t.Fatal(err)
	}
	out, _ = exec.Command("git", "-C", remote, "rev-parse", "refs/heads/tomhjp/other").Output()
	if strings.TrimSpace(string(out)) != wtHead {
		t.Errorf("GitHub branch of the worktree = %q, want %s", out, wtHead)
	}
}

// TestGitHTTPBackendRefusesPush makes sure that the sandbox does not
// accept a push.
func TestGitHTTPBackendRefusesPush(t *testing.T) {
	repo, _, _ := newTestRepo(t)
	oldRoot := rootDir
	rootDir = filepath.Dir(repo)
	t.Cleanup(func() { rootDir = oldRoot })
	mux := http.NewServeMux()
	registerGitRoutes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	cmd := exec.Command("git", "-C", repo, "push", srv.URL+"/api/git/repos/proj", "HEAD:refs/heads/pushed")
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Errorf("push succeeded: %s", out)
	}
	if exec.Command("git", "-C", repo, "rev-parse", "--verify", "--quiet", "refs/heads/pushed").Run() == nil {
		t.Errorf("push created a branch")
	}
}
