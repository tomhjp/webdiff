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
