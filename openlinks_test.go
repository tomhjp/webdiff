package main

import (
	"strings"
	"testing"
)

func TestWriteOpenButtons(t *testing.T) {
	saved := sshHost
	defer func() { sshHost = saved }()

	t.Run("emits both schemes", func(t *testing.T) {
		sshHost = "slop"
		var b strings.Builder
		writeOpenButtons(&b, "/home/ubuntu/ai/webdiff", "dir-open")
		got := b.String()
		for _, want := range []string{
			`href="zed://ssh/slop/home/ubuntu/ai/webdiff"`,
			`href="webdiff://ghostty?host=slop&amp;dir=%2Fhome%2Fubuntu%2Fai%2Fwebdiff"`,
			`class="dir-open open-link"`,
			`<use href="#icon-zed"/>`,
			`<use href="#icon-ghostty"/>`,
		} {
			if !strings.Contains(got, want) {
				t.Errorf("output missing %s:\n%s", want, got)
			}
		}
	})

	// A <use> naming a symbol the sprite doesn't define renders as an empty
	// box, which looks like a missing button rather than an error.
	t.Run("sprite defines every referenced symbol", func(t *testing.T) {
		sshHost = "slop"
		var b strings.Builder
		writeOpenButtons(&b, "/tmp/x", "dir-open")
		for _, id := range []string{"icon-zed", "icon-ghostty"} {
			if !strings.Contains(b.String(), `href="#`+id+`"`) {
				t.Fatalf("no button references %s", id)
			}
			if !strings.Contains(iconSprite, `<symbol id="`+id+`"`) {
				t.Errorf("sprite has no <symbol id=%q>", id)
			}
		}
	})

	// Without a host the links can't name anywhere to connect, so they're
	// suppressed rather than rendered dead.
	t.Run("suppressed without ssh host", func(t *testing.T) {
		sshHost = ""
		var b strings.Builder
		writeOpenButtons(&b, "/home/ubuntu/ai/webdiff", "dir-open")
		if b.Len() != 0 {
			t.Errorf("want no output, got %q", b.String())
		}
	})

	t.Run("suppressed without dir", func(t *testing.T) {
		sshHost = "slop"
		var b strings.Builder
		writeOpenButtons(&b, "", "dir-open")
		if b.Len() != 0 {
			t.Errorf("want no output, got %q", b.String())
		}
	})

	// A path containing HTML or query metacharacters must not be able to
	// break out of the attribute or graft on another query parameter.
	t.Run("escapes hostile paths", func(t *testing.T) {
		sshHost = "slop"
		var b strings.Builder
		writeOpenButtons(&b, `/tmp/a"><script>&x=1`, "dir-open")
		got := b.String()
		if strings.Contains(got, "<script>") {
			t.Errorf("unescaped markup leaked:\n%s", got)
		}
		// The & belongs to the path, so it must arrive percent-encoded
		// rather than as a parameter separator.
		if strings.Contains(got, "&x=1") {
			t.Errorf("path's & not encoded:\n%s", got)
		}
	})
}
