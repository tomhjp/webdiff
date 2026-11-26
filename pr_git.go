package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The git poll finds the pull requests of the repos that maintner does
// not watch. GitHub shows the head of each pull request N as the ref
// refs/pull/N/head, and the test merge of an open pull request as
// refs/pull/N/merge. One git ls-remote for each repo gets those refs
// and the branches. The pull request of a branch is the one whose head
// is the commit of the branch on GitHub or in the checkout.
//
// The poll is adaptive. It asks about a repo each prConfig.GitPoll
// while one of its branches has a commit from the last prActiveWindow,
// and else each prConfig.GitPollIdle.
//
// The poll asks public GitHub first. For a private repo, public GitHub
// refuses, and the poll asks the git proxy (see prConfig.GitProxy).
//
// Git does not tell the title or the review state of a pull request. A
// pull request without a merge ref is closed or merged, or it has
// conflicts, thus its state is not known.

// gitQuery is one branch to find in a git poll.
type gitQuery struct {
	Branch string
	// Dir is the checkout of the branch, or "".
	Dir string
}

// gitLsRemoteTimeout is the maximum time of one git ls-remote.
const gitLsRemoteTimeout = time.Minute

// prActiveWindow is how long a repo is active after a commit on one of
// its branches. The git poll asks about an active repo more frequently.
const prActiveWindow = 3 * time.Hour

// gitPollLoop polls git until ctx is done. It wakes each t.gitPoll, and
// asks about each repo that is due (see gitPollDue).
func (t *prTracker) gitPollLoop(ctx context.Context) {
	ticker := time.NewTicker(t.gitPoll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		t.pollGit(ctx, false)
	}
}

// gitPollInterval returns the poll interval of a repo whose newest
// commit on a tracked branch is at lastCommit.
func (t *prTracker) gitPollInterval(lastCommit, now time.Time) time.Duration {
	if now.Sub(lastCommit) < prActiveWindow {
		return t.gitPoll
	}
	return t.gitPollIdle
}

// gitPollDue reports whether a repo that the poll last asked about at
// lastPoll is due at now. The ticker does not fire at exact times, thus a poll
// that is due a short time after now is due now.
func (t *prTracker) gitPollDue(lastPoll, lastCommit, now time.Time) bool {
	interval := t.gitPollInterval(lastCommit, now)
	return now.Sub(lastPoll) >= interval-t.gitPoll/10
}

// pollGit refreshes the entries that come from git, with one git
// ls-remote for each repo that is due, or for each repo if all is true.
func (t *prTracker) pollGit(ctx context.Context, all bool) {
	t.mu.Lock()
	byRepo := make(map[string][]prKey)
	dirs := make(map[prKey]string)
	for k, e := range t.entries {
		if t.viaGitLocked(k.Repo) {
			byRepo[k.Repo] = append(byRepo[k.Repo], k)
			dirs[k] = e.dir
		}
	}
	polled := maps.Clone(t.gitPolled)
	t.mu.Unlock()

	now := time.Now()
	for repo, keys := range byRepo {
		if !all {
			var lastCommit time.Time
			for _, k := range keys {
				if c := localCommitTime(dirs[k], k.Branch); c.After(lastCommit) {
					lastCommit = c
				}
			}
			if !t.gitPollDue(polled[repo], lastCommit, now) {
				continue
			}
		}
		queries := make([]gitQuery, len(keys))
		for i, k := range keys {
			queries[i] = gitQuery{
				Branch: k.Branch,
				Dir:    dirs[k],
			}
		}
		found, err := t.fetchGit(ctx, repo, queries)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("pull requests of %s: %v", repo, err)
			continue
		}
		t.mu.Lock()
		changed := false
		for i, k := range keys {
			if e, ok := t.entries[k]; ok && !samePR(e.pr, found[i]) {
				e.pr = found[i]
				changed = true
			}
		}
		if changed {
			t.signalLocked()
		}
		t.mu.Unlock()
	}
}

// fetchGit finds the pull request of each query in the GitHub repo
// ("owner/name"). The result has the order of queries, with nil for a
// branch with no pull request.
func (t *prTracker) fetchGit(ctx context.Context, repo string, queries []gitQuery) ([]*pullRequest, error) {
	var branches []string
	for _, q := range queries {
		if !slices.Contains(branches, q.Branch) {
			branches = append(branches, q.Branch)
		}
	}
	refs, err := t.lsRemote(ctx, repo, branches)
	if err != nil {
		return nil, err
	}

	// A new branch from main has the commit of main. A pull request
	// that has that commit as its head is not the pull request of the
	// branch.
	mainSHAs := make(map[string]bool)
	for _, name := range []string{"main", "master"} {
		if sha := refs["refs/heads/"+name]; sha != "" {
			mainSHAs[sha] = true
		}
	}
	heads := make(map[string][]int32) // commit → pull request numbers
	open := make(map[int32]bool)
	for ref, sha := range refs {
		n, kind, ok := parsePullRef(ref)
		if !ok {
			continue
		}
		switch kind {
		case "head":
			heads[sha] = append(heads[sha], n)
		case "merge":
			open[n] = true
		}
	}

	out := make([]*pullRequest, len(queries))
	for i, q := range queries {
		var shas []string
		for _, sha := range []string{refs["refs/heads/"+q.Branch], localBranchSHA(q.Dir, q.Branch)} {
			if sha != "" && !mainSHAs[sha] {
				shas = append(shas, sha)
			}
		}
		// Use the newest open pull request, or else the newest one.
		var best int32
		for _, sha := range shas {
			for _, n := range heads[sha] {
				if best == 0 || (open[n] && !open[best]) || (open[n] == open[best] && n > best) {
					best = n
				}
			}
		}
		if best == 0 {
			continue
		}
		pr := &pullRequest{
			Repo:          repo,
			Number:        best,
			ReviewUnknown: true,
		}
		if open[best] {
			pr.State = "OPEN"
		}
		out[i] = pr
	}
	return out, nil
}

// parsePullRef parses "refs/pull/<n>/<kind>".
func parsePullRef(ref string) (n int32, kind string, ok bool) {
	rest, ok := strings.CutPrefix(ref, "refs/pull/")
	if !ok {
		return 0, "", false
	}
	num, kind, ok := strings.Cut(rest, "/")
	if !ok {
		return 0, "", false
	}
	v, err := strconv.ParseInt(num, 10, 32)
	if err != nil || v <= 0 {
		return 0, "", false
	}
	return int32(v), kind, true
}

// localBranchSHA returns the commit of branch in the checkout at dir,
// or "".
func localBranchSHA(dir, branch string) string {
	if dir == "" {
		return ""
	}
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// localCommitTime returns the committer date of the newest commit of
// branch in the checkout at dir, or the zero time.
func localCommitTime(dir, branch string) time.Time {
	if dir == "" {
		return time.Time{}
	}
	out, err := exec.Command("git", "-C", dir, "log", "-1", "--format=%ct", "refs/heads/"+branch, "--").Output()
	if err != nil {
		return time.Time{}
	}
	sec, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(sec, 0)
}

// lsRemote returns the commit of each pull request ref, main branch and
// branch of repo on GitHub, keyed by the full ref name. It uses the git
// URL that answered the last time. If there is none, or if it fails, it
// tries each of t.gitBases.
func (t *prTracker) lsRemote(ctx context.Context, repo string, branches []string) (map[string]string, error) {
	t.mu.Lock()
	last := t.gitURLs[repo]
	t.mu.Unlock()
	var urls []string
	if last != "" {
		urls = append(urls, last)
	}
	for _, base := range t.gitBases {
		if u := base + "/" + repo + ".git"; u != last {
			urls = append(urls, u)
		}
	}
	var errs []string
	for _, u := range urls {
		refs, err := gitLsRemote(ctx, u, branches)
		if err == nil {
			t.mu.Lock()
			t.gitURLs[repo] = u
			t.gitPolled[repo] = time.Now()
			t.mu.Unlock()
			return refs, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		errs = append(errs, err.Error())
	}
	// A repo that fails has the same poll interval as one that
	// answers, so that the poll does not ask about it more frequently.
	t.mu.Lock()
	t.gitPolled[repo] = time.Now()
	t.mu.Unlock()
	return nil, fmt.Errorf("git ls-remote: %s", strings.Join(errs, "; "))
}

// gitLsRemote runs git ls-remote on url for the pull request refs, the
// main branches and branches.
func gitLsRemote(ctx context.Context, url string, branches []string) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitLsRemoteTimeout)
	defer cancel()
	args := []string{
		// Use no credentials: the poll asks public GitHub, and the
		// proxy needs none. Without this, a private repo can cause a
		// prompt or use the credentials of the user.
		"-c", "credential.helper=",
		"ls-remote", url,
		"refs/pull/*/head", "refs/pull/*/merge",
		"refs/heads/main", "refs/heads/master",
	}
	for _, b := range branches {
		args = append(args, "refs/heads/"+b)
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=true", "SSH_ASKPASS=true")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s: %v: %s", url, err, strings.TrimSpace(stderr.String()))
	}
	refs := make(map[string]string)
	for line := range strings.Lines(string(out)) {
		sha, ref, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if ok {
			refs[ref] = sha
		}
	}
	return refs, nil
}
