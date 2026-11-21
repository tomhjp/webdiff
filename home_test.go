package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

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
	targets := []repostats.Target{
		{Name: "a-none", Path: "/tmp/wt/a/none"},
		{Name: "b-old", Path: "/tmp/wt/b/old"},
		{Name: "c-new", Path: "/tmp/wt/c/new"},
		{Name: "d-old", Path: "/tmp/wt/d/old"},
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
	want := []string{"c-new", "b-old", "d-old", "a-none"}
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
