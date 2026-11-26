package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// commitEditMu makes sure that only one commit edit changes a repo at a
// time. Edits come from a person, so one lock for all repos is
// sufficient.
var commitEditMu sync.Mutex

// handleCommitMessage changes the message of a commit of the branch of
// a checkout, or commits the working tree with a new message.
//
// The request is {"kind", "name", "commit", "message"}. commit is the
// SHA of a commit that the diff page shows, or "working" for the
// working tree. The response is {"rewritten": {old: new}}. It has an
// entry for each commit that has a new SHA, and for "working" it has
// the SHA of the new commit.
func handleCommitMessage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed", nil)
		return
	}
	// A JSON body makes a cross-site request send a CORS preflight
	// request first, which this server does not accept.
	if !isJSONRequest(r) {
		writeJSONError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json", nil)
		return
	}
	var req struct {
		Kind    string `json:"kind"`
		Name    string `json:"name"`
		Commit  string `json:"commit"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad JSON: "+err.Error(), nil)
		return
	}
	ref, ok := resolveRepoRef("/" + req.Kind + "/" + req.Name + "/")
	if !ok || !isGitRepo(ref.abs) {
		writeJSONError(w, http.StatusNotFound, "unknown repo", nil)
		return
	}
	msg, err := cleanCommitMessage(req.Message)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error(), nil)
		return
	}

	commitEditMu.Lock()
	defer commitEditMu.Unlock()
	var rewritten map[string]string
	if req.Commit == "working" {
		var sha string
		sha, err = commitWorkingTree(r.Context(), ref.abs, msg)
		if err == nil {
			rewritten = map[string]string{
				"working": sha,
			}
		}
	} else {
		rewritten, err = rewordCommit(r.Context(), ref.abs, req.Commit, msg)
	}
	if err != nil {
		writeJSONError(w, http.StatusConflict, err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"rewritten": rewritten,
	})
}

// isJSONRequest reports whether the body of r is JSON.
func isJSONRequest(r *http.Request) bool {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && mt == "application/json"
}

// cleanCommitMessage removes the blank lines at the start and end of
// msg and the spaces at the ends of its lines, and adds a final
// newline. A message with no text is an error.
func cleanCommitMessage(msg string) (string, error) {
	lines := strings.Split(strings.ReplaceAll(msg, "\r\n", "\n"), "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " \t\r")
	}
	out := strings.Trim(strings.Join(lines, "\n"), "\n")
	if out == "" {
		return "", errors.New("the commit message is empty")
	}
	return out + "\n", nil
}

// gitRun runs git in repo with stdin as its input and env added to the
// environment. It returns the output without the final newline. The
// error has the standard error output of git.
func gitRun(ctx context.Context, repo, stdin string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", repo}, args...)...)
	cmd.Stdin = strings.NewReader(stdin)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimRight(string(out), "\n"), nil
}

// rewordCommit gives commit sha of the branch of repo the message msg.
// It then makes the commits after sha again on top of the new commit,
// with the same trees and messages, and moves the branch (or a
// detached HEAD) to the new last commit. Only commit objects change,
// so the index and the working tree stay the same, and there can be no
// conflicts.
//
// sha must be one of the commits that branchCommits returns. The
// returned map has the new SHA of each commit that changes.
func rewordCommit(ctx context.Context, repo, sha, msg string) (map[string]string, error) {
	git := func(stdin string, env []string, args ...string) (string, error) {
		return gitRun(ctx, repo, stdin, env, args...)
	}
	head, err := git("", nil, "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	commits, err := branchCommits(ctx, repo)
	if err != nil {
		return nil, err
	}
	i := -1
	for j, c := range commits {
		if c.SHA == sha {
			i = j
			break
		}
	}
	if i < 0 {
		return nil, fmt.Errorf("commit %q is not on this branch; reload the page", sha)
	}
	chain := commits[i:]
	if chain[len(chain)-1].SHA != head {
		return nil, errors.New("HEAD changed; reload the page")
	}

	// A branch that was merged after sha can contain sha. Only the
	// first-parent commits are made again, so that branch would keep
	// the old commit.
	anc, err := git("", nil, "rev-list", "--ancestry-path", sha+"..HEAD")
	if err != nil {
		return nil, err
	}
	onChain := make(map[string]bool, len(chain))
	for _, c := range chain[1:] {
		onChain[c.SHA] = true
	}
	for _, c := range strings.Fields(anc) {
		if !onChain[c] {
			return nil, fmt.Errorf("commit %s of a merged branch comes after %s; edit the message with git", c[:min(12, len(c))], sha[:min(12, len(sha))])
		}
	}

	rewritten := make(map[string]string, len(chain))
	prev := ""
	for j, c := range chain {
		raw, err := git("", nil, "cat-file", "commit", c.SHA)
		if err != nil {
			return nil, err
		}
		header, body, _ := strings.Cut(raw, "\n\n")
		if strings.Contains("\n"+header, "\nencoding ") {
			return nil, fmt.Errorf("commit %s has an encoding header; edit the message with git", c.SHA[:12])
		}
		info, err := git("", nil, "log", "-1", "--date=raw", "--format=%T%x00%P%x00%an%x00%ae%x00%ad", c.SHA)
		if err != nil {
			return nil, err
		}
		f := strings.Split(info, "\x00")
		if len(f) != 5 {
			return nil, fmt.Errorf("git log %s: bad output", c.SHA)
		}
		parents := strings.Fields(f[1])
		message := body + "\n"
		if j == 0 {
			if message == msg {
				// The message does not change, so no commit changes.
				return map[string]string{}, nil
			}
			message = msg
		} else {
			parents[0] = prev
		}
		args := []string{"commit-tree", f[0]}
		for _, p := range parents {
			args = append(args, "-p", p)
		}
		args = append(args, "-F", "-")
		env := []string{
			"GIT_AUTHOR_NAME=" + f[2],
			"GIT_AUTHOR_EMAIL=" + f[3],
			"GIT_AUTHOR_DATE=" + f[4],
		}
		next, err := git(message, env, args...)
		if err != nil {
			return nil, err
		}
		rewritten[c.SHA] = next
		prev = next
	}

	// The update fails if HEAD moved since it was read.
	reflog := "webdiff: reword " + sha[:min(12, len(sha))]
	if branch, err := git("", nil, "symbolic-ref", "-q", "HEAD"); err == nil {
		_, err = git("", nil, "update-ref", "-m", reflog, branch, prev, head)
		if err != nil {
			return nil, err
		}
	} else {
		_, err = git("", nil, "update-ref", "--no-deref", "-m", reflog, "HEAD", prev, head)
		if err != nil {
			return nil, err
		}
	}
	return rewritten, nil
}

// commitWorkingTree commits all the changes in the working tree of
// repo, including the files that are not tracked and not ignored, with
// the message msg and a Signed-off-by trailer. It returns the SHA of
// the new commit.
func commitWorkingTree(ctx context.Context, repo, msg string) (string, error) {
	if _, err := gitRun(ctx, repo, "", nil, "add", "-A"); err != nil {
		return "", err
	}
	if _, err := gitRun(ctx, repo, msg, nil, "commit", "-q", "-s", "-F", "-"); err != nil {
		return "", err
	}
	return gitRun(ctx, repo, "", nil, "rev-parse", "HEAD")
}
