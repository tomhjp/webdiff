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
	if len(got) != 1 || got[0].Working.Files != 0 {
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
	if got[0].Working.Files != 1 || got[0].Working.Ins != 1 {
		t.Fatalf("updated snapshot: %+v", got[0].Working)
	}
}
