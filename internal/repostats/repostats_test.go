package repostats

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/tomhjp/webdiff/internal/gitindex"
)

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestManagerIsolatesQueryFailures(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"healthy", "invalid"} {
		repo := filepath.Join(root, name)
		if err := os.Mkdir(repo, 0700); err != nil {
			t.Fatal(err)
		}
		runGit(t, repo, "init", "-q")
		if name == "healthy" {
			runGit(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-qm", "initial")
		}
	}
	m, err := New(Config{
		RootDir:       root,
		WorktreesRoot: filepath.Join(root, "wt"),
	}, gitindex.New(filepath.Join(t.TempDir(), "indexes")))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatalf("one invalid repo blocked startup: %v", err)
	}
	got, err := m.Current(ctx)
	if err != nil {
		t.Fatalf("one invalid repo blocked the snapshot: %v", err)
	}
	if len(got) != 2 || got[0].Name != "healthy" || got[0].Error != "" || got[0].Branch == "" || got[1].Name != "invalid" || got[1].Error == "" {
		t.Fatalf("snapshot did not isolate the query failure: %+v", got)
	}
}

func TestManagerMaintainsFreshStats(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "init", "-q")
	runGit(t, repo, "config", "user.email", "test@example.com")
	runGit(t, repo, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("one\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "file.txt")
	runGit(t, repo, "commit", "-qm", "initial")
	runGit(t, repo, "branch", "-M", "main")
	wtRoot := filepath.Join(root, "wt")
	if err := os.Mkdir(wtRoot, 0700); err != nil {
		t.Fatal(err)
	}
	m, err := New(Config{RootDir: root, WorktreesRoot: wtRoot, Debounce: 20 * time.Millisecond, MaxDebounce: 100 * time.Millisecond, Workers: 1, Reconcile: time.Hour, FullRefresh: time.Hour}, gitindex.New(filepath.Join(t.TempDir(), "indexes")))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	got, err := m.Current(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].AllChanges.Files != 0 {
		t.Fatalf("initial snapshot: %+v", got)
	}

	if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("one\ntwo\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// Give fsnotify time to deliver the event; Current then flushes the
	// debounce rather than waiting for its normal quiet period.
	time.Sleep(20 * time.Millisecond)
	got, err = m.Current(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].AllChanges.Files != 1 || got[0].AllChanges.Ins != 1 {
		t.Fatalf("updated snapshot: %+v", got[0].AllChanges)
	}
}

func TestAllChangesSummaryIncludesBranchAndWorkingTree(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init", "-q", "-b", "main")
	runGit(t, repo, "config", "user.email", "test@example.com")
	runGit(t, repo, "config", "user.name", "Test")
	runGit(t, repo, "commit", "--allow-empty", "-qm", "initial")
	runGit(t, repo, "checkout", "-qb", "feature")
	for _, name := range []string{"earlier", "latest"} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte("one\n"), 0600); err != nil {
			t.Fatal(err)
		}
		runGit(t, repo, "add", name)
		runGit(t, repo, "commit", "-qm", name)
	}
	if err := os.WriteFile(filepath.Join(repo, "working"), []byte("one\ntwo\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "staged"), []byte("staged\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "staged")
	if err := os.WriteFile(filepath.Join(repo, "earlier"), []byte("one\nunstaged\n"), 0600); err != nil {
		t.Fatal(err)
	}
	m, err := New(Config{}, gitindex.New(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	got, err := m.calculate(context.Background(), Target{
		Kind: Repo,
		Name: "repo",
		Path: repo,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := Summary{
		Files: 4,
		Ins:   6,
	}
	if got.AllChanges != want {
		t.Fatalf("all changes = %+v, want %+v", got.AllChanges, want)
	}
	if got.Commits != 2 || !got.Dirty() {
		t.Fatalf("commits = %d, dirty = %v; want 2, true", got.Commits, got.Dirty())
	}
	wantList := []Commit{
		{
			SHA:     got.CommitList[0].SHA,
			Subject: "earlier",
			Summary: Summary{
				Files: 1,
				Ins:   1,
			},
		},
		{
			SHA:     got.CommitList[1].SHA,
			Subject: "latest",
			Summary: Summary{
				Files: 1,
				Ins:   1,
			},
		},
	}
	if !slices.Equal(got.CommitList, wantList) {
		t.Fatalf("commit list = %+v, want %+v", got.CommitList, wantList)
	}
	// "working" is new, "staged" is new, and "earlier" has one more line.
	wantUncommitted := Summary{
		Files: 3,
		Ins:   4,
	}
	if got.Uncommitted != wantUncommitted {
		t.Fatalf("uncommitted = %+v, want %+v", got.Uncommitted, wantUncommitted)
	}
}

// A repo with "master" and no "main" uses "master" as the diff base.
func TestAllChangesSummaryUsesMaster(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init", "-q", "-b", "master")
	runGit(t, repo, "config", "user.email", "test@example.com")
	runGit(t, repo, "config", "user.name", "Test")
	runGit(t, repo, "commit", "--allow-empty", "-qm", "initial")
	runGit(t, repo, "checkout", "-qb", "feature")
	if err := os.WriteFile(filepath.Join(repo, "file"), []byte("one\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "file")
	runGit(t, repo, "commit", "-qm", "file")
	m, err := New(Config{}, gitindex.New(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	got, err := m.calculate(context.Background(), Target{
		Kind: Repo,
		Name: "repo",
		Path: repo,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := Summary{
		Files: 1,
		Ins:   1,
	}
	if got.AllChanges != want {
		t.Fatalf("all changes = %+v, want %+v", got.AllChanges, want)
	}
	if got.Commits != 1 || got.Dirty() {
		t.Fatalf("commits = %d, dirty = %v; want 1, false", got.Commits, got.Dirty())
	}
}

// An untracked file makes the checkout dirty. An ignored file does not.
func TestDirtyIncludesUntrackedFiles(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init", "-q", "-b", "main")
	runGit(t, repo, "config", "user.email", "test@example.com")
	runGit(t, repo, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("ignored\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", ".gitignore")
	runGit(t, repo, "commit", "-qm", "initial")
	m, err := New(Config{}, gitindex.New(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	target := Target{
		Kind: Repo,
		Name: "repo",
		Path: repo,
	}
	for _, step := range []struct {
		file  string
		dirty bool
	}{
		{"", false},
		{"ignored", false},
		{"untracked", true},
	} {
		if step.file != "" {
			if err := os.WriteFile(filepath.Join(repo, step.file), []byte("x\n"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		got, err := m.calculate(context.Background(), target)
		if err != nil {
			t.Fatal(err)
		}
		if got.Dirty() != step.dirty || got.Commits != 0 {
			t.Errorf("after %q: dirty = %v, commits = %d; want %v, 0", step.file, got.Dirty(), got.Commits, step.dirty)
		}
	}
}

// Watch gives a channel that closes when an edit changes the stats, and
// the next Watch returns the new stats. No Current call is necessary.
func TestManagerWatch(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "init", "-q")
	runGit(t, repo, "config", "user.email", "test@example.com")
	runGit(t, repo, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("one\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "file.txt")
	runGit(t, repo, "commit", "-qm", "initial")
	runGit(t, repo, "branch", "-M", "main")
	m, err := New(Config{
		RootDir:       root,
		WorktreesRoot: filepath.Join(root, "wt"),
		Debounce:      20 * time.Millisecond,
		MaxDebounce:   100 * time.Millisecond,
		Workers:       1,
		Reconcile:     time.Hour,
		FullRefresh:   time.Hour,
	}, gitindex.New(filepath.Join(t.TempDir(), "indexes")))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if _, ok, _ := m.Watch(Repo, "missing"); ok {
		t.Fatal("Watch found a missing target")
	}
	target, ok, changed := m.Watch(Repo, "repo")
	if !ok || target.AllChanges.Files != 0 {
		t.Fatalf("initial target: %+v, %v", target, ok)
	}
	if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("one\ntwo\n"), 0600); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(10 * time.Second)
	for target.AllChanges.Files != 1 {
		select {
		case <-changed:
		case <-deadline:
			t.Fatalf("stats did not change: %+v", target.AllChanges)
		}
		target, _, changed = m.Watch(Repo, "repo")
	}
	if target.AllChanges.Ins != 1 {
		t.Fatalf("updated stats: %+v", target.AllChanges)
	}
}

// CommitList has the newest MaxCommitList commits, the oldest first.
// Commits counts all of them.
func TestCommitListIsLimited(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init", "-q", "-b", "main")
	runGit(t, repo, "config", "user.email", "test@example.com")
	runGit(t, repo, "config", "user.name", "Test")
	runGit(t, repo, "commit", "--allow-empty", "-qm", "initial")
	runGit(t, repo, "checkout", "-qb", "feature")
	n := MaxCommitList + 2
	for i := range n {
		runGit(t, repo, "commit", "--allow-empty", "-qm", fmt.Sprintf("c%d", i))
	}
	m, err := New(Config{}, gitindex.New(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	got, err := m.calculate(context.Background(), Target{
		Kind: Repo,
		Name: "repo",
		Path: repo,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Commits != n || len(got.CommitList) != MaxCommitList {
		t.Fatalf("commits = %d, list length = %d; want %d, %d", got.Commits, len(got.CommitList), n, MaxCommitList)
	}
	if first, last := got.CommitList[0].Subject, got.CommitList[MaxCommitList-1].Subject; first != "c2" || last != fmt.Sprintf("c%d", n-1) {
		t.Fatalf("list from %q to %q, want c2 to c%d", first, last, n-1)
	}
}
