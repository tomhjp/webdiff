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
