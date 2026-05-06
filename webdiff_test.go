package main

import (
	"os/exec"
	"strings"
	"testing"

	terminal "github.com/buildkite/terminal-to-html/v3"
)

// renderDiff pipes a synthetic git diff through the requested pager
// (or none) and runs it through the same terminal-to-html → wrapDiffLines
// pipeline that fileDiff uses, so we can assert on the final HTML.
func renderDiff(t *testing.T, raw string, pagerCmd string) string {
	t.Helper()
	out := raw
	if pagerCmd != "" {
		cmd := exec.Command("sh", "-c", pagerCmd)
		cmd.Stdin = strings.NewReader(raw)
		paged, err := cmd.Output()
		if err != nil {
			t.Fatalf("pager %q: %v", pagerCmd, err)
		}
		out = string(paged)
	}
	screen, _ := terminal.NewScreen(terminal.WithMaxSize(120, 0))
	screen.Write([]byte(out))
	_, rest := extractHeading(screen.AsHTML())
	return wrapDiffLines(rest, "f")
}

const sampleRawDiff = "" +
	"\x1b[1mdiff --git a/f b/f\x1b[m\n" +
	"\x1b[1mindex de98044..7be73ce 100644\x1b[m\n" +
	"\x1b[1m--- a/f\x1b[m\n" +
	"\x1b[1m+++ b/f\x1b[m\n" +
	"\x1b[36m@@ -1,3 +1,3 @@\x1b[m\n" +
	" a\n" +
	"\x1b[31m-b\x1b[m\n" +
	"\x1b[32m+B\x1b[m\n" +
	" c\n"

func TestWrapDiffLines_RawGitDiff(t *testing.T) {
	html := renderDiff(t, sampleRawDiff, "")
	// Comments require data-file on the changed lines.
	if !strings.Contains(html, `class="line line-add" data-file="f"`) {
		t.Fatalf("missing line-add with data-file in raw-diff output:\n%s", html)
	}
	if !strings.Contains(html, `class="line line-del" data-file="f"`) {
		t.Fatalf("missing line-del with data-file in raw-diff output:\n%s", html)
	}
	// File-header lines (+++ / ---) must NOT be classified as diff lines.
	if strings.Count(html, `line-add`) > 1 || strings.Count(html, `line-del`) > 1 {
		t.Fatalf("file-header lines bled into diff classification:\n%s", html)
	}
}

func TestWrapDiffLines_DeltaDefault(t *testing.T) {
	if _, err := exec.LookPath("delta"); err != nil {
		t.Skip("delta not installed")
	}
	html := renderDiff(t, sampleRawDiff, "delta --no-gitconfig --line-numbers --width 120")
	if !strings.Contains(html, `class="line line-add"`) {
		t.Fatalf("default delta: no line-add:\n%s", html)
	}
	if !strings.Contains(html, `class="line line-del"`) {
		t.Fatalf("default delta: no line-del:\n%s", html)
	}
	if !strings.Contains(html, `data-file="f"`) {
		t.Fatalf("default delta: no data-file:\n%s", html)
	}
	// Line numbers should appear via gutter parsing.
	if !strings.Contains(html, `data-new-line=`) || !strings.Contains(html, `data-old-line=`) {
		t.Fatalf("default delta: missing line-number attributes:\n%s", html)
	}
}

func TestWrapDiffLines_DeltaCustomTheme(t *testing.T) {
	if _, err := exec.LookPath("delta"); err != nil {
		t.Skip("delta not installed")
	}
	// Force a non-default palette: bgs in the green/red hues but
	// different cube cells (28 / 88) plus reskinned line-number colours.
	pager := strings.Join([]string{
		"delta --no-gitconfig --line-numbers --width 120",
		"--plus-style 'syntax 28'",
		"--minus-style 'syntax 88'",
		"--line-numbers-plus-style '34'",
		"--line-numbers-minus-style '124'",
		"--line-numbers-zero-style '244'",
	}, " ")
	html := renderDiff(t, sampleRawDiff, pager)
	if !strings.Contains(html, `class="line line-add"`) {
		t.Fatalf("custom-theme delta: no line-add:\n%s", html)
	}
	if !strings.Contains(html, `class="line line-del"`) {
		t.Fatalf("custom-theme delta: no line-del:\n%s", html)
	}
	if !strings.Contains(html, `data-file="f"`) {
		t.Fatalf("custom-theme delta: no data-file:\n%s", html)
	}
}

func TestWrapDiffLines_DeltaNoLineNumbers(t *testing.T) {
	if _, err := exec.LookPath("delta"); err != nil {
		t.Skip("delta not installed")
	}
	html := renderDiff(t, sampleRawDiff, "delta --no-gitconfig --width 120")
	if !strings.Contains(html, `class="line line-add" data-file="f"`) {
		t.Fatalf("delta-no-line-numbers: missing line-add data-file:\n%s", html)
	}
	if !strings.Contains(html, `class="line line-del" data-file="f"`) {
		t.Fatalf("delta-no-line-numbers: missing line-del data-file:\n%s", html)
	}
}

func TestClassifyXterm256(t *testing.T) {
	cases := []struct {
		n    int
		want string
	}{
		{22, "add"}, {28, "add"}, {34, "add"}, {2, "add"}, {10, "add"},
		{52, "del"}, {88, "del"}, {124, "del"}, {1, "del"}, {9, "del"},
		{0, ""}, {7, ""}, {238, ""}, {250, ""}, {15, ""},
	}
	for _, c := range cases {
		got := classifyXterm256(c.n)
		if got != c.want {
			t.Errorf("classifyXterm256(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}
