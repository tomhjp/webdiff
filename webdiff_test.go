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

// TestWrapDiffLines_NewFileBlankAddedLines covers a brand-new file
// where every line is added and the body contains literal blank lines.
// terminal-to-html drops the trailing \x1b[K bg cells when the body
// has no characters, so empty added lines arrive at wrapDiffLines with
// no `term-bgxN` class — classification has to fall through to the
// gutter-position heuristic. This is exactly the case the friend's
// "lines render with the wrong bg" screenshot was hitting.
func TestWrapDiffLines_NewFileBlankAddedLines(t *testing.T) {
	if _, err := exec.LookPath("delta"); err != nil {
		t.Skip("delta not installed")
	}
	raw := "" +
		"\x1b[1mdiff --git a/newfile.go b/newfile.go\x1b[m\n" +
		"\x1b[1mnew file mode 100644\x1b[m\n" +
		"\x1b[1mindex 0000000..1111111\x1b[m\n" +
		"\x1b[1m--- /dev/null\x1b[m\n" +
		"\x1b[1m+++ b/newfile.go\x1b[m\n" +
		"\x1b[36m@@ -0,0 +1,6 @@\x1b[m\n" +
		"\x1b[32m+package main\x1b[m\n" +
		"\x1b[32m+\x1b[m\n" +
		"\x1b[32m+import (\x1b[m\n" +
		"\x1b[32m+\t\"foo\"\x1b[m\n" +
		"\x1b[32m+\x1b[m\n" +
		"\x1b[32m+)\x1b[m\n"
	html := renderDiff(t, raw, "delta --no-gitconfig --line-numbers --width 80")
	for _, want := range []string{
		`line-add" data-file="f" data-new-line="1"`, // package main
		`line-add" data-file="f" data-new-line="2"`, // blank line
		`line-add" data-file="f" data-new-line="3"`, // import (
		`line-add" data-file="f" data-new-line="5"`, // blank line inside import block
		`line-add" data-file="f" data-new-line="6"`, // )
	} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %q in output:\n%s", want, html)
		}
	}
	// Hunk-header decoration `1│` inside the heading box must not be
	// misread as a removed line.
	if strings.Contains(html, `data-old-line="1"`) {
		t.Errorf("hunk header decoration leaked into a line-del:\n%s", html)
	}
}

// TestWrapDiffLines_EmphLineSplit covers a removed/added line that
// delta paints with both a regular diff bg (bgx52/bgx22) AND an emph
// bg (bgx124/bgx28) on the differing words. splitDiffLine must split
// at the first bg span (which is always the "regular" bg covering the
// leading whitespace) — not the most-frequent class — otherwise the
// leading whitespace gets stranded in the (no-bg) gutter prefix and
// renders as page background instead of regular diff red/green.
func TestWrapDiffLines_EmphLineSplit(t *testing.T) {
	if _, err := exec.LookPath("delta"); err != nil {
		t.Skip("delta not installed")
	}
	raw := "" +
		"\x1b[1mdiff --git a/f.go b/f.go\x1b[m\n" +
		"\x1b[1mindex 0..1 100644\x1b[m\n" +
		"\x1b[1m--- a/f.go\x1b[m\n" +
		"\x1b[1m+++ b/f.go\x1b[m\n" +
		"\x1b[36m@@ -1,1 +1,1 @@\x1b[m\n" +
		"\x1b[31m-\t\t\t\tmaps.Copy(globalClaims, jwtClaims)\x1b[m\n" +
		"\x1b[32m+\t\t\t\tmaps.Copy(globalClaims, globalJWTClaims)\x1b[m\n"
	html := renderDiff(t, raw, "delta --no-gitconfig --line-numbers --width 120")
	// The leading whitespace span (bgx52 / bgx22) MUST be inside
	// line-tail so the diff bg covers it. If splitDiffLine got
	// confused by the emph bg the leading whitespace would land in
	// line-prefix instead, which has no diff bg.
	for _, want := range []string{
		`<span class="line-tail"><span class="term-bgx52">`,
		`<span class="line-tail"><span class="term-fgx231 term-bgx22">`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %q in output:\n%s", want, html)
		}
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
