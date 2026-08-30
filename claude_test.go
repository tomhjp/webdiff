package main

import (
	"slices"
	"testing"
)

func TestStreamDelta(t *testing.T) {
	cases := []struct {
		name       string
		old, new   []string
		drop, keep int
		append     []string
	}{
		{"unchanged", []string{"a", "b"}, []string{"a", "b"}, 0, 2, nil},
		{"append", []string{"a", "b"}, []string{"a", "b", "c"}, 0, 2, []string{"c"}},
		{"history shift", []string{"a", "b", "c"}, []string{"b", "c", "d"}, 1, 2, []string{"d"}},
		{"pi live tail redraw", []string{"a", "answer", "working", "footer"}, []string{"a", "answer", "more", "working", "footer"}, 0, 2, []string{"more", "working", "footer"}},
		{"shift and tail redraw", []string{"old", "a", "b", "status"}, []string{"a", "b", "new", "status"}, 1, 2, []string{"new", "status"}},
		{"full redraw", []string{"a", "b", "c"}, []string{"x", "y"}, 3, 0, []string{"x", "y"}},
		{"single blank is not an anchor", []string{"a", "", "b"}, []string{"", "x"}, 3, 0, []string{"", "x"}},
		{"empty old", nil, []string{"a"}, 0, 0, []string{"a"}},
		{"empty new", []string{"a", "b"}, nil, 2, 0, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			drop, keep, appendLines := streamDelta(tc.old, tc.new)
			if drop != tc.drop || keep != tc.keep || !slices.Equal(appendLines, tc.append) {
				t.Fatalf("streamDelta(%v, %v) = (%d, %d, %v), want (%d, %d, %v)", tc.old, tc.new, drop, keep, appendLines, tc.drop, tc.keep, tc.append)
			}
			got := append(slices.Clone(tc.old[drop:drop+keep]), appendLines...)
			if !slices.Equal(got, tc.new) {
				t.Fatalf("patch reconstructs %v, want %v", got, tc.new)
			}
		})
	}
}

func TestRewriteFileHrefs(t *testing.T) {
	rootDir = "/tmp/webdiff-test"
	worktreesRoot = "/tmp/webdiff-test/wt"

	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "repo file",
			in:   `before <a href="file:///tmp/webdiff-test/test-repo/foo.txt">foo.txt</a> after`,
			want: `before <a href="/file/repos/test-repo/foo.txt">foo.txt</a> after`,
		},
		{
			name: "worktree file",
			in:   `<a href="file:///tmp/webdiff-test/wt/corp/feature-x/src/main.go">main.go</a>`,
			want: `<a href="/file/worktrees/corp-feature-x/src/main.go">main.go</a>`,
		},
		{
			// One level under worktreesRoot is the grouping dir, not a
			// worktree, and must not resolve as either kind.
			name: "worktrees grouping dir — left alone",
			in:   `<a href="file:///tmp/webdiff-test/wt/corp">corp</a>`,
			want: `<a href="file:///tmp/webdiff-test/wt/corp">corp</a>`,
		},
		{
			name: "fragment preserved",
			in:   `<a href="file:///tmp/webdiff-test/test-repo/foo.go#L42">L42</a>`,
			want: `<a href="/file/repos/test-repo/foo.go#L42">L42</a>`,
		},
		{
			name: "percent-encoded path",
			in:   `<a href="file:///tmp/webdiff-test/test-repo/has%20space/x.txt">x</a>`,
			want: `<a href="/file/repos/test-repo/has%20space/x.txt">x</a>`,
		},
		{
			name: "outside roots — left alone",
			in:   `<a href="file:///etc/passwd">passwd</a>`,
			want: `<a href="file:///etc/passwd">passwd</a>`,
		},
		{
			name: "no file:// anchors — unchanged",
			in:   `<a href="https://example.com">x</a>`,
			want: `<a href="https://example.com">x</a>`,
		},
		{
			name: "multiple hrefs in one string",
			in:   `<a href="file:///tmp/webdiff-test/r/a">a</a> and <a href="file:///tmp/webdiff-test/r/b">b</a>`,
			want: `<a href="/file/repos/r/a">a</a> and <a href="/file/repos/r/b">b</a>`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := rewriteFileHrefs(tc.in)
			if got != tc.want {
				t.Errorf("rewriteFileHrefs(...):\n  got:  %s\n  want: %s", got, tc.want)
			}
		})
	}
}

// resolveAgent is the allowlist for `?agent=`, whose value reaches
// `bash -lc` — anything not in knownAgents must come back as the
// configured default rather than as itself.
func TestResolveAgent(t *testing.T) {
	agentName, agentCmd = "claude", "claude"

	cases := []struct {
		id       string
		wantName string
		wantCmd  string
	}{
		{"pi", "pi", "pi"},
		{"claude", "claude", "claude"},
		{"", "claude", "claude"},
		{"bogus", "claude", "claude"},
		{"rm -rf /", "claude", "claude"},
		{"PI", "claude", "claude"},
	}
	for _, tc := range cases {
		name, cmd := resolveAgent(tc.id)
		if name != tc.wantName || cmd != tc.wantCmd {
			t.Errorf("resolveAgent(%q) = (%q, %q), want (%q, %q)", tc.id, name, cmd, tc.wantName, tc.wantCmd)
		}
	}
}

// An operator-configured agent outside knownAgents still has to be
// offered, or the drop-down couldn't express "leave it as it is".
func TestAgentChoicesIncludesConfiguredDefault(t *testing.T) {
	agentName, agentCmd = "opencode", "opencode --foo"
	got := agentChoices()
	want := []string{"claude", "opencode", "pi"}
	if !slices.Equal(got, want) {
		t.Errorf("agentChoices() = %v, want %v", got, want)
	}

	agentName, agentCmd = "claude", "claude"
	got = agentChoices()
	want = []string{"claude", "pi"}
	if !slices.Equal(got, want) {
		t.Errorf("agentChoices() = %v, want %v", got, want)
	}
}

func TestAgentQuery(t *testing.T) {
	agentName, agentCmd = "claude", "claude"

	cases := []struct{ id, want string }{
		{"pi", "?agent=pi"},
		{"claude", ""},
		{"", ""},
		{"bogus", ""},
	}
	for _, tc := range cases {
		if got := agentQuery(tc.id); got != tc.want {
			t.Errorf("agentQuery(%q) = %q, want %q", tc.id, got, tc.want)
		}
	}
}

func TestAgentFromStartCommand(t *testing.T) {
	cases := []struct {
		name     string
		flagName string
		flagCmd  string
		start    string
		want     string
	}{
		{
			name:     "known agent",
			flagName: "claude",
			flagCmd:  "claude",
			start:    "bash -lc pi",
			want:     "pi",
		},
		{
			name:     "the default itself",
			flagName: "claude",
			flagCmd:  "claude",
			start:    "bash -lc claude",
			want:     "claude",
		},
		{
			// An -agent value carrying flags has to match whole, since
			// its first token isn't a knownAgents id.
			name:     "configured command with flags",
			flagName: "opencode",
			flagCmd:  "opencode --foo bar",
			start:    "bash -lc opencode --foo bar",
			want:     "opencode",
		},
		{
			name:     "unrecognised command",
			flagName: "claude",
			flagCmd:  "claude",
			start:    "bash -lc vim",
			want:     "",
		},
		{
			// A user's own pane, not spawned through webdiff.
			name:     "no bash wrapper",
			flagName: "claude",
			flagCmd:  "claude",
			start:    "",
			want:     "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			agentName, agentCmd = tc.flagName, tc.flagCmd
			if got := agentFromStartCommand(tc.start); got != tc.want {
				t.Errorf("agentFromStartCommand(%q) = %q, want %q", tc.start, got, tc.want)
			}
		})
	}
}
