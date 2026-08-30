package main

import (
	"os"
	"path/filepath"
	"testing"
)

// newWorktreeTestRoots points rootDir at a temp dir with worktreesRoot
// nested inside it, the way main() sets them up.
func newWorktreeTestRoots(t *testing.T) {
	t.Helper()
	rootDir = t.TempDir()
	worktreesRoot = filepath.Join(rootDir, "wt")
	if err := os.MkdirAll(worktreesRoot, 0700); err != nil {
		t.Fatal(err)
	}
}

// mkWorktree creates <worktreesRoot>/<repo>/<leaf> with a .git file, as
// `git worktree add` would.
func mkWorktree(t *testing.T, repo, leaf string) string {
	t.Helper()
	abs := filepath.Join(worktreesRoot, repo, leaf)
	if err := os.MkdirAll(abs, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(abs, ".git"), []byte("gitdir: /nowhere\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return abs
}

func mkRepo(t *testing.T, name string) string {
	t.Helper()
	abs := filepath.Join(rootDir, name)
	if err := os.MkdirAll(filepath.Join(abs, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	return abs
}

func TestWorktreeSlug(t *testing.T) {
	newWorktreeTestRoots(t)

	cases := []struct {
		name string
		abs  string
		want string
	}{
		{"worktree", filepath.Join(worktreesRoot, "corp", "fix-x"), "corp-fix-x"},
		{"plain repo", filepath.Join(rootDir, "webdiff"), "webdiff"},
		{"worktrees root itself", worktreesRoot, "wt"},
		{"grouping dir only", filepath.Join(worktreesRoot, "corp"), "corp"},
		{"too deep", filepath.Join(worktreesRoot, "corp", "fix-x", "cmd"), "cmd"},
		{"trailing slash", filepath.Join(worktreesRoot, "corp", "fix-x") + "/", "corp-fix-x"},
		{"outside both roots", "/etc/passwd", "passwd"},
		{"escapes via ..", filepath.Join(worktreesRoot, "..", "webdiff"), "webdiff"},
		{"hidden leaf", filepath.Join(worktreesRoot, "corp", ".hidden"), ".hidden"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := worktreeSlug(tc.abs); got != tc.want {
				t.Errorf("worktreeSlug(%q) = %q, want %q", tc.abs, got, tc.want)
			}
		})
	}

	t.Run("no worktrees root configured", func(t *testing.T) {
		saved := worktreesRoot
		worktreesRoot = ""
		defer func() { worktreesRoot = saved }()
		if got := worktreeSlug("/a/b/c"); got != "c" {
			t.Errorf("worktreeSlug = %q, want %q", got, "c")
		}
	})
}

func TestManagedWorktreePaths(t *testing.T) {
	newWorktreeTestRoots(t)
	want := []string{
		mkWorktree(t, "corp", "build-x"),
		mkWorktree(t, "corp", "fix-x"),
		mkWorktree(t, "webdiff", "fix-x"),
	}
	// Neither of these is a worktree: one is only a grouping dir, the
	// other is hidden.
	if err := os.MkdirAll(filepath.Join(worktreesRoot, "empty"), 0700); err != nil {
		t.Fatal(err)
	}
	mkWorktree(t, "corp", ".hidden")

	got := managedWorktreePaths()
	if len(got) != len(want) {
		t.Fatalf("managedWorktreePaths() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("managedWorktreePaths()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestResolveRepoRefWorktrees(t *testing.T) {
	newWorktreeTestRoots(t)
	mkRepo(t, "corp")
	wt := mkWorktree(t, "corp", "fix-x")

	ref, ok := resolveRepoRef("/worktrees/corp-fix-x/")
	if !ok {
		t.Fatal("resolveRepoRef(/worktrees/corp-fix-x/) failed")
	}
	if ref.abs != wt {
		t.Errorf("abs = %q, want %q", ref.abs, wt)
	}
	if ref.name != "corp-fix-x" || ref.url != "/worktrees/corp-fix-x/" {
		t.Errorf("name/url = %q/%q", ref.name, ref.url)
	}

	// A worktree whose gitfile is gone must still resolve, or the home
	// page's remove button has nothing to act on.
	if err := os.Remove(filepath.Join(wt, ".git")); err != nil {
		t.Fatal(err)
	}
	if _, ok := resolveRepoRef("/worktrees/corp-fix-x/"); !ok {
		t.Error("broken worktree should still resolve")
	}

	for _, bad := range []string{
		"/worktrees/corp/fix-x/", // nested form isn't the slug
		"/worktrees/corp/",       // grouping dir
		"/worktrees/fix-x/",      // leaf without its repo
		"/worktrees/nope/",
		"/repos/wt/", // worktreesRoot is not a repo
	} {
		if _, ok := resolveRepoRef(bad); ok {
			t.Errorf("resolveRepoRef(%q) resolved, want failure", bad)
		}
	}

	if _, ok := resolveRepoRef("/repos/corp/"); !ok {
		t.Error("resolveRepoRef(/repos/corp/) failed")
	}
}

func TestAllManagedRefs(t *testing.T) {
	newWorktreeTestRoots(t)
	mkRepo(t, "corp")
	// A non-repo dir under rootDir, which allManagedRefs must skip along
	// with the wt dir itself.
	if err := os.MkdirAll(filepath.Join(rootDir, "notarepo"), 0700); err != nil {
		t.Fatal(err)
	}
	mkWorktree(t, "corp", "fix-x")

	got := map[string]string{}
	for _, ref := range allManagedRefs() {
		got[ref.name] = ref.kind
	}
	want := map[string]string{
		"corp":       "repos",
		"corp-fix-x": "worktrees",
	}
	if len(got) != len(want) {
		t.Fatalf("allManagedRefs() = %v, want %v", got, want)
	}
	for name, kind := range want {
		if got[name] != kind {
			t.Errorf("ref %q kind = %q, want %q", name, got[name], kind)
		}
	}
}

func TestWorktreeLeaf(t *testing.T) {
	cases := []struct{ branch, want string }{
		{"tomhjp/fix-x", "fix-x"},
		{"tomhjp/Fix-X", "fix-x"},
		{"gabriel/queue-metrics", "gabriel-queue-metrics"},
		{"main", "main"},
		{"tomhjp/a/b", "a-b"},
		{"tomhjp/", ""},
		{"", ""},
		{"!!!", ""},
		{"tomhjp/.hidden", "hidden"},
		{"feat/v1.2_x", "feat-v1.2_x"},
	}
	for _, tc := range cases {
		if got := worktreeLeaf(tc.branch); got != tc.want {
			t.Errorf("worktreeLeaf(%q) = %q, want %q", tc.branch, got, tc.want)
		}
	}
}

func TestRepoSessionNameUsesSlug(t *testing.T) {
	newWorktreeTestRoots(t)
	// Two worktrees sharing a leaf must not share a session name.
	a := mkWorktree(t, "corp", "fix-x")
	b := mkWorktree(t, "webdiff", "fix-x")
	if got, want := repoSessionName(a), "corp-fix-x-wd"; got != want {
		t.Errorf("repoSessionName(%q) = %q, want %q", a, got, want)
	}
	if got, want := repoSessionName(b), "webdiff-fix-x-wd"; got != want {
		t.Errorf("repoSessionName(%q) = %q, want %q", b, got, want)
	}
	if got, want := repoSessionName(filepath.Join(rootDir, "corp")), "corp-wd"; got != want {
		t.Errorf("repoSessionName(repo) = %q, want %q", got, want)
	}
}
