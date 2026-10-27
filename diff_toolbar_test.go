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
			for _, want := range []string{`>files</a>`, `id="wt-rename-btn"`} {
				if !strings.Contains(menu, want) {
					t.Errorf("menu is missing %s", want)
				}
			}
			if query != "?changes=" {
				if !strings.Contains(html[:start], `id="toggle-all"`) {
					t.Error("collapse control is not before the menu")
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
