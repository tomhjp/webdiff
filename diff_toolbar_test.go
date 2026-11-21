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
