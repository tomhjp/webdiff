package main

import "testing"

func TestRewriteFileHrefs(t *testing.T) {
	rootDir = "/tmp/webdiff-test"
	worktreesRoot = "/tmp/webdiff-test-wt"

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
			in:   `<a href="file:///tmp/webdiff-test-wt/feature-x/src/main.go">main.go</a>`,
			want: `<a href="/file/worktrees/feature-x/src/main.go">main.go</a>`,
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
