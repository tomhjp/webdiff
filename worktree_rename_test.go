package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func renameTestWorktree(t *testing.T) (string, string) {
	t.Helper()
	oldRoot, oldWT, oldStats, oldPrefix := rootDir, worktreesRoot, statsManager, defaultBranchPrefix
	t.Cleanup(func() {
		rootDir, worktreesRoot, statsManager, defaultBranchPrefix = oldRoot, oldWT, oldStats, oldPrefix
	})
	rootDir = t.TempDir()
	worktreesRoot = filepath.Join(rootDir, "wt")
	statsManager = nil
	defaultBranchPrefix = "tomhjp/"
	t.Setenv("TMUX_TMPDIR", t.TempDir())
	t.Setenv("TMUX", "")
	repo := filepath.Join(rootDir, "repo")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	selectionGit(t, repo, "init", "-q", "-b", "main")
	selectionGit(t, repo, "config", "user.name", "Test")
	selectionGit(t, repo, "config", "user.email", "test@example.com")
	selectionGit(t, repo, "commit", "--allow-empty", "-qm", "initial")
	wt := filepath.Join(worktreesRoot, "repo", "source")
	selectionGit(t, repo, "worktree", "add", "-b", "tomhjp/source", wt)
	return repo, wt
}

func requestWorktreeRename(name, branch string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{
		"name":   name,
		"branch": branch,
	})
	w := httptest.NewRecorder()
	handleWorktreeRename(w, httptest.NewRequest(http.MethodPost, "/api/worktrees/rename", strings.NewReader(string(body))))
	return w
}

func TestWorktreeRename(t *testing.T) {
	_, wt := renameTestWorktree(t)
	if err := os.WriteFile(filepath.Join(wt, "staged"), []byte("staged\n"), 0600); err != nil {
		t.Fatal(err)
	}
	selectionGit(t, wt, "add", "staged")
	if err := os.WriteFile(filepath.Join(wt, "untracked"), []byte("untracked\n"), 0600); err != nil {
		t.Fatal(err)
	}
	status := selectionGit(t, wt, "status", "--porcelain")
	head := selectionGit(t, wt, "rev-parse", "HEAD")
	w := requestWorktreeRename("repo-source", " tomhjp/new-name ")
	if w.Code != http.StatusOK {
		t.Fatalf("rename: %d: %s", w.Code, w.Body.String())
	}
	var response map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(worktreesRoot, "repo", "new-name")
	if response["path"] != path || response["url"] != "/worktrees/repo-new-name/" {
		t.Fatalf("wrong identity: %v", response)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("old directory still exists: %v", err)
	}
	if got := selectionGit(t, path, "symbolic-ref", "--short", "HEAD"); got != "tomhjp/new-name" {
		t.Fatalf("branch = %s", got)
	}
	if got := selectionGit(t, path, "rev-parse", "HEAD"); got != head {
		t.Fatalf("HEAD changed: %s", got)
	}
	if got := selectionGit(t, path, "status", "--porcelain"); got != status {
		t.Fatalf("changes changed: %s, want %s", got, status)
	}
	if _, ok := resolveRepoRef("/worktrees/repo-source/"); ok {
		t.Fatal("old slug still resolves")
	}
	if ref, ok := resolveRepoRef(response["url"]); !ok || ref.abs != path {
		t.Fatalf("new slug does not resolve: %v", ref)
	}
	if got := selectionGit(t, path, "branch", "--list", "tomhjp/source"); got != "" {
		t.Fatalf("old branch still exists: %s", got)
	}
}

func TestWorktreeRenameRejectsAndRollsBack(t *testing.T) {
	for _, tc := range []struct {
		name   string
		branch string
		status int
		setup  func(*testing.T, string, string)
	}{
		{"empty", "", http.StatusBadRequest, nil},
		{"invalid", "bad name", http.StatusBadRequest, nil},
		{"option", "-oops", http.StatusBadRequest, nil},
		{"traversal", "../escape", http.StatusBadRequest, nil},
		{"empty leaf", "tomhjp/", http.StatusBadRequest, nil},
		{"existing branch", "main", http.StatusConflict, nil},
		{"directory collision", "tomhjp/target", http.StatusConflict, func(t *testing.T, repo, wt string) {
			if err := os.Mkdir(filepath.Join(filepath.Dir(wt), "target"), 0700); err != nil {
				t.Fatal(err)
			}
		}},
		{"slug collision", "tomhjp/tar-get", http.StatusConflict, func(t *testing.T, repo, wt string) {
			// Two different path pairs have the same slug.
			mkWorktree(t, "repo-tar", "get")
		}},
		{"locked move", "tomhjp/target", http.StatusBadGateway, func(t *testing.T, repo, wt string) {
			selectionGit(t, repo, "worktree", "lock", wt)
		}},
		{"detached", "tomhjp/target", http.StatusBadRequest, func(t *testing.T, repo, wt string) {
			selectionGit(t, wt, "checkout", "--detach")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, wt := renameTestWorktree(t)
			if tc.setup != nil {
				tc.setup(t, repo, wt)
			}
			before := currentBranch(wt)
			w := requestWorktreeRename("repo-source", tc.branch)
			if w.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", w.Code, tc.status, w.Body.String())
			}
			if got := currentBranch(wt); got != before {
				t.Fatalf("branch changed after failure: %s, want %s", got, before)
			}
		})
	}
}

func TestWorktreeRenameSameDirectory(t *testing.T) {
	_, wt := renameTestWorktree(t)
	for _, branch := range []string{"tomhjp/source", "TOMHJP/source"} {
		w := requestWorktreeRename("repo-source", branch)
		if w.Code != http.StatusOK || currentBranch(wt) != branch {
			t.Fatalf("same-directory rename: %d: %s", w.Code, w.Body.String())
		}
	}
}

func TestWorktreeRenameLiveSession(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	for _, branch := range []string{"tomhjp/target", "tomhjp/source.x"} {
		t.Run(branch, func(t *testing.T) {
			_, wt := renameTestWorktree(t)
			if branch == "tomhjp/source.x" {
				w := requestWorktreeRename("repo-source", "tomhjp/source-x")
				if w.Code != http.StatusOK {
					t.Fatalf("prepare: %d: %s", w.Code, w.Body.String())
				}
				wt = filepath.Join(filepath.Dir(wt), "source-x")
			}
			ctx := context.Background()
			oldSession := repoSessionName(wt)
			// A second agent session of the worktree is renamed too.
			oldSecond := checkoutSessionName(wt, 2)
			for _, name := range []string{oldSession, oldSecond} {
				out, err := tmuxOut(ctx, "new-session", "-d", "-s", name, "-c", wt, "sleep 300")
				if err != nil {
					t.Fatalf("start isolated tmux: %v: %s", err, out)
				}
			}
			newPath := filepath.Join(filepath.Dir(wt), worktreeLeaf(branch))
			t.Cleanup(func() {
				_, _ = tmuxOut(ctx, "kill-server")
				wdSessionsMu.Lock()
				for _, session := range []string{oldSession, oldSecond, repoSessionName(newPath), checkoutSessionName(newPath, 2)} {
					delete(wdSessionRepo, session)
					delete(wdSpinning, session)
					delete(wdLastRaw, session)
					delete(wdLastChange, session)
				}
				wdSessionsMu.Unlock()
			})
			recordRepoSession(oldSession, wt)
			pane, err := tmuxOut(ctx, "display-message", "-p", "-t", "="+oldSession, "#{pane_id}")
			if err != nil {
				t.Fatal(err)
			}
			secondPane, err := tmuxOut(ctx, "display-message", "-p", "-t", "="+oldSecond, "#{pane_id}")
			if err != nil {
				t.Fatal(err)
			}
			w := requestWorktreeRename(worktreeSlug(wt), branch)
			if w.Code != http.StatusOK {
				t.Fatalf("rename: %d: %s", w.Code, w.Body.String())
			}
			path := filepath.Join(filepath.Dir(wt), worktreeLeaf(branch))
			newSession := repoSessionName(path)
			got, err := tmuxOut(ctx, "display-message", "-p", "-t", "="+newSession, "#{pane_id}")
			if err != nil || got != pane {
				t.Fatalf("pane changed: %q, want %q: %v", got, pane, err)
			}
			newSecond := checkoutSessionName(path, 2)
			got, err = tmuxOut(ctx, "display-message", "-p", "-t", "="+newSecond, "#{pane_id}")
			if err != nil || got != secondPane {
				t.Fatalf("second pane changed: %q, want %q: %v", got, secondPane, err)
			}
			wdSessionsMu.RLock()
			ref := wdSessionRepo[newSession]
			wdSessionsMu.RUnlock()
			if ref.abs != path || ref.name != worktreeSlug(path) {
				t.Fatalf("stale session identity: %v", ref)
			}
		})
	}
}

func TestWorktreeRenameButtonPages(t *testing.T) {
	_, wt := renameTestWorktree(t)
	for _, tc := range []struct {
		name string
		url  string
		pane bool
		want bool
	}{
		{"worktree diff", "/worktrees/repo-source/", false, true},
		{"worktree pane", "/worktrees/repo-source/", true, true},
		{"repo diff", "/repos/repo/", false, false},
		{"repo pane", "/repos/repo/", true, false},
		{"unknown pane", "", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			if tc.pane {
				writeStreamPage(w, "%0", tc.url, "pane", "", "")
			} else {
				ref, ok := resolveRepoRef(tc.url)
				if !ok {
					t.Fatal("cannot resolve test repo")
				}
				renderRepo(w, httptest.NewRequest(http.MethodGet, tc.url, nil), ref)
			}
			if w.Code != http.StatusOK {
				t.Fatalf("render: %d: %s", w.Code, w.Body.String())
			}
			if got := strings.Contains(w.Body.String(), `id="wt-rename-btn"`); got != tc.want {
				t.Fatalf("rename button = %v, want %v", got, tc.want)
			}
			if tc.want && !strings.Contains(w.Body.String(), `data-branch="`+currentBranch(wt)+`"`) {
				t.Fatal("rename button has no current branch")
			}
		})
	}
}

func TestWorktreeRenameHTTPValidation(t *testing.T) {
	renameTestWorktree(t)
	for _, tc := range []struct {
		method string
		body   string
		status int
	}{
		{http.MethodGet, "", http.StatusMethodNotAllowed},
		{http.MethodPost, "{", http.StatusBadRequest},
		{http.MethodPost, `{"name":"missing","branch":"tomhjp/target"}`, http.StatusNotFound},
	} {
		w := httptest.NewRecorder()
		handleWorktreeRename(w, httptest.NewRequest(tc.method, "/api/worktrees/rename", strings.NewReader(tc.body)))
		if w.Code != tc.status {
			t.Fatalf("status = %d, want %d", w.Code, tc.status)
		}
	}
}

// The stream page has the same open and "+ worktree" buttons as the diff
// page, so the top-left buttons agree on both pages.
func TestStreamPageHeaderButtons(t *testing.T) {
	renameTestWorktree(t)
	saved := sshHost
	sshHost = "slop"
	defer func() { sshHost = saved }()
	for _, tc := range []struct {
		name string
		url  string
		want bool
	}{
		{"worktree", "/worktrees/repo-source/", true},
		{"repo", "/repos/repo/", true},
		{"unknown", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			writeStreamPage(w, "%0", tc.url, "pane", "", "")
			body := w.Body.String()
			for _, s := range []string{
				`href="zed://ssh/slop`,
				`href="vscode://vscode-remote/ssh-remote+slop`,
				`href="webdiff://ghostty?host=slop`,
				`id="wt-new-btn"`,
				`id="wt-create-row"`,
				`class="toggle wide-only" href="/history/?kind=`,
			} {
				if got := strings.Contains(body, s); got != tc.want {
					t.Errorf("%s present = %v, want %v", s, got, tc.want)
				}
			}
			if !tc.want {
				return
			}
			// "+ worktree" and rename are in the "more" menu, not in
			// the header row.
			start := strings.Index(body, `<div class="diff-more-menu" id="diff-more-menu" hidden>`)
			if start < 0 {
				t.Fatal("missing more menu")
			}
			menu := body[start : strings.Index(body[start:], `</div>`)+start]
			wants := []string{`id="wt-new-btn"`}
			if tc.name == "worktree" {
				wants = append(wants, `id="wt-rename-btn"`)
			}
			for _, s := range wants {
				if !strings.Contains(menu, s) {
					t.Errorf("menu is missing %s", s)
				}
			}
		})
	}
}

// The restart button has data-can-stop only for a webdiff-managed
// session, so the restart modal shows "stop session" only when the
// server can stop the session.
func TestStreamPageRestartCanStop(t *testing.T) {
	renameTestWorktree(t)
	for _, tc := range []struct {
		session string
		want    bool
	}{
		{"repo-source-wd", true},
		{"repo-source-wd2", true},
		{"other", false},
	} {
		t.Run(tc.session, func(t *testing.T) {
			w := httptest.NewRecorder()
			writeStreamPage(w, "%0", "/worktrees/repo-source/", "pane", tc.session, "")
			body := w.Body.String()
			start := strings.Index(body, `id="restart-session"`)
			if start < 0 {
				t.Fatal("missing restart button")
			}
			end := strings.Index(body[start:], `>`) + start
			if got := strings.Contains(body[start:end], `data-can-stop="1"`); got != tc.want {
				t.Fatalf("data-can-stop = %v, want %v", got, tc.want)
			}
		})
	}
}

// Remove is the last button in the right group of the stream page
// header, so it is always at the right edge.
func TestStreamPageRemoveLast(t *testing.T) {
	renameTestWorktree(t)
	w := httptest.NewRecorder()
	writeStreamPage(w, "%0", "/worktrees/repo-source/", "pane", "repo-source-wd", "")
	body := w.Body.String()
	right := strings.Index(body, `<div class="header-buttons-right">`)
	remove := strings.Index(body, `id="wt-remove-btn"`)
	if right < 0 || remove < right {
		t.Fatal("remove is not in the right group")
	}
	if !strings.Contains(body[remove:], `>remove</button></div></div>`) {
		t.Fatal("remove is not the last header button")
	}
}

// The stream page of a checkout has the same first heading row as the
// diff page: the parent repo as the title, and the branch at the right.
func TestStreamPageTitleRow(t *testing.T) {
	_, wt := renameTestWorktree(t)
	ref, ok := resolveRepoRef("/worktrees/repo-source/")
	if !ok {
		t.Fatal("cannot resolve worktree")
	}
	var want strings.Builder
	writeHeadingTitleRow(&want, ref, cachedStats(ref))
	w := httptest.NewRecorder()
	writeStreamPage(w, "%0", ref.url, "pane", "", "")
	body := w.Body.String()
	if !strings.Contains(body, `<div class="page-heading">`+want.String()) {
		t.Fatalf("stream page does not start with the shared title row: %s", body)
	}
	if !strings.Contains(want.String(), `<span class="head-branch">`+currentBranch(wt)+`</span>`) {
		t.Fatalf("title row has no branch: %s", want.String())
	}
	if !strings.Contains(want.String(), `<span class="title-short">repo</span>`) {
		t.Fatalf("title row does not name the parent repo: %s", want.String())
	}
}

// The stream page of session n > 1 keeps n in its diff link and its
// restart button, so both go to the same session again.
func TestStreamPageSessionNumber(t *testing.T) {
	renameTestWorktree(t)
	for _, tc := range []struct {
		session  string
		diffHref string
		n        string
		tag      bool
	}{
		{"repo-source-wd", `href="/worktrees/repo-source/">diff</a>`, `data-session-n="1"`, false},
		{"repo-source-wd2", `href="/worktrees/repo-source/?n=2">diff</a>`, `data-session-n="2"`, true},
	} {
		t.Run(tc.session, func(t *testing.T) {
			w := httptest.NewRecorder()
			writeStreamPage(w, "%0", "/worktrees/repo-source/", "pane", tc.session, "")
			body := w.Body.String()
			for _, want := range []string{tc.diffHref, tc.n, `href="/agent/worktrees/repo-source/?new=1"`} {
				if !strings.Contains(body, want) {
					t.Errorf("stream page does not contain %s", want)
				}
			}
			if got := strings.Contains(body, `class="session-tag"`); got != tc.tag {
				t.Errorf("session tag shown = %v, want %v", got, tc.tag)
			}
		})
	}
}
