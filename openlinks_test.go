package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteOpenButtons(t *testing.T) {
	saved := sshHost
	defer func() { sshHost = saved }()

	t.Run("emits all schemes", func(t *testing.T) {
		sshHost = "slop"
		var b strings.Builder
		writeOpenButtons(&b, "/home/ubuntu/ai/webdiff", "dir-open")
		got := b.String()
		for _, want := range []string{
			`href="zed://ssh/slop/home/ubuntu/ai/webdiff"`,
			`href="vscode://vscode-remote/ssh-remote+slop/home/ubuntu/ai/webdiff"`,
			`href="webdiff://ghostty?host=slop&amp;dir=%2Fhome%2Fubuntu%2Fai%2Fwebdiff"`,
			`class="dir-open open-link"`,
			`<use href="#icon-zed"/>`,
			`<use href="#icon-vscode"/>`,
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
		for _, id := range []string{"icon-zed", "icon-vscode", "icon-ghostty"} {
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

func TestEditorFileTarget(t *testing.T) {
	for _, tc := range []struct {
		dir, file string
		line      int
		want      string
	}{
		{"/r", "a.go", 3, "/r/a.go:3"},
		{"/r/", "dir/a.go", 1, "/r/dir/a.go:1"},
		// VS Code opens a remote path as a file only with a line.
		{"/r", "a.go", 0, "/r/a.go:1"},
		// "#" and "?" would end the path of the URL.
		{"/r", "x/a b#?.go", 7, "/r/x/a%20b%23%3F.go:7"},
	} {
		if got := editorFileTarget(tc.dir, tc.file, tc.line); got != tc.want {
			t.Errorf("editorFileTarget(%q, %q, %d) = %q, want %q", tc.dir, tc.file, tc.line, got, tc.want)
		}
	}
}

func TestFirstChangedLine(t *testing.T) {
	for _, tc := range []struct {
		name  string
		canon []diffLine
		want  int
	}{
		{"empty", nil, 1},
		{"add after context", []diffLine{
			{kind: ' ', oldLine: 4, newLine: 4},
			{kind: '+', newLine: 5},
		}, 5},
		{"delete after context", []diffLine{
			{kind: ' ', oldLine: 4, newLine: 6},
			{kind: '-', oldLine: 5},
		}, 7},
		{"delete at start", []diffLine{
			{kind: '-', oldLine: 1},
			{kind: ' ', oldLine: 2, newLine: 1},
		}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := firstChangedLine(tc.canon); got != tc.want {
				t.Errorf("firstChangedLine = %d, want %d", got, tc.want)
			}
		})
	}
}

// The editor links in the diff page heading open the checkout. The
// heading of each file has links that open that file at its first change.
func TestDiffPageEditorLinks(t *testing.T) {
	saved := sshHost
	defer func() { sshHost = saved }()
	sshHost = "slop"

	repo, wt := renameTestWorktree(t)
	lines := "1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n"
	for _, name := range []string{"small.go", "big.go", "gone.txt"} {
		selectionWrite(t, repo, name, lines)
	}
	selectionGit(t, repo, "add", ".")
	selectionGit(t, repo, "commit", "-qm", "files")
	selectionGit(t, wt, "reset", "-q", "--hard", "main")
	selectionWrite(t, wt, "small.go", "1\n2\n3\n4\nfive\n6\n7\n8\n9\n10\n")
	selectionWrite(t, wt, "big.go", "1\n2\nthree\nfour\nfive\n6\n7\n8\n9\n10\n")
	if err := os.Remove(filepath.Join(wt, "gone.txt")); err != nil {
		t.Fatal(err)
	}

	ref, ok := resolveRepoRef("/worktrees/repo-source/")
	if !ok {
		t.Fatal("cannot resolve worktree")
	}
	w := httptest.NewRecorder()
	renderRepo(w, httptest.NewRequest(http.MethodGet, ref.url, nil), ref)
	if w.Code != http.StatusOK {
		t.Fatalf("render: %d", w.Code)
	}
	html := w.Body.String()

	toolbar, files, ok := strings.Cut(html, `<nav class="pillbar">`)
	if !ok {
		t.Fatal("missing pillbar")
	}
	for _, want := range []string{
		`class="toggle open-link" href="zed://ssh/slop` + wt + `"`,
		`class="toggle open-link" href="vscode://vscode-remote/ssh-remote+slop` + wt + `"`,
		`href="webdiff://ghostty?host=slop&amp;dir=`,
	} {
		if !strings.Contains(toolbar, want) {
			t.Errorf("page heading is missing %s", want)
		}
	}

	for _, want := range []string{
		`class="file-open open-link" href="zed://ssh/slop` + wt + `/big.go:3"`,
		`class="file-open open-link" href="vscode://vscode-remote/ssh-remote+slop` + wt + `/big.go:3"`,
		`class="file-open open-link" href="zed://ssh/slop` + wt + `/small.go:5"`,
		`class="file-open open-link" href="vscode://vscode-remote/ssh-remote+slop` + wt + `/small.go:5"`,
	} {
		if !strings.Contains(files, want) {
			t.Errorf("file headings are missing %s", want)
		}
	}
	if !strings.Contains(files, `data-file="gone.txt"`) {
		t.Fatal("deleted file is not in the diff")
	}
	if strings.Contains(files, "gone.txt:") {
		t.Error("deleted file has editor links")
	}
}
