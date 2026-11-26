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

// newGitPRTestRemote makes a GitHub-like remote at <dir>/o/r.git and a
// checkout of it. The remote has these refs:
//
//	main        commit M
//	feat-a      commit A, the head of closed PR 1 and open PR 2
//	(no feat-b) commit B is the head of PR 3; the checkout has feat-b at B
//	feat-c      commit M, which PR 4 has as its head
//	feat-d      commit D, with no PR
func newGitPRTestRemote(t *testing.T) (base, checkout string, run func(dir string, args ...string) string) {
	t.Helper()
	base = t.TempDir()
	checkout = filepath.Join(base, "work")
	remote := filepath.Join(base, "o", "r.git")
	run = func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	if err := os.MkdirAll(checkout, 0700); err != nil {
		t.Fatal(err)
	}
	run(checkout, "init", "-q", "-b", "main")
	run(checkout, "commit", "-qm", "M", "--allow-empty")
	m := run(checkout, "rev-parse", "HEAD")
	commit := func(branch string) string {
		run(checkout, "checkout", "-q", "-b", branch, m)
		run(checkout, "commit", "-qm", branch, "--allow-empty")
		return run(checkout, "rev-parse", "HEAD")
	}
	a := commit("feat-a")
	b := commit("feat-b")
	d := commit("feat-d")
	run(checkout, "branch", "feat-c", m)
	run(checkout, "checkout", "-q", "main")
	run(base, "clone", "-q", "--bare", checkout, remote)
	run(remote, "branch", "-D", "feat-b")
	for ref, sha := range map[string]string{
		"refs/pull/1/head":  a,
		"refs/pull/2/head":  a,
		"refs/pull/2/merge": d, // any commit
		"refs/pull/3/head":  b,
		"refs/pull/4/head":  m,
	} {
		run(remote, "update-ref", ref, sha)
	}
	return base, checkout, run
}

func newGitTestPRTracker(base, checkout string) *prTracker {
	t := newPRTracker(prConfig{
		GitPoll: time.Hour,
	})
	// The first base fails, as public GitHub does for a private repo.
	t.gitBases = []string{
		"file://" + filepath.Join(base, "missing"),
		"file://" + base,
	}
	t.repos[checkout] = "o/r"
	return t
}

func TestPRTrackerGit(t *testing.T) {
	base, checkout, run := newGitPRTestRemote(t)
	tr := newGitTestPRTracker(base, checkout)
	ctx := context.Background()

	queries := []prQuery{
		{Dir: checkout, Branch: "feat-a"},
		{Dir: checkout, Branch: "feat-b"},
		{Dir: checkout, Branch: "feat-c"},
		{Dir: checkout, Branch: "feat-d"},
	}
	got := tr.lookup(ctx, queries)
	if p := got[0]; p == nil || p.Number != 2 || p.State != "OPEN" || !p.ReviewUnknown {
		t.Errorf("feat-a: got %+v, want open PR 2 with no review state", p)
	}
	if p := got[1]; p == nil || p.Number != 3 || p.State != "" {
		t.Errorf("feat-b: got %+v, want PR 3 of the local branch, state not known", p)
	}
	if p := got[2]; p != nil {
		t.Errorf("feat-c: got %+v, want no PR for the commit of main", p)
	}
	if p := got[3]; p != nil {
		t.Errorf("feat-d: got %+v, want no PR", p)
	}
	if u := tr.gitURLs["o/r"]; u != "file://"+base+"/o/r.git" {
		t.Errorf("git URL = %q, want the second base", u)
	}

	// The poll finds a new PR.
	remote := filepath.Join(base, "o", "r.git")
	run(remote, "update-ref", "refs/pull/5/head", run(remote, "rev-parse", "feat-d"))
	changed := tr.changes()
	tr.pollGit(ctx, true)
	select {
	case <-changed:
	default:
		t.Fatal("changes not signalled")
	}
	if p := tr.lookupOne(ctx, checkout, "feat-d"); p == nil || p.Number != 5 {
		t.Errorf("feat-d after poll: got %+v, want PR 5", p)
	}
}

// With maintner, the git poll is only for the repos that maintner does
// not watch.
func TestPRTrackerGitFallback(t *testing.T) {
	base, checkout, _ := newGitPRTestRemote(t)
	tr := newGitTestPRTracker(base, checkout)
	f := &fakeGHAPI{
		prs: map[string][]fakePR{},
	}
	tr.apiURL = "http://maintner.test/api/gh"
	tr.client.Transport = f
	if p := tr.lookupOne(context.Background(), checkout, "feat-a"); p == nil || p.Number != 2 {
		t.Fatalf("got %+v, want PR 2 from git", p)
	}
	if !tr.untracked["o/r"] {
		t.Error("o/r is not marked as not watched by maintner")
	}
	n := f.callCount()
	tr.pollGit(context.Background(), true)
	if got := f.callCount(); got != n {
		t.Errorf("maintner calls = %d after the git poll, want %d", got, n)
	}
}

func TestPRBadgeHTMLReviewUnknown(t *testing.T) {
	got := prBadgeHTML(&pullRequest{
		Repo:          "o/r",
		Number:        7,
		ReviewUnknown: true,
	}, false)
	for _, want := range []string{
		`class="pr-badge pr-unknown"`,
		`title="o/r#7 (review state not known)"`,
		`>○</span>#7</a>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("badge %q does not contain %q", got, want)
		}
	}
}

func TestGitPollDue(t *testing.T) {
	tr := newPRTracker(prConfig{
		GitPoll:     15 * time.Minute,
		GitPollIdle: 24 * time.Hour,
	})
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name                   string
		sincePoll, sinceCommit time.Duration
		want                   bool
	}{
		{"active, polled recently", 5 * time.Minute, time.Hour, false},
		{"active, due", 15 * time.Minute, time.Hour, true},
		{"active, ticker a little early", 14*time.Minute + 30*time.Second, time.Hour, true},
		{"idle, polled 15m ago", 15 * time.Minute, 4 * time.Hour, false},
		{"idle, polled 23h ago", 23 * time.Hour, 4 * time.Hour, false},
		{"idle, due", 24 * time.Hour, 4 * time.Hour, true},
		{"never polled", 0, 4 * time.Hour, true},
	} {
		lastPoll := now.Add(-tc.sincePoll)
		if tc.sincePoll == 0 {
			lastPoll = time.Time{}
		}
		if got := tr.gitPollDue(lastPoll, now.Add(-tc.sinceCommit), now); got != tc.want {
			t.Errorf("%s: due = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The poll asks about a repo with a recent commit each GitPoll, and
// does not ask before that.
func TestPRTrackerGitPollActive(t *testing.T) {
	base, checkout, run := newGitPRTestRemote(t)
	tr := newGitTestPRTracker(base, checkout)
	tr.gitPoll = 15 * time.Minute
	tr.gitPollIdle = 24 * time.Hour
	ctx := context.Background()
	if p := tr.lookupOne(ctx, checkout, "feat-d"); p != nil {
		t.Fatalf("got %+v, want no PR", p)
	}
	remote := filepath.Join(base, "o", "r.git")
	run(remote, "update-ref", "refs/pull/5/head", run(remote, "rev-parse", "feat-d"))

	// The lookup asked about the repo just now.
	tr.pollGit(ctx, false)
	if p := tr.lookupOne(ctx, checkout, "feat-d"); p != nil {
		t.Fatalf("got %+v before the repo is due, want no PR", p)
	}

	// The commits of the test are new, thus the repo is active.
	tr.mu.Lock()
	tr.gitPolled["o/r"] = time.Now().Add(-16 * time.Minute)
	tr.mu.Unlock()
	tr.pollGit(ctx, false)
	if p := tr.lookupOne(ctx, checkout, "feat-d"); p == nil || p.Number != 5 {
		t.Fatalf("got %+v after the repo is due, want PR 5", p)
	}
}
