package main

import (
	"testing"

	terminal "github.com/buildkite/terminal-to-html/v3"
)

// Pi sends 24-bit colours when it runs in Rex. The patched
// terminal-to-html in third_party writes them as inline styles. Before
// the patch, the parameters were read as unrelated SGR codes.
func TestTerminalHTMLTrueColor(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{
			"\x1b[48;2;37;65;49m\x1b[38;2;104;183;141m+ add\x1b[0m",
			`<span class="" style="color:#68b78d;background-color:#254131;">+ add</span>`,
		},
		{
			"\x1b[1;38;2;0;0;255mbold\x1b[0m",
			`<span class="term-fg1" style="color:#0000ff;">bold</span>`,
		},
		// 8-bit and 16-colour SGR keep their classes, which palette.css
		// can override.
		{
			"\x1b[38;5;150mx\x1b[0m \x1b[42my\x1b[0m",
			`<span class="term-fgx150">x</span> <span class="term-bg42">y</span>`,
		},
	} {
		screen, _ := terminal.NewScreen()
		screen.Write([]byte(tc.in))
		if got := screen.AsHTML(); got != tc.want {
			t.Errorf("AsHTML(%q)\n got: %s\nwant: %s", tc.in, got, tc.want)
		}
	}
}
