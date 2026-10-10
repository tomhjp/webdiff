package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/tomhjp/webdiff/internal/gitindex"
)

func selectionGit(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func selectionRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	selectionGit(t, repo, "init", "-q", "-b", "main")
	selectionGit(t, repo, "config", "user.name", "Test")
	selectionGit(t, repo, "config", "user.email", "test@example.com")
	selectionGit(t, repo, "commit", "--allow-empty", "-qm", "initial")
	selectionGit(t, repo, "checkout", "-qb", "feature")
	return repo
}

func selectionWrite(t *testing.T, repo, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestSelectionCombinations(t *testing.T) {
	repo := selectionRepo(t)
	for _, name := range []string{"a", "b", "c"} {
		selectionWrite(t, repo, name, name+"\n")
		selectionGit(t, repo, "add", name)
		selectionGit(t, repo, "commit", "-qm", name)
	}
	selectionWrite(t, repo, "working", "untracked\n")
	selectionWrite(t, repo, "staged", "staged\n")
	selectionGit(t, repo, "add", "staged")
	before := selectionGit(t, repo, "ls-files", "--stage")
	oldCache := indexCache
	indexCache = gitindex.New(filepath.Join(t.TempDir(), "indexes"))
	t.Cleanup(func() { indexCache = oldCache })
	commits, err := branchCommits(context.Background(), repo)
	if err != nil || len(commits) != 3 {
		t.Fatalf("commits = %+v, %v", commits, err)
	}
	for mask := 0; mask < 16; mask++ {
		selected := make(map[string]bool)
		var want []string
		for i, commit := range commits {
			if mask&(1<<i) != 0 {
				selected[commit.SHA] = true
				want = append(want, string(rune('a'+i)))
			}
		}
		if mask&8 != 0 {
			selected["working"] = true
			want = append(want, "staged", "working")
		}
		env, base, cleanup, err := selectionIndex(context.Background(), repo, commits, selected)
		if err != nil {
			t.Fatalf("mask %d: %v", mask, err)
		}
		got := changedFiles(env, repo, base)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("mask %d: files = %v, want %v", mask, got, want)
		}
		var temporaryIndex string
		for _, value := range env {
			if strings.HasPrefix(value, "GIT_INDEX_FILE=") {
				temporaryIndex = strings.TrimPrefix(value, "GIT_INDEX_FILE=")
			}
		}
		cleanup()
		if _, err := os.Stat(temporaryIndex); !os.IsNotExist(err) {
			t.Errorf("selection index was retained: %v", err)
		}
	}
	if got := selectionGit(t, repo, "ls-files", "--stage"); got != before {
		t.Fatal("selection changed the real index")
	}
}

func TestSelectionDependentChanges(t *testing.T) {
	repo := selectionRepo(t)
	for _, content := range []string{"one\n", "two\n", "three\n"} {
		selectionWrite(t, repo, "file", content)
		selectionGit(t, repo, "add", "file")
		selectionGit(t, repo, "commit", "-qm", content)
	}
	commits, err := branchCommits(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	for _, indexes := range [][]int{{2}, {0, 1, 2}} {
		selected := make(map[string]bool)
		for _, i := range indexes {
			selected[commits[i].SHA] = true
		}
		env, base, cleanup, err := selectionIndex(context.Background(), repo, commits, selected)
		if err != nil {
			t.Fatal(err)
		}
		if got := changedFiles(env, repo, base); !reflect.DeepEqual(got, []string{"file"}) {
			t.Errorf("files = %v", got)
		}
		cleanup()
	}
	_, _, cleanup, err := selectionIndex(context.Background(), repo, commits, map[string]bool{
		commits[0].SHA: true,
		commits[2].SHA: true,
	})
	defer cleanup()
	if err == nil || !strings.Contains(err.Error(), "prerequisite") {
		t.Fatalf("dependent selection should report a conflict: %v", err)
	}
}

func TestSelectionMergeAndBinary(t *testing.T) {
	repo := selectionRepo(t)
	selectionGit(t, repo, "checkout", "-qb", "side")
	selectionWrite(t, repo, "binary", "\x00one\xff")
	selectionGit(t, repo, "add", "binary")
	selectionGit(t, repo, "commit", "-qm", "binary on side")
	selectionGit(t, repo, "checkout", "-q", "feature")
	selectionWrite(t, repo, "feature", "feature\n")
	selectionGit(t, repo, "add", "feature")
	selectionGit(t, repo, "commit", "-qm", "feature")
	selectionGit(t, repo, "merge", "--no-ff", "-qm", "merge side", "side")
	commits, err := branchCommits(context.Background(), repo)
	if err != nil || len(commits) != 2 || commits[1].Subject != "merge side" {
		t.Fatalf("first-parent list = %+v: %v", commits, err)
	}
	env, base, cleanup, err := selectionIndex(context.Background(), repo, commits, map[string]bool{
		commits[1].SHA: true,
	})
	defer cleanup()
	if err != nil {
		t.Fatal(err)
	}
	if got := changedFiles(env, repo, base); !reflect.DeepEqual(got, []string{"binary"}) {
		t.Fatalf("merge diff = %v", got)
	}
	_, _, binary := fileDiffStat(env, repo, base, "binary")
	if !binary {
		t.Fatal("binary change was not preserved")
	}
}

func TestDiffPickerEscapesSubjects(t *testing.T) {
	var body strings.Builder
	sha := strings.Repeat("a", 40)
	writeDiffPicker(&body, []branchCommit{{
		SHA:     sha,
		Subject: "<script>alert(1)</script>",
	}}, map[string]bool{
		sha: true,
	})
	if strings.Contains(body.String(), "<script>") || !strings.Contains(body.String(), "&lt;script&gt;") || !strings.Contains(body.String(), `value="`+sha+`" checked`) {
		t.Fatalf("unsafe or incorrect picker: %s", body.String())
	}
}

func TestSelectedChanges(t *testing.T) {
	commits := []branchCommit{
		{SHA: "abc"},
		{SHA: "def"},
	}
	for _, tc := range []struct {
		query string
		want  map[string]bool
	}{
		{"", map[string]bool{"abc": true, "def": true, "working": true}},
		{"changes=", map[string]bool{}},
		{"changes=abc,working", map[string]bool{"abc": true, "working": true}},
		{"mode=latest", map[string]bool{"abc": true, "def": true, "working": true}},
		{"mode=branch", map[string]bool{"abc": true, "def": true, "working": true}},
		{"mode=branch&changes=def", map[string]bool{"def": true}},
	} {
		q, _ := url.ParseQuery(tc.query)
		got, err := selectedChanges(q, commits)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: %v, %v", tc.query, got, err)
		}
	}
	q, _ := url.ParseQuery("changes=HEAD")
	if _, err := selectedChanges(q, commits); err == nil {
		t.Fatal("accepted an arbitrary ref")
	}
}

func TestCreateFromWorktree(t *testing.T) {
	oldRoot, oldWT, oldStats := rootDir, worktreesRoot, statsManager
	t.Cleanup(func() { rootDir, worktreesRoot, statsManager = oldRoot, oldWT, oldStats })
	rootDir = t.TempDir()
	worktreesRoot = filepath.Join(rootDir, "wt")
	statsManager = nil
	repo := filepath.Join(rootDir, "repo")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	selectionGit(t, repo, "init", "-q", "-b", "main")
	selectionGit(t, repo, "config", "user.name", "Test")
	selectionGit(t, repo, "config", "user.email", "test@example.com")
	selectionGit(t, repo, "commit", "--allow-empty", "-qm", "initial")
	main := selectionGit(t, repo, "rev-parse", "HEAD")
	source := filepath.Join(worktreesRoot, "repo", "source")
	selectionGit(t, repo, "worktree", "add", "-b", "source", source)
	selectionGit(t, source, "commit", "--allow-empty", "-qm", "source commit")
	head := selectionGit(t, source, "rev-parse", "HEAD")
	for _, tc := range []struct {
		branch   string
		fromMain bool
		want     string
	}{
		{"child", false, head},
		{"from-main", true, main},
	} {
		body, _ := json.Marshal(map[string]any{"kind": "worktrees", "repo": "repo-source", "branch": tc.branch, "fromMain": tc.fromMain})
		w := httptest.NewRecorder()
		handleWorktreeCreate(w, httptest.NewRequest(http.MethodPost, "/api/worktrees/create", strings.NewReader(string(body))))
		if w.Code != http.StatusOK {
			t.Fatalf("create: %d: %s", w.Code, w.Body.String())
		}
		var response map[string]string
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		wantPath := filepath.Join(worktreesRoot, "repo", tc.branch)
		if response["path"] != wantPath || response["url"] != "/worktrees/repo-"+tc.branch+"/" {
			t.Fatalf("wrong identity: %v", response)
		}
		if got := selectionGit(t, wantPath, "rev-parse", "HEAD"); got != tc.want {
			t.Errorf("created HEAD = %s, want %s", got, tc.want)
		}
	}
}
