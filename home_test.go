package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tomhjp/webdiff/internal/repostats"
)

func TestWriteDirGroupNestsSessions(t *testing.T) {
	var b strings.Builder
	target := repostats.Target{
		Kind:   repostats.Worktree,
		Name:   "corp-foo",
		Path:   "/tmp/wt/corp/foo",
		Branch: "tomhjp/foo",
	}
	writeDirGroup(&b, target, "corp", []homeSession{
		{
			Session: "corp-foo-wd",
			Label:   "corp-foo-wd:0.0",
			Href:    "/pane?paneID=%251",
			Desc:    "pi · working",
			When:    "now",
		},
		{
			Session: "corp-foo-wd",
			Label:   "corp-foo-wd",
			Href:    "/history/1-corp-foo-wd",
			Desc:    "stopped",
			Branch:  "tomhjp/foo",
			When:    "2h",
			Stopped: true,
		},
	})
	got := b.String()
	for _, want := range []string{
		`<div class="dir-group"><div class="dir-row-wrap">`,
		`<a class="dir-row tmux-row session-row" href="/pane?paneID=%251" title="corp-foo-wd:0.0" data-session="corp-foo-wd"><span class="dir-name">pi · working</span><span class="dir-branch">now</span></a>`,
		`<a class="dir-row tmux-row session-row session-stopped" href="/history/1-corp-foo-wd" title="corp-foo-wd"><span class="dir-name">stopped</span><span class="dir-branch">2h</span></a></div>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("writeDirGroup output does not contain\n%s\ngot:\n%s", want, got)
		}
	}
}

func TestWriteSessionRowOther(t *testing.T) {
	var b strings.Builder
	writeSessionRow(&b, homeSession{
		Session: "scratch-wd",
		Label:   "scratch-wd",
		Href:    "/history/1-scratch-wd",
		Desc:    "stopped",
		Branch:  "main",
		When:    "1d",
		Stopped: true,
	}, false)
	want := `<a class="dir-row tmux-row session-stopped" href="/history/1-scratch-wd" title="scratch-wd"><span class="dir-name">scratch-wd</span><span class="dir-stats">stopped · main</span><span class="dir-branch">1d</span></a>`
	if got := b.String(); got != want {
		t.Errorf("writeSessionRow =\n%s\nwant:\n%s", got, want)
	}
}

func TestWriteRepoActionsGroup(t *testing.T) {
	target := repostats.Target{
		Kind:   repostats.Repo,
		Name:   "corp",
		Path:   "/tmp/corp",
		Branch: "feature",
		AllChanges: repostats.Summary{
			Files: 3,
			Ins:   10,
		},
	}
	live := homeSession{
		Session: "corp-wd",
		Label:   "corp-wd:0.0",
		Href:    "/pane?paneID=%251",
		Desc:    "pi · working",
		When:    "now",
	}
	stopped := homeSession{
		Session: "corp-wd",
		Label:   "corp-wd",
		Href:    "/history/1-corp-wd",
		Desc:    "stopped",
		When:    "2h",
		Stopped: true,
	}
	tests := []struct {
		name     string
		sessions []homeSession
	}{
		{
			name: "no sessions",
		},
		{
			name:     "stopped session",
			sessions: []homeSession{stopped},
		},
		{
			name:     "live session",
			sessions: []homeSession{live},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var b strings.Builder
			writeRepoActionsGroup(&b, target, tt.sessions)
			got := b.String()
			for _, want := range []string{
				`data-wt-form="wt-create-repos-corp"`,
				`id="wt-create-repos-corp" data-repo="corp" data-kind="repos" data-open="agent" hidden>`,
			} {
				if !strings.Contains(got, want) {
					t.Errorf("output does not contain %s:\n%s", want, got)
				}
			}
			// "+ agent" always shows: with a live session, it starts
			// another one.
			if !strings.Contains(got, `href="/agent/repos/corp/?new=1"`) {
				t.Errorf("+ agent button not shown:\n%s", got)
			}
			if n := strings.Count(got, "session-row"); n != len(tt.sessions) {
				t.Errorf("got %d session rows, want %d:\n%s", n, len(tt.sessions), got)
			}
			for _, unwanted := range []string{"feature", "stats-files", "stats-ins"} {
				if strings.Contains(got, unwanted) {
					t.Errorf("output contains %q:\n%s", unwanted, got)
				}
			}
		})
	}
}

func TestSortByAgentActivity(t *testing.T) {
	created := func(sec int64) time.Time { return time.Unix(sec, 0) }
	targets := []repostats.Target{
		{Name: "a-none", Path: "/tmp/wt/a/none", Created: created(10)},
		{Name: "b-old", Path: "/tmp/wt/b/old"},
		{Name: "c-new", Path: "/tmp/wt/c/new"},
		{Name: "d-old", Path: "/tmp/wt/d/old"},
		// The targets with no session show the most recently
		// created first.
		{Name: "e-none-newest", Path: "/tmp/wt/e/none", Created: created(30)},
		{Name: "f-none-newer", Path: "/tmp/wt/f/none", Created: created(20)},
	}
	session := func(name string) string {
		return repoSessionName(filepath.Join("/tmp/wt", name))
	}
	bySession := map[string][]homeSession{
		session("b/old"): {
			{LastUsed: 100},
		},
		// A stopped session that is newer than the live one counts.
		session("c/new"): {
			{LastUsed: 50},
			{LastUsed: 300, Stopped: true},
		},
		session("d/old"): {
			{LastUsed: 100},
		},
	}
	sortByAgentActivity(targets, bySession)
	var got []string
	for _, target := range targets {
		got = append(got, target.Name)
	}
	want := []string{"c-new", "b-old", "d-old", "e-none-newest", "f-none-newer", "a-none"}
	if !slices.Equal(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

func TestSessionLabel(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip(err)
	}
	tests := []struct {
		agent, dir string
		n          int
		want       string
	}{
		{"pi", filepath.Join(home, "ai/wt/corp/safer-ciguestlet-shutdown"), 1, "pi · ~/ai/wt/corp/safer-ciguestlet-shutdown"},
		{"claude", "/srv/repo", 1, "claude · /srv/repo"},
		{"", filepath.Join(home, "ai/webdiff"), 1, "~/ai/webdiff"},
		{"pi", "", 1, "pi"},
		{"pi", "/srv/repo", 2, "pi #2 · /srv/repo"},
		{"", "/srv/repo", 3, "#3 · /srv/repo"},
	}
	for _, tt := range tests {
		if got := sessionLabel(tt.agent, tt.dir, tt.n); got != tt.want {
			t.Errorf("sessionLabel(%q, %q, %d) = %q, want %q", tt.agent, tt.dir, tt.n, got, tt.want)
		}
	}
}

func TestCheckoutSessionRows(t *testing.T) {
	rows := checkoutSessionRows([]homeSession{
		{
			Session: "corp-wd3",
			Desc:    "stopped",
			Stopped: true,
		},
		{
			Session: "corp-wd2",
			Agent:   "claude",
		},
		{
			Session: "corp-wd",
			Desc:    "stopped",
			Stopped: true,
		},
		{
			Session: "corp-wd",
			Agent:   "pi",
		},
	}, "/srv/corp")
	var got []string
	for _, r := range rows {
		got = append(got, r.Desc)
	}
	want := []string{"pi · /srv/corp", "claude #2 · /srv/corp", "stopped", "stopped #3"}
	if !slices.Equal(got, want) {
		t.Errorf("rows = %q, want %q", got, want)
	}
}

func TestGroupByBranch(t *testing.T) {
	old := defaultBranchPrefix
	defaultBranchPrefix = "tomhjp/"
	defer func() { defaultBranchPrefix = old }()
	targets := []repostats.Target{
		{Name: "corp-foo", Path: "/tmp/wt/corp/foo", Branch: "tomhjp/foo"},
		{Name: "corp-bar", Path: "/tmp/wt/corp/bar", Branch: "tomhjp/bar"},
		{Name: "tailscale-foo", Path: "/tmp/wt/tailscale/foo", Branch: "tomhjp/foo"},
		{Name: "corp-old", Path: "/tmp/wt/corp/old", Broken: true},
		{Name: "tailscale-old", Path: "/tmp/wt/tailscale/old", Broken: true},
		{Name: "corp-det", Path: "/tmp/wt/corp/det", Branch: "HEAD"},
	}
	var got []string
	for _, g := range groupByBranch(targets) {
		var names []string
		for _, target := range g.Worktrees {
			names = append(names, target.Name)
		}
		got = append(got, g.Title+": "+strings.Join(names, " "))
	}
	want := []string{
		"foo: corp-foo tailscale-foo",
		"bar: corp-bar",
		"old: corp-old",
		"old: tailscale-old",
		"det: corp-det",
	}
	if !slices.Equal(got, want) {
		t.Errorf("groups =\n%q\nwant\n%q", got, want)
	}
}

func TestWriteBranchGroup(t *testing.T) {
	g := branchGroup{
		Title: "foo",
		Worktrees: []repostats.Target{
			{
				Kind:       repostats.Worktree,
				Name:       "corp-foo",
				Path:       "/tmp/wt/corp/foo",
				ParentRepo: "corp",
				Branch:     "tomhjp/foo",
				Commits:    3,
				AllChanges: repostats.Summary{
					Files: 2,
					Ins:   5,
				},
				Uncommitted: repostats.Summary{
					Files: 1,
					Ins:   2,
				},
			},
			{
				Kind:       repostats.Worktree,
				Name:       "tailscale-foo",
				Path:       "/tmp/wt/tailscale/foo",
				ParentRepo: "tailscale",
				Branch:     "tomhjp/foo",
				Commits:    1,
			},
		},
	}
	sessions := func(target repostats.Target) []homeSession {
		if target.Name != "corp-foo" {
			return nil
		}
		return []homeSession{
			{
				Session: "corp-foo-wd",
				Label:   "corp-foo-wd:0.0",
				Href:    "/pane?paneID=%251",
				Desc:    "pi · working",
				When:    "now",
			},
		}
	}
	var b strings.Builder
	writeBranchGroup(&b, g, sessions)
	got := b.String()
	for _, want := range []string{
		`<div class="dir-group branch-group"><div class="branch-title"><span class="branch-title-name">foo</span></div>`,
		`<span class="title-short">corp</span>`,
		`<div class="dir-row repo-row"><a class="dir-link" href="/worktrees/corp-foo/"><span class="dir-name">`,
		`<button class="dir-commits commit-toggle" type="button" aria-expanded="false" aria-controls="commits-worktrees-corp-foo" title="show or hide the commits">3 commits</button><span class="dir-dirty" title="uncommitted changes" aria-label="uncommitted changes">●</span>`,
		`<span class="dir-stats"><span class="stats-files">2 files</span> <span class="stats-ins" data-stat="+5"></span></span></a>`,
		`<div class="commit-list" id="commits-worktrees-corp-foo" hidden>`,
		`<span class="title-short">tailscale</span>`,
		`<button class="dir-commits commit-toggle" type="button" aria-expanded="false" aria-controls="commits-worktrees-tailscale-foo" title="show or hide the commits">1 commit</button><span class="dir-dirty"></span>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output does not contain\n%s\ngot:\n%s", want, got)
		}
	}
	if strings.Contains(got, "dir-branch\">tomhjp/foo") || strings.Contains(got, "dir-branch\">foo") {
		t.Errorf("repo row shows the branch:\n%s", got)
	}
	// A worktree with no commits and no changes has a plain count.
	var clean strings.Builder
	writeWorktreeRow(&clean, repostats.Target{
		Kind: repostats.Worktree,
		Name: "corp-bar",
		Path: "/tmp/wt/corp/bar",
	}, "corp")
	if got, want := clean.String(), `<span class="dir-commits">0 commits</span><span class="dir-dirty"></span>`; !strings.Contains(got, want) || strings.Contains(got, "commit-toggle") {
		t.Errorf("clean worktree row does not contain %s, or has a toggle:\n%s", want, got)
	}
	// The session row of corp comes before the tailscale row.
	if i, j := strings.Index(got, "session-row"), strings.Index(got, "tailscale"); i < 0 || i > j {
		t.Errorf("session row is not below its repo row:\n%s", got)
	}
}

func TestWriteCommitList(t *testing.T) {
	target := repostats.Target{
		Kind:    repostats.Worktree,
		Name:    "corp-foo",
		Path:    "/tmp/wt/corp/foo",
		Commits: 3,
		CommitList: []repostats.Commit{
			{
				SHA:     "1111111111111111111111111111111111111111",
				Subject: "first <change>",
				Summary: repostats.Summary{
					Files: 1,
					Ins:   2,
					Del:   1,
				},
			},
			{
				SHA:     "2222222222222222222222222222222222222222",
				Subject: "second",
			},
		},
		Uncommitted: repostats.Summary{
			Files: 1,
			Del:   4,
		},
	}
	var b strings.Builder
	writeCommitList(&b, target)
	got := b.String()
	want := `<div class="commit-list" id="commits-worktrees-corp-foo" hidden>` +
		`<div class="dir-row commit-row commit-more"><span class="dir-name">… 1 earlier commit</span></div>` +
		`<a class="dir-row commit-row" href="/worktrees/corp-foo/?commit=1111111111111111111111111111111111111111" title="view commit 1111111111111111111111111111111111111111"><span class="commit-row-sha">1111111</span><span class="dir-name">first &lt;change&gt;</span><span class="dir-stats"><span class="stats-ins" data-stat="+2"></span> <span class="stats-del" data-stat="-1"></span></span></a>` +
		`<a class="dir-row commit-row" href="/worktrees/corp-foo/?commit=2222222222222222222222222222222222222222" title="view commit 2222222222222222222222222222222222222222"><span class="commit-row-sha">2222222</span><span class="dir-name">second</span><span class="dir-stats"></span></a>` +
		`<div class="dir-row commit-row commit-uncommitted"><span class="commit-row-sha">●</span><span class="dir-name">uncommitted</span><span class="dir-stats"><span class="stats-del" data-stat="-4"></span></span></div>` +
		`</div>`
	if got != want {
		t.Errorf("writeCommitList =\n%s\nwant:\n%s", got, want)
	}

	// A clean worktree with no commits has no list.
	b.Reset()
	writeCommitList(&b, repostats.Target{
		Kind: repostats.Worktree,
		Name: "corp-bar",
	})
	if got := b.String(); got != "" {
		t.Errorf("writeCommitList for a clean worktree = %q, want \"\"", got)
	}
}
