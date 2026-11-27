package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSSH makes sshCheckout and git use an ssh that runs the command
// on this host, as a real ssh does on the remote host: it joins the
// arguments after the destination and gives them to the shell.
func fakeSSH(t *testing.T) {
	t.Helper()
	// git knows the options of a program with the name "ssh".
	prog := filepath.Join(t.TempDir(), "ssh")
	script := `#!/bin/sh
while [ $# -gt 0 ]; do
	case "$1" in
	-o|-p|-l|-i) shift 2 ;;
	--) shift; break ;;
	-*) shift ;;
	*) break ;;
	esac
done
shift
exec sh -c "$*"
`
	if err := os.WriteFile(prog, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	old := sshProgram
	sshProgram = prog
	t.Cleanup(func() { sshProgram = old })
	t.Setenv("GIT_SSH_COMMAND", prog)
}

func TestParseSSHCheckout(t *testing.T) {
	c, err := parseSSHCheckout("slop:/home/ubuntu/ai/wt/x/y")
	if err != nil || c.host != "slop" || c.dir != "/home/ubuntu/ai/wt/x/y" {
		t.Errorf("parseSSHCheckout = %+v, %v", c, err)
	}
	if c.fetchURL() != "slop:/home/ubuntu/ai/wt/x/y" {
		t.Errorf("fetchURL = %q", c.fetchURL())
	}
	for _, bad := range []string{"", "slop", "slop:relative", ":/x", "-oProxyCommand=x:/y", "a/b:/c"} {
		if _, err := parseSSHCheckout(bad); err == nil {
			t.Errorf("parseSSHCheckout(%q): no error", bad)
		}
	}
}

func TestParseSSHInfo(t *testing.T) {
	// tailscale has the sole remote "ro" and no local main.
	out := "head abc\nbranch \nremote ro git://proxy/tailscale/tailscale.git\ntrack main \ntrack master \nref refs/remotes/ro/main\n"
	got, err := parseSSHInfo(out)
	if err != nil {
		t.Fatal(err)
	}
	if got.Head != "abc" || got.Branch != "" || got.RemoteName != "ro" || got.Remote != "git://proxy/tailscale/tailscale.git" || got.Main != "main" || got.MainRef != "refs/remotes/ro/main" {
		t.Errorf("parseSSHInfo = %+v", got)
	}
	if _, err := parseSSHInfo("head abc\nbranch x\n"); err == nil {
		t.Errorf("no remote: no error")
	}
}

// TestGitHubOverSSH sends a branch to a bare repo that stands in for
// GitHub, and fetches it back, through ssh.
func TestGitHubOverSSH(t *testing.T) {
	fakeSSH(t)
	repo, git, shas := newTestRepo(t)
	ctx := context.Background()
	github := filepath.Join(filepath.Dir(filepath.Dir(repo)), "github")
	remote := filepath.Join(github, "owner", "proj.git")
	runGit := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Other",
			"GIT_AUTHOR_EMAIL=other@example.com",
			"GIT_COMMITTER_NAME=Other",
			"GIT_COMMITTER_EMAIL=other@example.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	// commitOnGitHub adds a commit on top of parent to the branch
	// feature on GitHub. The commit writes content to the file "a".
	withStdin := func(stdin string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", remote}, args...)...)
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git %s: %v", strings.Join(args, " "), err)
		}
		return strings.TrimSpace(string(out))
	}
	commitOnGitHub := func(parent, content string) string {
		t.Helper()
		blob := withStdin(content, "hash-object", "-w", "--stdin")
		// Keep the files of parent, and change "a".
		var entries strings.Builder
		for line := range strings.Lines(runGit(remote, "ls-tree", parent)) {
			if !strings.HasSuffix(strings.TrimRight(line, "\n"), "\ta") {
				entries.WriteString(line)
				if !strings.HasSuffix(line, "\n") {
					entries.WriteString("\n")
				}
			}
		}
		entries.WriteString("100644 blob " + blob + "\ta\n")
		tree := withStdin(entries.String(), "mktree")
		sha := runGit(remote, "commit-tree", tree, "-p", parent, "-m", "on github")
		runGit(remote, "update-ref", "refs/heads/feature", sha)
		return sha
	}

	// A path that needs quotes for the shell.
	wt := filepath.Join(filepath.Dir(repo), "it's a worktree")
	// The branch can be checked out in only one place.
	git("checkout", "-q", "--detach")
	git("worktree", "add", "-q", wt, "feature")
	// A subdirectory of the checkout also works.
	if err := os.Mkdir(filepath.Join(wt, "sub"), 0700); err != nil {
		t.Fatal(err)
	}
	src := &sshCheckout{
		host: "sandbox",
		dir:  filepath.Join(wt, "sub"),
	}
	s := &githubSender{
		urlFormat: github + "/{repo}.git",
		cacheDir:  t.TempDir(),
	}

	info, err := checkoutInfo(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	if info.Repo != "owner/proj" || info.Branch != "feature" || info.Head != shas[2] || info.Main != "main" || info.MainRef != "refs/remotes/origin/main" || info.Count != 3 || len(info.Commits) != 3 || info.Commits[0].SHA != shas[2] || info.Commits[0].Subject != "add c" {
		t.Errorf("checkoutInfo = %+v", info)
	}

	res, err := s.send(ctx, src, "feature", shas[2])
	if err != nil {
		t.Fatal(err)
	}
	if res.Repo != "owner/proj" || res.Head != shas[2] {
		t.Errorf("send = %+v", res)
	}
	if got := runGit(remote, "rev-parse", "refs/heads/feature"); got != shas[2] {
		t.Errorf("GitHub branch = %s, want %s", got, shas[2])
	}
	if _, err := s.send(ctx, src, "feature", shas[1]); err == nil {
		t.Errorf("send with an old HEAD: no error")
	}

	fetch := func(branch, wantResult string) gitHubFetchResponse {
		t.Helper()
		res, err := s.fetchFromGitHub(ctx, src, branch)
		if err != nil {
			t.Fatal(err)
		}
		if res.Result != wantResult {
			t.Fatalf("fetch result = %q (%s), want %q", res.Result, res.Error, wantResult)
		}
		if got := runGit(wt, "rev-parse", res.Ref); got != res.Head {
			t.Errorf("%s = %s, want %s", res.Ref, got, res.Head)
		}
		return res
	}
	head := func() string {
		return runGit(wt, "rev-parse", "HEAD")
	}

	fetch("feature", "up-to-date")

	// A new commit on GitHub moves the branch forward.
	gh := commitOnGitHub(shas[2], "from github\n")
	res2 := fetch("feature", "fast-forward")
	if res2.Ref != "refs/remotes/origin/feature" || res2.Head != gh || head() != gh {
		t.Errorf("fetch = %+v, HEAD = %s, want %s", res2, head(), gh)
	}

	// A local commit after it is not lost.
	runGit(wt, "commit", "-q", "--allow-empty", "-m", "local")
	local := head()
	fetch("feature", "ahead")
	if head() != local {
		t.Errorf("HEAD = %s, want %s", head(), local)
	}

	// The branch on GitHub is replaced: no merge.
	other := commitOnGitHub(shas[2], "other\n")
	fetch("feature", "diverged")
	if head() != local {
		t.Errorf("HEAD = %s, want %s", head(), local)
	}

	// A change in the working tree stops the fast-forward.
	runGit(wt, "reset", "-q", "--hard", shas[2])
	if err := os.WriteFile(filepath.Join(wt, "a"), []byte("mine\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if res := fetch("feature", "merge-failed"); res.Error == "" {
		t.Errorf("merge-failed with no error")
	}
	if head() != shas[2] {
		t.Errorf("HEAD = %s, want %s", head(), shas[2])
	}
	runGit(wt, "checkout", "-q", "--", "a")
	if res := fetch("feature", "fast-forward"); head() != other || res.Head != other {
		t.Errorf("HEAD = %s, want %s", head(), other)
	}

	// Another branch only updates its remote-tracking ref.
	runGit(remote, "update-ref", "refs/heads/elsewhere", shas[0])
	if res := fetch("elsewhere", "other-branch"); res.Head != shas[0] || res.Checkout != "feature" {
		t.Errorf("fetch = %+v", res)
	}
	if head() != other {
		t.Errorf("HEAD = %s, want %s", head(), other)
	}

	if _, err := s.fetchFromGitHub(ctx, src, "missing"); err == nil {
		t.Errorf("fetch of a missing branch: no error")
	}
}
