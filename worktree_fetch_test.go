package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestFetchRemoteMainWithExistingBranch(t *testing.T) {
	dir := t.TempDir()
	seed := filepath.Join(dir, "seed")
	remote := filepath.Join(dir, "remote.git")
	repo := filepath.Join(dir, "repo")
	git := func(path string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", path}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git -C %s %s: %v: %s", path, strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	if err := os.Mkdir(seed, 0700); err != nil {
		t.Fatal(err)
	}
	git(seed, "init", "-q", "-b", "main")
	git(seed, "config", "user.name", "Test")
	git(seed, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(seed, "file"), []byte("first\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git(seed, "add", "file")
	git(seed, "commit", "-qm", "first")
	first := git(seed, "rev-parse", "HEAD")
	cmd := exec.Command("git", "clone", "-q", "--bare", seed, remote)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone bare: %v: %s", err, out)
	}
	cmd = exec.Command("git", "clone", "-q", remote, repo)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone: %v: %s", err, out)
	}
	git(repo, "config", "user.name", "Test")
	git(repo, "config", "user.email", "test@example.com")
	git(repo, "checkout", "-qb", "feature")
	git(repo, "commit", "-qm", "feature", "--allow-empty")
	feature := git(repo, "rev-parse", "HEAD")

	git(seed, "remote", "add", "origin", remote)
	git(seed, "commit", "-qm", "upstream", "--allow-empty")
	git(seed, "push", "-q", "origin", "main")
	upstream := git(seed, "rev-parse", "HEAD")
	if err := fetchRemoteMain(repo, "origin"); err != nil {
		t.Fatal(err)
	}
	if got := git(repo, "rev-parse", "origin/main"); got != upstream {
		t.Errorf("origin/main = %s, want %s", got, upstream)
	}
	if got := git(repo, "rev-parse", "main"); got != first {
		t.Errorf("local main changed: %s", got)
	}
	if got := git(repo, "rev-parse", "feature"); got != feature {
		t.Errorf("feature changed: %s", got)
	}

	git(seed, "reset", "--hard", first)
	git(seed, "commit", "-qm", "rewrite", "--allow-empty")
	git(seed, "push", "-q", "--force", "origin", "main")
	if err := fetchRemoteMain(repo, "origin"); err == nil {
		t.Fatal("fetch accepted a non-fast-forward update")
	}
	if got := git(repo, "rev-parse", "origin/main"); got != upstream {
		t.Errorf("origin/main changed after rejected fetch: %s", got)
	}
}
