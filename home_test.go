package main

import (
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
		name      string
		sessions  []homeSession
		wantAgent bool
	}{
		{
			name:      "no sessions",
			wantAgent: true,
		},
		{
			name:      "stopped session",
			sessions:  []homeSession{stopped},
			wantAgent: true,
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
			if hasAgent := strings.Contains(got, `href="/agent/repos/corp/"`); hasAgent != tt.wantAgent {
				t.Errorf("+ agent button shown = %v, want %v:\n%s", hasAgent, tt.wantAgent, got)
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
