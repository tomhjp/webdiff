package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newPullTestRepo returns a clone of a new remote, and a function that
// adds a commit to the remote's main branch and returns its hash.
func newPullTestRepo(t *testing.T) (repo string, push func() string) {
	t.Helper()
	dir := t.TempDir()
	seed := filepath.Join(dir, "seed")
	remote := filepath.Join(dir, "remote.git")
	repo = filepath.Join(dir, "repo")
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	if err := os.Mkdir(seed, 0700); err != nil {
		t.Fatal(err)
	}
	run("-C", seed, "init", "-q", "-b", "main")
	run("-C", seed, "config", "user.name", "Test")
	run("-C", seed, "config", "user.email", "test@example.com")
	run("-C", seed, "commit", "-qm", "first", "--allow-empty")
	run("clone", "-q", "--bare", seed, remote)
	run("-C", seed, "remote", "add", "origin", remote)
	run("clone", "-q", remote, repo)
	return repo, func() string {
		run("-C", seed, "commit", "-qm", "upstream", "--allow-empty")
		run("-C", seed, "push", "-q", "origin", "main")
		return run("-C", seed, "rev-parse", "HEAD")
	}
}

func gitHead(t *testing.T, repo string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func TestPullRepoFastForwards(t *testing.T) {
	repo, push := newPullTestRepo(t)
	upstream := push()
	if err := pullRepo(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	if got := gitHead(t, repo); got != upstream {
		t.Errorf("HEAD = %s, want %s", got, upstream)
	}
}

func TestPullRepoNoUpstream(t *testing.T) {
	repo := t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init", "-q", "-b", "main").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	if err := pullRepo(context.Background(), repo); err != errNoUpstream {
		t.Errorf("pullRepo = %v, want errNoUpstream", err)
	}
}

func TestPullDueReposOncePerInterval(t *testing.T) {
	repo, push := newPullTestRepo(t)
	statePath := filepath.Join(t.TempDir(), "pull.json")
	repos := map[string]string{
		"repo": repo,
	}
	now := time.Unix(1_000_000, 0)

	first := push()
	pullDueRepos(context.Background(), statePath, repos, now)
	if got := gitHead(t, repo); got != first {
		t.Fatalf("after first pull HEAD = %s, want %s", got, first)
	}

	// Not yet due: HEAD stays at the first pull.
	push()
	pullDueRepos(context.Background(), statePath, repos, now.Add(repoPullInterval-time.Minute))
	if got := gitHead(t, repo); got != first {
		t.Errorf("pull before interval: HEAD = %s, want %s", got, first)
	}

	// Due again.
	pullDueRepos(context.Background(), statePath, repos, now.Add(repoPullInterval))
	if got := gitHead(t, repo); got == first {
		t.Errorf("pull after interval did not move HEAD")
	}
	if got := loadPullState(statePath)["repo"]; got != now.Add(repoPullInterval).Unix() {
		t.Errorf("state = %d, want %d", got, now.Add(repoPullInterval).Unix())
	}
}
