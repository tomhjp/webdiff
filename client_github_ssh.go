package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/tomhjp/webdiff/internal/repostats"
)

// sshProgram is the ssh command that sshCheckout uses. Tests change it.
var sshProgram = "ssh"

// sshCheckout is a checkout on a host that the desktop reaches with
// ssh, for example the sandbox of a VS Code Remote-SSH window. It does
// not use the webdiff server of the sandbox: git fetches and pushes
// over ssh, and the other git commands also run over ssh.
type sshCheckout struct {
	// host is the ssh destination, for example an alias in
	// ~/.ssh/config.
	host string
	// dir is the absolute path of the checkout on host.
	dir string
}

// parseSSHCheckout parses "<host>:<path>", where path is absolute.
func parseSSHCheckout(s string) (*sshCheckout, error) {
	host, dir, ok := strings.Cut(s, ":")
	if !ok || host == "" || strings.HasPrefix(host, "-") || strings.Contains(host, "/") || !strings.HasPrefix(dir, "/") {
		return nil, fmt.Errorf("%q is not <host>:<absolute path>", s)
	}
	return &sshCheckout{
		host: host,
		dir:  dir,
	}, nil
}

func (c *sshCheckout) fetchURL() string {
	// The scp form. git uses ssh for it, because there is no slash
	// before the colon.
	return c.host + ":" + c.dir
}

// run runs the shell script script in the checkout on the host, and
// returns its output with no trailing space.
func (c *sshCheckout) run(ctx context.Context, script string) (string, error) {
	// ssh gives the command to the shell of the user on the host.
	cmd := exec.CommandContext(ctx, sshProgram, "-o", "BatchMode=yes", "--", c.host, "cd "+shellQuote(c.dir)+" || exit\n"+script)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("ssh %s: %s", c.host, msg)
	}
	return strings.TrimSpace(string(out)), nil
}

// git runs git with args in the checkout on the host.
func (c *sshCheckout) git(ctx context.Context, args ...string) (string, error) {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = shellQuote(a)
	}
	return c.run(ctx, "git "+strings.Join(quoted, " "))
}

// infoScript writes the facts that sshCheckout.details needs, one on
// each line, with a tag first. It uses one ssh connection for all of
// them.
const infoScript = `head=$(git rev-parse --verify -q HEAD) || { echo "the checkout has no commits" >&2; exit 1; }
echo "head $head"
echo "top $(git rev-parse --show-toplevel)"
echo "branch $(git symbolic-ref --short -q HEAD)"
git remote -v | while read -r name url kind; do
	[ "$kind" = "(fetch)" ] && echo "remote $name $url"
done
for b in main master; do
	echo "track $b $(git config --get "branch.$b.remote")"
done
git for-each-ref --format='ref %(refname)' 'refs/remotes/*/main' 'refs/remotes/*/master' refs/heads/main refs/heads/master
`

// sshInfo is what details returns about an sshCheckout.
type sshInfo struct {
	gitHubInfo
	// Branch is the current branch, or "" when HEAD is detached.
	Branch string
	// RemoteName is the name of the default remote.
	RemoteName string
	// Top is the top directory of the checkout.
	Top string
}

// details returns the facts about the checkout. It makes the same
// choices as handleGitHubInfo does on the sandbox. If c.dir is a
// subdirectory of the checkout, details changes c.dir to the top
// directory, because git fetches only from the top directory.
func (c *sshCheckout) details(ctx context.Context) (sshInfo, error) {
	out, err := c.run(ctx, infoScript)
	if err != nil {
		return sshInfo{}, err
	}
	info, err := parseSSHInfo(out)
	if err != nil {
		return sshInfo{}, err
	}
	if info.Top != "" {
		c.dir = info.Top
	}
	return info, nil
}

func parseSSHInfo(out string) (sshInfo, error) {
	var info sshInfo
	var remotes []string
	urls := map[string]string{}
	tracked := map[string]string{}
	refs := map[string]bool{}
	for line := range strings.Lines(out) {
		tag, rest, _ := strings.Cut(strings.TrimRight(line, "\r\n"), " ")
		switch tag {
		case "head":
			info.Head = rest
		case "branch":
			info.Branch = rest
		case "top":
			info.Top = rest
		case "remote":
			name, u, _ := strings.Cut(rest, " ")
			remotes = append(remotes, name)
			urls[name] = u
		case "track":
			b, r, _ := strings.Cut(rest, " ")
			tracked[b] = r
		case "ref":
			refs[rest] = true
		}
	}
	if info.Head == "" {
		return sshInfo{}, errors.New("the sandbox did not report HEAD")
	}
	remote, ok := repostats.PickRemote(remotes, func(b string) string { return tracked[b] })
	if !ok {
		return sshInfo{}, errors.New("the repo has no remote")
	}
	info.RemoteName = remote
	info.Remote = urls[remote]
	info.Main = "main"
	for _, ref := range repostats.MainRefs(remote) {
		if refs[ref] {
			info.MainRef = ref
			info.Main = ref[strings.LastIndex(ref, "/")+1:]
			break
		}
	}
	return info, nil
}

func (c *sshCheckout) info(ctx context.Context) (gitHubInfo, error) {
	d, err := c.details(ctx)
	return d.gitHubInfo, err
}

// gitHubFetchResponse is the result of fetchFromGitHub.
type gitHubFetchResponse struct {
	Repo   string `json:"repo"`
	Branch string `json:"branch"`
	// Head is the SHA of the branch on GitHub.
	Head string `json:"head"`
	// Ref is the remote-tracking ref in the checkout that now points
	// to Head, for example "refs/remotes/origin/tomhjp/fix".
	Ref string `json:"ref"`
	// Checkout is the current branch of the checkout, or "" when HEAD
	// is detached.
	Checkout string `json:"checkout"`
	// Result tells what happened to the current branch:
	//   - "fast-forward": it moved forward to Head.
	//   - "up-to-date": it was at Head already.
	//   - "ahead": it has Head and more commits.
	//   - "diverged": it and Head both have commits that the other
	//     does not have.
	//   - "other-branch": the current branch is not Branch.
	//   - "merge-failed": a fast-forward was possible, but git did
	//     not do it, for example because of changes in the working
	//     tree. Error tells why.
	Result string `json:"result"`
	Error  string `json:"error,omitempty"`
}

// fetchFromGitHub fetches branch from GitHub with the credentials of
// the desktop, and pushes it to the remote-tracking ref of branch in
// src. If the current branch of src is branch, it moves the current
// branch forward to the commit from GitHub when it can do this with no
// merge. It does not make merge commits and does not move branches
// backward.
func (s *githubSender) fetchFromGitHub(ctx context.Context, src *sshCheckout, branch string) (gitHubFetchResponse, error) {
	branch = strings.TrimSpace(branch)
	if err := checkBranchName(branch); err != nil {
		return gitHubFetchResponse{}, err
	}
	info, err := src.details(ctx)
	if err != nil {
		return gitHubFetchResponse{}, fmt.Errorf("read the checkout on the sandbox: %w", err)
	}
	repo, err := gitHubRepo(info.Remote)
	if err != nil {
		return gitHubFetchResponse{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.cacheRepo(ctx, repo)
	if err != nil {
		return gitHubFetchResponse{}, err
	}
	// Fetch from the sandbox first. Then the fetch from GitHub and the
	// push to the sandbox send only the new objects.
	if err := c.fetchCheckout(src, info.gitHubInfo); err != nil {
		return gitHubFetchResponse{}, err
	}
	if _, err := c.git("fetch", "--no-tags", "--no-write-fetch-head", s.gitHubURL(repo), "+refs/heads/"+branch+":refs/webdiff/github"); err != nil {
		return gitHubFetchResponse{}, fmt.Errorf("fetch from GitHub: %w", err)
	}
	head, err := c.git("rev-parse", "refs/webdiff/github")
	if err != nil {
		return gitHubFetchResponse{}, err
	}
	res := gitHubFetchResponse{
		Repo:     repo,
		Branch:   branch,
		Head:     head,
		Ref:      "refs/remotes/" + info.RemoteName + "/" + branch,
		Checkout: info.Branch,
	}
	// The branch on GitHub can contain amended commits, so replace the
	// remote-tracking ref, as git fetch does.
	if _, err := c.git("-c", "push.negotiate=true", "push", "--quiet", src.fetchURL(), "+"+head+":"+res.Ref); err != nil {
		return gitHubFetchResponse{}, fmt.Errorf("push to the sandbox: %w", err)
	}

	if info.Branch != branch {
		res.Result = "other-branch"
		return res, nil
	}
	isAncestor := func(a, b string) bool {
		_, err := c.git("merge-base", "--is-ancestor", a, b)
		return err == nil
	}
	switch {
	case head == info.Head:
		res.Result = "up-to-date"
	case isAncestor(head, info.Head):
		res.Result = "ahead"
	case !isAncestor(info.Head, head):
		res.Result = "diverged"
	default:
		// git does not do the merge if HEAD changed to a commit that
		// is not an ancestor of head, or if the merge changes files
		// that have changes in the working tree.
		if _, err := src.git(ctx, "merge", "--ff-only", "--quiet", head); err != nil {
			res.Result = "merge-failed"
			res.Error = err.Error()
		} else {
			res.Result = "fast-forward"
		}
	}
	return res, nil
}

// gitHubCheckoutInfo is the output of "webdiff github info".
type gitHubCheckoutInfo struct {
	Repo string `json:"repo"`
	// Branch is the current branch, or "" when HEAD is detached.
	Branch  string `json:"branch"`
	Head    string `json:"head"`
	Main    string `json:"main"`
	MainRef string `json:"mainRef"`
	// Count is the number of commits after the main branch.
	Count int `json:"count"`
	// Commits are the newest commits after the main branch, newest
	// first. There are at most maxInfoCommits.
	Commits []gitHubCommit `json:"commits"`
}

type gitHubCommit struct {
	SHA     string `json:"sha"`
	Subject string `json:"subject"`
}

const maxInfoCommits = 50

// checkoutInfo returns the gitHubCheckoutInfo of src.
func checkoutInfo(ctx context.Context, src *sshCheckout) (gitHubCheckoutInfo, error) {
	d, err := src.details(ctx)
	if err != nil {
		return gitHubCheckoutInfo{}, err
	}
	repo, err := gitHubRepo(d.Remote)
	if err != nil {
		return gitHubCheckoutInfo{}, err
	}
	res := gitHubCheckoutInfo{
		Repo:    repo,
		Branch:  d.Branch,
		Head:    d.Head,
		Main:    d.Main,
		MainRef: d.MainRef,
		Commits: []gitHubCommit{},
	}
	if d.MainRef == "" {
		return res, nil
	}
	rng := d.MainRef + ".." + d.Head
	out, err := src.run(ctx, "git rev-list --count "+shellQuote(rng)+" && git log --format=%H%x09%s -n "+strconv.Itoa(maxInfoCommits)+" "+shellQuote(rng))
	if err != nil {
		return gitHubCheckoutInfo{}, err
	}
	count, log, _ := strings.Cut(out, "\n")
	if res.Count, err = strconv.Atoi(strings.TrimSpace(count)); err != nil {
		return gitHubCheckoutInfo{}, fmt.Errorf("bad commit count %q", count)
	}
	for line := range strings.Lines(log) {
		sha, subject, _ := strings.Cut(strings.TrimRight(line, "\r\n"), "\t")
		if sha != "" {
			res.Commits = append(res.Commits, gitHubCommit{
				SHA:     sha,
				Subject: subject,
			})
		}
	}
	return res, nil
}

// defaultSendCacheDir returns the directory of the bare repos of the
// githubSender.
func defaultSendCacheDir() string {
	ucd, err := os.UserCacheDir()
	if err != nil {
		ucd = os.TempDir()
	}
	return filepath.Join(ucd, "webdiff", "send")
}

// runGitHub is "webdiff github". It does the GitHub sends of the
// desktop client for a checkout that the desktop reaches with ssh, with
// no webdiff server. The VS Code extension in vscode/ uses it. It
// writes the result as JSON on stdout.
func runGitHub(args []string) error {
	usage := func(fs *flag.FlagSet) func() {
		return func() {
			fmt.Fprintln(fs.Output(), "usage:")
			fmt.Fprintln(fs.Output(), "  webdiff github info <host>:<path>")
			fmt.Fprintln(fs.Output(), "  webdiff github send -branch <name> [-head <sha>] <host>:<path>")
			fmt.Fprintln(fs.Output(), "  webdiff github fetch -branch <name> <host>:<path>")
			fs.PrintDefaults()
		}
	}
	if len(args) == 0 {
		fs := flag.NewFlagSet("github", flag.ContinueOnError)
		usage(fs)()
		return errors.New("no command")
	}
	cmd := args[0]
	fs := flag.NewFlagSet("github "+cmd, flag.ContinueOnError)
	fs.Usage = usage(fs)
	githubURL := fs.String("github-url", "git@github.com:{repo}.git", "URL of the GitHub repo; {repo} is replaced with owner/name of the GitHub repo")
	branch := fs.String("branch", "", "name of the branch on GitHub")
	head := fs.String("head", "", "with send: the SHA that HEAD must be")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return errors.New("give one <host>:<path>")
	}
	src, err := parseSSHCheckout(fs.Arg(0))
	if err != nil {
		return err
	}
	s := &githubSender{
		urlFormat: *githubURL,
		cacheDir:  defaultSendCacheDir(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	var res any
	switch cmd {
	case "info":
		res, err = checkoutInfo(ctx, src)
	case "send":
		res, err = s.send(ctx, src, *branch, *head)
	case "fetch":
		res, err = s.fetchFromGitHub(ctx, src, *branch)
	default:
		fs.Usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(res)
}
