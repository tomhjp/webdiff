package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDiffToolbar(t *testing.T) {
	_, wt := renameTestWorktree(t)
	selectionWrite(t, wt, "change.txt", "new content\n")
	for _, query := range []string{"", "?context=full", "?changes="} {
		t.Run(query, func(t *testing.T) {
			ref, ok := resolveRepoRef("/worktrees/repo-source/")
			if !ok {
				t.Fatal("cannot resolve worktree")
			}
			w := httptest.NewRecorder()
			renderRepo(w, httptest.NewRequest(http.MethodGet, ref.url+query, nil), ref)
			if w.Code != http.StatusOK {
				t.Fatalf("render: %d", w.Code)
			}
			html := w.Body.String()
			start := strings.Index(html, `<div class="diff-more-menu" id="diff-more-menu" hidden>`)
			if start < 0 {
				t.Fatal("missing hidden options menu")
			}
			end := strings.Index(html[start:], `</div>`) + start
			menu := html[start:end]
			for _, want := range []string{`id="wt-new-btn"`, `>files</a>`, `id="wt-rename-btn"`} {
				if !strings.Contains(menu, want) {
					t.Errorf("menu is missing %s", want)
				}
			}
			// "more" is in the left group with home and the agent
			// button. The diff actions are in the right group.
			right := strings.Index(html, `<div class="diff-right">`)
			if right < end {
				t.Fatal("right group is not after the more menu")
			}
			more := strings.Index(html, `id="diff-more-toggle"`)
			home := strings.Index(html, `href="/">home</a>`)
			if home < 0 || more < home || more > start {
				t.Error("more is not next to home")
			}
			if !strings.Contains(html[right:], `id="diff-picker"`) {
				t.Error("changes picker is not in the right group")
			}
			if query != "?changes=" {
				toggle := strings.Index(html, `id="toggle-all"`)
				if toggle < right || !strings.Contains(html[toggle:], `<use href="#icon-expand-all"/>`) {
					t.Error("expand/collapse control is not an icon in the right group")
				}
				label := ">full context</button>"
				if query == "?context=full" {
					label = ">diff only</button>"
				}
				if !strings.Contains(menu, label) {
					t.Error("context toggle is missing or has the wrong label")
				}
			}
			remove := strings.Index(html, `id="wt-remove-btn"`)
			if remove < end || !strings.Contains(html[remove:], `>remove</button></div>`) {
				t.Error("remove is not the last toolbar control")
			}
		})
	}
}

// TestDiffSelectionReset checks that the diff page shows all changes
// when it cannot show the selection in the URL.
func TestDiffSelectionReset(t *testing.T) {
	_, wt := renameTestWorktree(t)
	selectionWrite(t, wt, "a", "a\n")
	selectionGit(t, wt, "add", "a")
	selectionGit(t, wt, "commit", "-qm", "add a")
	first := selectionGit(t, wt, "rev-parse", "HEAD")
	selectionWrite(t, wt, "f", "one\n")
	selectionGit(t, wt, "add", "f")
	selectionGit(t, wt, "commit", "-qm", "add f")
	// The working tree needs the second commit.
	selectionWrite(t, wt, "f", "two\n")
	ref, ok := resolveRepoRef("/worktrees/repo-source/")
	if !ok {
		t.Fatal("cannot resolve worktree")
	}
	for _, tc := range []struct {
		name  string
		query string
		reset string
	}{
		{"dependent", "?changes=working," + first, "prerequisite"},
		{"stale", "?changes=" + strings.Repeat("0", 40), "no longer on this branch"},
		{"empty", "?changes=", ""},
		{"all", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			renderRepo(w, httptest.NewRequest(http.MethodGet, ref.url+tc.query, nil), ref)
			if w.Code != http.StatusOK {
				t.Fatalf("render: %d: %s", w.Code, w.Body)
			}
			html := w.Body.String()
			marked := strings.Contains(html, `id="diff-picker" data-reset="1"`)
			notice := strings.Contains(html, `class="selection-reset"`)
			if tc.reset == "" {
				if marked || notice {
					t.Fatal("reset a selection that works")
				}
				return
			}
			if !marked || !notice || !strings.Contains(html, tc.reset) {
				t.Fatalf("selection was not reset with the error %q", tc.reset)
			}
			for _, file := range []string{"a", "f"} {
				if !strings.Contains(html, `href="#file-`+file+`"`) {
					t.Errorf("all changes do not include %s", file)
				}
			}
			if !strings.Contains(html, `value="working" checked`) || !strings.Contains(html, `value="`+first+`" checked`) {
				t.Error("picker does not select all changes")
			}
		})
	}
}

// The diff page shows the messages of the selected commits, oldest
// first, in one section before the files. The commit view shows one
// commit: its message and its changes only.
func TestDiffCommitMessages(t *testing.T) {
	_, wt := renameTestWorktree(t)
	selectionWrite(t, wt, "a", "a\n")
	selectionGit(t, wt, "add", "a")
	selectionGit(t, wt, "commit", "-qm", "add a\n\nThe body of <a>.")
	first := selectionGit(t, wt, "rev-parse", "HEAD")
	selectionWrite(t, wt, "b", "b\n")
	selectionGit(t, wt, "add", "b")
	selectionGit(t, wt, "commit", "-qm", "add b")
	second := selectionGit(t, wt, "rev-parse", "HEAD")
	selectionWrite(t, wt, "c", "c\n")
	ref, ok := resolveRepoRef("/worktrees/repo-source/")
	if !ok {
		t.Fatal("cannot resolve worktree")
	}
	render := func(t *testing.T, query string) string {
		t.Helper()
		w := httptest.NewRecorder()
		renderRepo(w, httptest.NewRequest(http.MethodGet, ref.url+query, nil), ref)
		if w.Code != http.StatusOK {
			t.Fatalf("render: %d: %s", w.Code, w.Body)
		}
		return w.Body.String()
	}
	firstLink := `<a class="commit-sha" href="/worktrees/repo-source/?commit=` + first + `"`
	secondLink := `<a class="commit-sha" href="/worktrees/repo-source/?commit=` + second + `"`
	firstBody := `<pre class="commit-message">add a

The body of &lt;a&gt;.</pre>`

	t.Run("all", func(t *testing.T) {
		html := render(t, "")
		section := strings.Index(html, `<details id="commit-messages" open>`)
		if section < 0 {
			t.Fatal("missing commit messages section")
		}
		if f := strings.Index(html, `<details id="file-`); f < section {
			t.Error("commit messages are not before the files")
		}
		if !strings.Contains(html, `<nav class="pillbar"><a class="pill" href="#commit-messages">commits</a>`) {
			t.Error("commits pill is not first")
		}
		if !strings.Contains(html, `<span class="stats-files">2 commits</span>`) {
			t.Error("wrong commit count")
		}
		i1, i2 := strings.Index(html, firstLink), strings.Index(html, secondLink)
		if i1 < section || i2 < i1 {
			t.Error("messages are not oldest first")
		}
		if !strings.Contains(html, firstBody) {
			t.Error("missing the full message of the first commit")
		}
		// The picker lists the commits oldest first, then the working
		// tree. The text of each commit links to its commit view.
		p1 := strings.Index(html, `<a class="diff-picker-label" href="/worktrees/repo-source/?commit=`+first+`" title="view this commit">`+first[:12]+` add a</a>`)
		p2 := strings.Index(html, `<a class="diff-picker-label" href="/worktrees/repo-source/?commit=`+second+`"`)
		pw := strings.Index(html, `value="working"`)
		if p1 < 0 || p2 < p1 || pw < p2 {
			t.Errorf("picker order is wrong: %d %d %d", p1, p2, pw)
		}
	})

	t.Run("working only", func(t *testing.T) {
		// The section has only the entry of the working tree.
		html := render(t, "?changes=working")
		if !strings.Contains(html, `<span class="stats-files">0 commits</span>`) || !strings.Contains(html, `data-commit="working"`) {
			t.Error("no working tree entry without a selected commit")
		}
		if strings.Contains(html, firstLink) || strings.Contains(html, secondLink) {
			t.Error("commit messages without a selected commit")
		}
	})

	t.Run("commit view", func(t *testing.T) {
		html := render(t, "?n=2&commit="+first)
		if !strings.Contains(html, `id="diff-picker" data-commit="1"`) {
			t.Error("picker does not know the commit view")
		}
		if !strings.Contains(html, `<label class="diff-picker-current"><input type="checkbox" value="`+first+`" checked>`) {
			t.Error("picker does not mark the commit of the view")
		}
		// The menu shows only the commit of the view as selected.
		if !strings.Contains(html, `>changes (1)</button>`) {
			t.Error("picker count is not 1")
		}
		for _, value := range []string{second, "working"} {
			if strings.Contains(html, `<input type="checkbox" value="`+value+`" checked>`) {
				t.Errorf("picker selects %s in the commit view", value)
			}
		}
		if !strings.Contains(html, firstBody) || strings.Contains(html, secondLink) {
			t.Error("commit view does not show only its own message")
		}
		if !strings.Contains(html, `<span class="stats-files">1 commit</span>`) {
			t.Error("wrong commit count")
		}
		// The links keep the session number.
		if !strings.Contains(html, `href="/worktrees/repo-source/?commit=`+first+`&amp;n=2"`) {
			t.Error("commit link does not keep the session number")
		}
		for file, want := range map[string]bool{"a": true, "b": false, "c": false} {
			if got := strings.Contains(html, `href="#file-`+file+`"`); got != want {
				t.Errorf("file %s shown = %v, want %v", file, got, want)
			}
		}
	})

	t.Run("unknown commit", func(t *testing.T) {
		html := render(t, "?commit="+strings.Repeat("0", 40))
		if !strings.Contains(html, "is not on this branch") {
			t.Error("missing error for a commit that is not on the branch")
		}
		if strings.Contains(html, `class="selection-reset"`) {
			t.Error("commit view reset to all changes")
		}
	})
}

// Through the desktop client, the diff page has a "send to github"
// button. Without the client it does not. The commit messages have edit
// buttons in both cases, also one for the working tree.
func TestDiffEditAndSendControls(t *testing.T) {
	_, wt := renameTestWorktree(t)
	selectionWrite(t, wt, "a", "a\n")
	selectionGit(t, wt, "add", "a")
	selectionGit(t, wt, "commit", "-qm", "add a")
	head := selectionGit(t, wt, "rev-parse", "HEAD")
	selectionWrite(t, wt, "b", "b\n")
	ref, ok := resolveRepoRef("/worktrees/repo-source/")
	if !ok {
		t.Fatal("cannot resolve worktree")
	}
	render := func(t *testing.T, query string, desktop bool) string {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, ref.url+query, nil)
		if desktop {
			r.Header.Set(clientHeader, "1")
		}
		w := httptest.NewRecorder()
		renderRepo(w, r, ref)
		if w.Code != http.StatusOK {
			t.Fatalf("render: %d: %s", w.Code, w.Body)
		}
		return w.Body.String()
	}
	commitEdit := `<button class="toggle commit-edit" type="button" data-commit="` + head + `"`
	workingEdit := `data-commit="working"`

	html := render(t, "", true)
	if !strings.Contains(html, `id="send-github" data-kind="worktrees" data-name="repo-source"`) ||
		!strings.Contains(html, `data-head="`+head+`" data-commits="1"`) ||
		!strings.Contains(html, `data-dirty="1">send to github</button>`) {
		t.Error("missing or wrong send to github button")
	}
	if !strings.Contains(html, `<div class="commit-messages" data-kind="worktrees" data-name="repo-source">`) {
		t.Error("commit messages have no checkout")
	}
	if !strings.Contains(html, commitEdit) || !strings.Contains(html, workingEdit) {
		t.Error("missing edit buttons")
	}

	// The working tree has an entry also with no commit selected.
	html = render(t, "?changes=working", true)
	if !strings.Contains(html, `id="commit-messages"`) || !strings.Contains(html, workingEdit) || strings.Contains(html, commitEdit) {
		t.Error("working tree entry is wrong with only the working tree selected")
	}
	// The commit view has no working tree entry.
	html = render(t, "?commit="+head, true)
	if !strings.Contains(html, commitEdit) || strings.Contains(html, workingEdit) {
		t.Error("commit view edit buttons are wrong")
	}

	html = render(t, "", false)
	if strings.Contains(html, `id="send-github"`) {
		t.Error("page without the client has send to github")
	}
	if !strings.Contains(html, commitEdit) || !strings.Contains(html, workingEdit) {
		t.Error("page without the client is missing edit buttons")
	}
}
