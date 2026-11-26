package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/tomhjp/webdiff/internal/repostats"
)

func TestIssueLabel(t *testing.T) {
	for _, tc := range []struct {
		ref  issueRef
		base string
		want string
	}{
		{issueRef{"tailscale/corp", 1}, "tailscale/corp", "#1"},
		{issueRef{"tailscale/corp", 1}, "tailscale/tb", "corp#1"},
		{issueRef{"tailscale/tailscale", 2}, "tailscale/corp", "oss#2"},
		{issueRef{"tailscale/tailscale", 2}, "tailscale/tailscale", "#2"},
		{issueRef{"tailscale/tb", 3}, "tailscale/corp", "tailscale/tb#3"},
		// A line about more than one repo names the repo of each issue.
		{issueRef{"tailscale/tb", 3}, "", "tailscale/tb#3"},
		{issueRef{"tailscale/corp", 4}, "", "corp#4"},
	} {
		if got := tc.ref.label(tc.base); got != tc.want {
			t.Errorf("%v.label(%q) = %q, want %q", tc.ref, tc.base, got, tc.want)
		}
	}
}

func TestParseIssueList(t *testing.T) {
	got, err := parseIssueList(" #1, 2 corp#3;oss#4 tailscale/tb#5 https://github.com/o/r/issues/6 https://github.com/o/r/pull/7/ #1 ", "tailscale/gotst")
	if err != nil {
		t.Fatal(err)
	}
	want := []issueRef{
		{"tailscale/gotst", 1},
		{"tailscale/gotst", 2},
		{"tailscale/corp", 3},
		{"tailscale/tailscale", 4},
		{"tailscale/tb", 5},
		{"o/r", 6},
		{"o/r", 7},
	}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}

	if got, err := parseIssueList("  ", "o/r"); err != nil || len(got) != 0 {
		t.Errorf("empty list: got %v, %v", got, err)
	}
	for _, bad := range []string{"#x", "foo#1", "a/b/c#1", "#0", "-3", "https://github.com/o/r/commit/1"} {
		if _, err := parseIssueList(bad, "o/r"); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
	// A relative reference needs a repo.
	if _, err := parseIssueList("#1", ""); err == nil {
		t.Error("#1 with no repo: no error")
	}
	if got, err := parseIssueList("corp#1", ""); err != nil || !slices.Equal(got, []issueRef{{"tailscale/corp", 1}}) {
		t.Errorf("corp#1 with no repo: got %v, %v", got, err)
	}
}

func TestCommitIssueRefs(t *testing.T) {
	msg := `cmd/foo: fix the thing (see #99 in the text)

Step #2 of the plan does not name an issue.

Updates #1
Fixes: tailscale/corp#2, #3.
updates tailscale/tailscale#4 and #5
Updates #cleanup
Fixes https://github.com/o/r/issues/6
Updates 7

Signed-off-by: A <a@example.com>`
	got := commitIssueRefs(msg)
	want := []issueRef{
		{"", 1},
		{"tailscale/corp", 2},
		{"", 3},
		{"tailscale/tailscale", 4},
		{"", 5},
		{"o/r", 6},
	}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestTargetCommitIssues(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) string {
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
	run("init", "-q", "-b", "main")
	run("commit", "-qm", "first\n\nUpdates #1\nUpdates tailscale/corp#2", "--allow-empty")
	a := run("rev-parse", "HEAD")
	run("commit", "-qm", "second\n\nFixes #3\nUpdates #1", "--allow-empty")
	b := run("rev-parse", "HEAD")
	target := repostats.Target{
		Path: dir,
		CommitList: []repostats.Commit{
			{SHA: a},
			{SHA: b},
		},
	}
	want := []issueRef{
		{"", 1},
		{"tailscale/corp", 2},
		{"", 3},
	}
	if got := targetCommitIssues(target); !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	// The second call uses the cache, which does not need the repo.
	target.Path = filepath.Join(dir, "missing")
	if got := targetCommitIssues(target); !slices.Equal(got, want) {
		t.Errorf("from the cache: got %v, want %v", got, want)
	}
}

func TestWriteGroupIssues(t *testing.T) {
	write := func(gi groupIssues) string {
		var b strings.Builder
		writeGroupIssues(&b, gi)
		return b.String()
	}

	// No issues: a button to add them.
	got := write(groupIssues{
		Branch: "tomhjp/foo",
		Base:   "tailscale/corp",
	})
	for _, want := range []string{
		`data-branch="tomhjp/foo"`,
		`data-repo="tailscale/corp"`,
		`class="issue-edit issue-add"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("no issues: %s does not contain %s", got, want)
		}
	}

	// Issues from commits only: no edit button.
	got = write(groupIssues{
		Branch: "tomhjp/foo",
		Base:   "tailscale/corp",
		Issues: []issueRef{{"tailscale/corp", 1}, {"tailscale/tailscale", 2}},
	})
	for _, want := range []string{
		`<a class="issue-link" href="https://github.com/tailscale/corp/issues/1" target="_blank" rel="noopener" title="tailscale/corp#1">#1</a>`,
		`>oss#2</a>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("commit issues: %s does not contain %s", got, want)
		}
	}
	if strings.Contains(got, "issue-edit") {
		t.Errorf("commit issues: %s has an edit button", got)
	}

	// Issues that the user wrote: an edit button, with the labels to
	// edit.
	manual := []issueRef{{"tailscale/corp", 1}, {"o/r", 5}}
	got = write(groupIssues{
		Branch: "tomhjp/foo",
		Base:   "tailscale/corp",
		Issues: manual,
		Manual: manual,
	})
	for _, want := range []string{`data-manual="#1 o/r#5"`, `class="issue-edit"`} {
		if !strings.Contains(got, want) {
			t.Errorf("manual issues: %s does not contain %s", got, want)
		}
	}

	// A group with no branch and no issues shows nothing.
	if got := write(groupIssues{}); got != "" {
		t.Errorf("no branch: got %q", got)
	}
}

func TestBranchIssuesAPI(t *testing.T) {
	old := branchIssues
	t.Cleanup(func() { branchIssues = old })
	path := filepath.Join(t.TempDir(), "sub", "branch-issues.json")
	branchIssues = loadBranchIssueStore(path)

	post := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		handleBranchIssues(w, httptest.NewRequest(http.MethodPost, "/api/branch-issues", strings.NewReader(body)))
		return w
	}
	if w := post(`{"branch":"tomhjp/foo","repo":"tailscale/gotst","issues":"#1, corp#2"}`); w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	want := []issueRef{{"tailscale/gotst", 1}, {"tailscale/corp", 2}}
	if got := branchIssues.get("tomhjp/foo"); !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}

	// The file keeps the issues.
	if got := loadBranchIssueStore(path).get("tomhjp/foo"); !slices.Equal(got, want) {
		t.Errorf("after load: got %v, want %v", got, want)
	}

	// A bad list changes nothing.
	if w := post(`{"branch":"tomhjp/foo","repo":"tailscale/gotst","issues":"nope"}`); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "not an issue number") {
		t.Errorf("bad list: status %d: %s", w.Code, w.Body)
	}
	if got := branchIssues.get("tomhjp/foo"); !slices.Equal(got, want) {
		t.Errorf("after bad list: got %v, want %v", got, want)
	}

	// A rename of the branch keeps its issues.
	if err := branchIssues.rename("tomhjp/foo", "tomhjp/bar"); err != nil {
		t.Fatal(err)
	}
	if got := branchIssues.get("tomhjp/bar"); !slices.Equal(got, want) {
		t.Errorf("after rename: got %v, want %v", got, want)
	}

	// An empty list removes the issues.
	if w := post(`{"branch":"tomhjp/bar","repo":"tailscale/gotst","issues":""}`); w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if got := loadBranchIssueStore(path).get("tomhjp/bar"); len(got) != 0 {
		t.Errorf("after clear: got %v", got)
	}
}
