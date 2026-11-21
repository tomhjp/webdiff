package main

import (
	"net/url"
	"path/filepath"
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
