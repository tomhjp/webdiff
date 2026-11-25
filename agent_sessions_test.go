package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckoutSessionName(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "corp")
	for _, tt := range []struct {
		n    int
		want string
	}{
		{0, "corp-wd"},
		{1, "corp-wd"},
		{2, "corp-wd2"},
		{12, "corp-wd12"},
	} {
		if got := checkoutSessionName(repo, tt.n); got != tt.want {
			t.Errorf("checkoutSessionName(%q, %d) = %q, want %q", repo, tt.n, got, tt.want)
		}
	}
}

func TestParseSessionName(t *testing.T) {
	for _, tt := range []struct {
		name    string
		primary string
		n       int
		ok      bool
	}{
		{"corp-wd", "corp-wd", 1, true},
		{"corp-wd2", "corp-wd", 2, true},
		{"corp-wd20", "corp-wd", 20, true},
		// Session 1 of a repo whose name ends in "-wd2".
		{"foo-wd2-wd", "foo-wd2-wd", 1, true},
		{"foo-wd2-wd3", "foo-wd2-wd", 3, true},
		{"corp-wd1", "", 0, false},
		{"corp-wd02", "", 0, false},
		{"corp-wd21", "", 0, false},
		{"corp-2", "", 0, false},
		{"corp", "", 0, false},
		{"wd2", "", 0, false},
		{"", "", 0, false},
	} {
		primary, n, ok := parseSessionName(tt.name)
		if primary != tt.primary || n != tt.n || ok != tt.ok {
			t.Errorf("parseSessionName(%q) = %q, %d, %v, want %q, %d, %v", tt.name, primary, n, ok, tt.primary, tt.n, tt.ok)
		}
	}
}

// The name of each session gives back its checkout and number.
func TestSessionNameRoundTrip(t *testing.T) {
	root := t.TempDir()
	for _, repo := range []string{"corp", "corp-wd", "foo-wd2", "a.b"} {
		path := filepath.Join(root, repo)
		for n := 1; n <= maxCheckoutSessions; n++ {
			name := checkoutSessionName(path, n)
			primary, got, ok := parseSessionName(name)
			if !ok || primary != repoSessionName(path) || got != n {
				t.Errorf("parseSessionName(%q) = %q, %d, %v, want %q, %d, true", name, primary, got, ok, repoSessionName(path), n)
			}
		}
	}
}

func TestSessionNumberParam(t *testing.T) {
	for _, tt := range []struct {
		query string
		n     int
		ok    bool
	}{
		{"", 1, true},
		{"n=1", 1, true},
		{"n=3", 3, true},
		{"n=0", 0, false},
		{"n=21", 0, false},
		{"n=x", 0, false},
	} {
		q, err := url.ParseQuery(tt.query)
		if err != nil {
			t.Fatal(err)
		}
		n, ok := sessionNumberParam(q)
		if n != tt.n || ok != tt.ok {
			t.Errorf("sessionNumberParam(%q) = %d, %v, want %d, %v", tt.query, n, ok, tt.n, tt.ok)
		}
	}
}

func TestNewAgentURL(t *testing.T) {
	agentName, agentCmd = "claude", "claude"
	if got, want := newAgentURL("/repos/corp/", ""), "/agent/repos/corp/?new=1"; got != want {
		t.Errorf("newAgentURL = %q, want %q", got, want)
	}
	if got, want := newAgentURL("/repos/corp/", "pi"), "/agent/repos/corp/?agent=pi&new=1"; got != want {
		t.Errorf("newAgentURL = %q, want %q", got, want)
	}
}

// The "more" menus of the diff page and the stream page link to the
// other running sessions of the checkout, but not to the session of
// the page.
func TestMoreMenuOtherSessions(t *testing.T) {
	_, wt := renameTestWorktree(t)
	ref, ok := resolveRepoRef("/worktrees/repo-source/")
	if !ok {
		t.Fatal("cannot resolve worktree")
	}
	saved := rexc
	t.Cleanup(func() { rexc = saved })
	rexc = &rexClient{
		sessions: map[string]*rexSession{
			"s1": {
				ID:         "s1",
				Label:      checkoutSessionName(wt, 1),
				Agent:      "b1",
				AgentLabel: "pi",
			},
			"s3": {
				ID:         "s3",
				Label:      checkoutSessionName(wt, 3),
				Agent:      "b3",
				AgentLabel: "claude",
			},
		},
	}
	link1 := `<a class="toggle other-session" href="/stream/worktrees/repo-source/" title="open agent session 1 of this checkout">pi</a>`
	link3 := `<a class="toggle other-session" href="/stream/worktrees/repo-source/?n=3" title="open agent session 3 of this checkout">claude #3</a>`
	menu := func(t *testing.T, body string) string {
		t.Helper()
		start := strings.Index(body, `<div class="diff-more-menu" id="diff-more-menu" hidden>`)
		if start < 0 {
			t.Fatal("missing more menu")
		}
		return body[start : strings.Index(body[start:], `</div>`)+start]
	}
	for _, tc := range []struct {
		n          int
		want, skip []string
	}{
		{1, []string{link3}, []string{link1}},
		{2, []string{link1, link3}, nil},
		{3, []string{link1}, []string{link3}},
	} {
		t.Run(fmt.Sprintf("diff n=%d", tc.n), func(t *testing.T) {
			w := httptest.NewRecorder()
			renderRepo(w, httptest.NewRequest(http.MethodGet, ref.url+sessionQuery(tc.n, ""), nil), ref)
			m := menu(t, w.Body.String())
			for _, s := range tc.want {
				if !strings.Contains(m, s) {
					t.Errorf("menu is missing %s", s)
				}
			}
			for _, s := range tc.skip {
				if strings.Contains(m, s) {
					t.Errorf("menu has %s", s)
				}
			}
		})
		t.Run(fmt.Sprintf("stream n=%d", tc.n), func(t *testing.T) {
			w := httptest.NewRecorder()
			writeStreamPage(t.Context(), w, "%0", ref.url, "pane", checkoutSessionName(wt, tc.n), "")
			m := menu(t, w.Body.String())
			for _, s := range tc.want {
				if !strings.Contains(m, s) {
					t.Errorf("menu is missing %s", s)
				}
			}
			for _, s := range tc.skip {
				if strings.Contains(m, s) {
					t.Errorf("menu has %s", s)
				}
			}
		})
	}
}
