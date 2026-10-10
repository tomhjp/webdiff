package repostats

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
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
}
