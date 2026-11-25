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
		env, base, working, cleanup, err := selectionIndex(context.Background(), repo, commits, selected)
		if err != nil {
			t.Fatalf("mask %d: %v", mask, err)
		}
		if !working.Changed {
			t.Errorf("mask %d: working tree has changes, but working = false", mask)
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
		env, base, _, cleanup, err := selectionIndex(context.Background(), repo, commits, selected)
		if err != nil {
			t.Fatal(err)
		}
		if got := changedFiles(env, repo, base); !reflect.DeepEqual(got, []string{"file"}) {
			t.Errorf("files = %v", got)
		}
		cleanup()
	}
	_, _, _, cleanup, err := selectionIndex(context.Background(), repo, commits, map[string]bool{
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
	env, base, _, cleanup, err := selectionIndex(context.Background(), repo, commits, map[string]bool{
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
	}, workingTree{
		Changed: true,
	}, false, "/repos/r/", 1, "")
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
	// The commit view selects one commit, also with a changes
	// parameter.
	for _, query := range []string{"commit=def", "commit=def&changes=abc"} {
		q, _ := url.ParseQuery(query)
		got, err := selectedChanges(q, commits)
		if want := map[string]bool{"def": true}; err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%s: %v, %v", query, got, err)
		}
	}
	for _, query := range []string{"changes=HEAD", "commit=HEAD", "commit=working", "commit="} {
		q, _ := url.ParseQuery(query)
		if _, err := selectedChanges(q, commits); err == nil {
			t.Errorf("%s: accepted an arbitrary ref", query)
		}
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

// selectionIndex reports if the working tree has changes, both when the
// working tree is selected and when it is not.
func TestSelectionWorkingChanged(t *testing.T) {
	repo := selectionRepo(t)
	selectionWrite(t, repo, "a", "a\n")
	selectionGit(t, repo, "add", "a")
	selectionGit(t, repo, "commit", "-qm", "a")
	oldCache := indexCache
	indexCache = gitindex.New(filepath.Join(t.TempDir(), "indexes"))
	t.Cleanup(func() { indexCache = oldCache })
	commits, err := branchCommits(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	check := func(want bool) {
		t.Helper()
		for _, withWorking := range []bool{false, true} {
			selected := map[string]bool{
				commits[0].SHA: true,
			}
			if withWorking {
				selected["working"] = true
			}
			_, _, working, cleanup, err := selectionIndex(context.Background(), repo, commits, selected)
			cleanup()
			if err != nil {
				t.Fatal(err)
			}
			if working.Changed != want {
				t.Errorf("selected working %v: working = %v, want %v", withWorking, working, want)
			}
		}
	}
	check(false)
	selectionWrite(t, repo, "untracked", "new\n")
	check(true)
}

// A clean working tree is disabled in the picker and is not counted.
func TestDiffPickerCleanWorkingTree(t *testing.T) {
	sha := strings.Repeat("a", 40)
	commits := []branchCommit{{
		SHA:     sha,
		Subject: "change",
	}}
	selected := map[string]bool{
		sha:       true,
		"working": true,
	}
	for _, tc := range []struct {
		working bool
		count   string
		input   string
	}{
		{true, "changes (2)", `value="working" checked>`},
		{false, "changes (1)", `value="working" disabled>`},
	} {
		var body strings.Builder
		writeDiffPicker(&body, commits, selected, workingTree{
			Changed: tc.working,
		}, false, "/repos/r/", 1, "")
		html := body.String()
		if !strings.Contains(html, tc.count) || !strings.Contains(html, tc.input) {
			t.Errorf("working %v: picker = %s", tc.working, html)
		}
	}
}

// branchCommits and selectionIndex count the lines that each change adds
// and removes, and the picker shows the counts.
func TestChangeLineStats(t *testing.T) {
	repo := selectionRepo(t)
	selectionWrite(t, repo, "a", "1\n2\n3\n")
	selectionGit(t, repo, "add", "a")
	selectionGit(t, repo, "commit", "-qm", "add a")
	selectionWrite(t, repo, "a", "1\nx\n")
	selectionWrite(t, repo, "bin", "\x00\x01")
	selectionGit(t, repo, "add", "a", "bin")
	selectionGit(t, repo, "commit", "-qm", "edit a")
	// A merge counts the lines from its first parent.
	selectionGit(t, repo, "checkout", "-qb", "side", "main")
	selectionWrite(t, repo, "side", "s\n")
	selectionGit(t, repo, "add", "side")
	selectionGit(t, repo, "commit", "-qm", "side")
	selectionGit(t, repo, "checkout", "-q", "feature")
	selectionGit(t, repo, "merge", "-q", "--no-ff", "-m", "merge side", "side")
	selectionWrite(t, repo, "untracked", "u\nv\n")
	selectionWrite(t, repo, "a", "1\n")

	oldCache := indexCache
	indexCache = gitindex.New(filepath.Join(t.TempDir(), "indexes"))
	t.Cleanup(func() { indexCache = oldCache })
	commits, err := branchCommits(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	var got []lineStat
	for _, commit := range commits {
		got = append(got, commit.lineStat)
	}
	want := []lineStat{
		{Ins: 3, Del: 0},
		{Ins: 1, Del: 2},
		{Ins: 1, Del: 0},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("commit stats = %v, want %v", got, want)
	}
	for _, withWorking := range []bool{false, true} {
		_, _, working, cleanup, err := selectionIndex(context.Background(), repo, commits, map[string]bool{
			"working": withWorking,
		})
		cleanup()
		if err != nil {
			t.Fatal(err)
		}
		wantWorking := workingTree{
			Changed:  true,
			lineStat: lineStat{Ins: 2, Del: 1},
		}
		if working != wantWorking {
			t.Errorf("selected working %v: working = %+v, want %+v", withWorking, working, wantWorking)
		}

		var body strings.Builder
		writeDiffPicker(&body, commits, allChanges(commits), working, false, "/repos/r/", 1, "")
		html := body.String()
		for _, s := range []string{
			`working tree</span><span class="diff-picker-stat"><span class="stats-ins" data-stat="+2"></span><span class="stats-del" data-stat="-1"></span></span>`,
			`add a</a><span class="diff-picker-stat"><span class="stats-ins" data-stat="+3"></span></span>`,
		} {
			if !strings.Contains(html, s) {
				t.Errorf("picker does not contain %s: %s", s, html)
			}
		}
	}
}
