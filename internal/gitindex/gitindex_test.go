package gitindex

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func TestAcquireTracksWorkingTreeWithoutChangingRealIndex(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "init", "-q")
	git(t, repo, "config", "user.email", "test@example.com")
	git(t, repo, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("one\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "tracked.txt")
	git(t, repo, "commit", "-qm", "initial")
	realIndexBefore, err := os.ReadFile(filepath.Join(repo, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("one\ntwo\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "new.txt"), []byte("new\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cache := New(filepath.Join(t.TempDir(), "indexes"))
	env, release, err := cache.Acquire(context.Background(), repo, "repos-repo")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "diff", "--cached", "--name-only", "HEAD")
	cmd.Dir = repo
	cmd.Env = env
	out, err := cmd.Output()
	release()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(out), "new.txt\ntracked.txt\n"; got != want {
		t.Fatalf("changed files = %q, want %q", got, want)
	}
	realIndexAfter, err := os.ReadFile(filepath.Join(repo, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	if string(realIndexBefore) != string(realIndexAfter) {
		t.Fatal("Acquire changed the checkout's real index")
	}
	if got := git(t, repo, "diff", "--cached", "--name-only"); got != "" {
		t.Fatalf("real index has staged changes: %q", got)
	}
}
