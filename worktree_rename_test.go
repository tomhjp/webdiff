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
			out, err := tmuxOut(ctx, "new-session", "-d", "-s", oldSession, "-c", wt, "sleep 300")
			if err != nil {
				t.Fatalf("start isolated tmux: %v: %s", err, out)
			}
			t.Cleanup(func() {
				_, _ = tmuxOut(ctx, "kill-server")
				wdSessionsMu.Lock()
				for _, session := range []string{oldSession, repoSessionName(filepath.Join(filepath.Dir(wt), worktreeLeaf(branch)))} {
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
