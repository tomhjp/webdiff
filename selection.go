package main

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/tomhjp/webdiff/internal/gitindex"
)

// lineStat counts the lines that a change adds and removes. Binary
// files do not add to the counts.
type lineStat struct {
	Ins int
	Del int
}

// addNumstat adds one line of "git diff --numstat" output to s.
func (s *lineStat) addNumstat(line string) {
	fields := strings.SplitN(line, "\t", 3)
	if len(fields) != 3 {
		return
	}
	// Binary files show "-" in place of the counts.
	if n, err := strconv.Atoi(fields[0]); err == nil {
		s.Ins += n
	}
	if n, err := strconv.Atoi(fields[1]); err == nil {
		s.Del += n
	}
}

type branchCommit struct {
	SHA     string
	Subject string
	lineStat
}

// workingTree tells about the changes in the working tree from HEAD.
type workingTree struct {
	Changed bool
	lineStat
}

// branchCommits lists first-parent commits. A merge is one selectable
// change, and its line counts are from its first parent.
func branchCommits(ctx context.Context, repo string) ([]branchCommit, error) {
	base, _ := branchBase(repo)
	// Each commit line starts with NUL, so that it is different from
	// the numstat lines that follow it.
	cmd := exec.CommandContext(ctx, "git", "-C", repo, "log", "--first-parent", "--diff-merges=first-parent", "--numstat", "--no-ext-diff", "--reverse", "--format=%x00%H %s", base+"..HEAD")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var commits []branchCommit
	for _, line := range strings.Split(string(out), "\n") {
		if header, ok := strings.CutPrefix(line, "\x00"); ok {
			sha, subject, ok := strings.Cut(header, " ")
			if ok {
				commits = append(commits, branchCommit{
					SHA:     sha,
					Subject: subject,
				})
			}
			continue
		}
		if len(commits) > 0 {
			commits[len(commits)-1].addNumstat(line)
		}
	}
	return commits, nil
}

// allChanges selects all commits and the working tree. This is the
// selection when the URL has no changes parameter.
func allChanges(commits []branchCommit) map[string]bool {
	selected := map[string]bool{"working": true}
	for _, commit := range commits {
		selected[commit.SHA] = true
	}
	return selected
}

// selectedChanges returns the changes that the URL selects. The commit
// parameter selects one commit of the branch, for the commit view. Else
// the changes parameter selects the changes, and no changes parameter
// selects all changes.
func selectedChanges(q url.Values, commits []branchCommit) (map[string]bool, error) {
	if q.Has("commit") {
		sha := q.Get("commit")
		for _, commit := range commits {
			if commit.SHA == sha {
				return map[string]bool{
					sha: true,
				}, nil
			}
		}
		return nil, fmt.Errorf("commit %q is not on this branch", sha)
	}
	if !q.Has("changes") {
		return allChanges(commits), nil
	}
	selected := make(map[string]bool)
	for _, value := range strings.Split(q.Get("changes"), ",") {
		if value != "" {
			selected[value] = true
		}
	}
	valid := map[string]bool{"working": true}
	for _, commit := range commits {
		valid[commit.SHA] = true
	}
	for value := range selected {
		if !valid[value] {
			return nil, fmt.Errorf("selected commit %q is no longer on this branch; reload without the changes parameter", value)
		}
	}
	return selected, nil
}

// selectionIndex builds one temporary index. No selected combination is cached.
// The base is the parent of the oldest selected commit, or HEAD for working changes.
//
// working tells if the working tree has changes from HEAD, and counts
// their lines. working.Changed is true when selectionIndex fails before
// it can find out, so that the caller continues to show the working
// tree as a choice.
func selectionIndex(ctx context.Context, repo string, commits []branchCommit, selected map[string]bool) (env []string, base string, working workingTree, cleanup func(), err error) {
	working.Changed = true
	dir, err := os.MkdirTemp("", "webdiff-selection-")
	if err != nil {
		return nil, "", working, func() {}, err
	}
	cleanup = func() { os.RemoveAll(dir) }
	fail := func(e error) ([]string, string, workingTree, func(), error) {
		cleanup()
		return nil, "", working, func() {}, e
	}
	cache := indexCache
	if cache == nil {
		cache = gitindex.New(filepath.Join(dir, "warm"))
	}
	warm, release, err := cache.Acquire(ctx, repo, indexKey(repo))
	if err != nil {
		return fail(err)
	}
	cleanup = func() {
		release()
		os.RemoveAll(dir)
	}
	run := func(env []string, input []byte, args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = repo
		cmd.Env = env
		cmd.Stdin = bytes.NewReader(input)
		out, err := cmd.Output()
		if err != nil {
			var detail []byte
			if exit, ok := err.(*exec.ExitError); ok {
				detail = exit.Stderr
			}
			return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, detail)
		}
		return out, nil
	}
	headOut, err := run(warm, nil, "rev-parse", "HEAD")
	if err != nil {
		return fail(err)
	}
	head := strings.TrimSpace(string(headOut))
	base = head
	var chosen []branchCommit
	for _, commit := range commits {
		if selected[commit.SHA] {
			chosen = append(chosen, commit)
		}
	}
	if len(chosen) > 0 {
		out, e := run(warm, nil, "rev-parse", chosen[0].SHA+"^1")
		if e != nil {
			return fail(e)
		}
		base = strings.TrimSpace(string(out))
	}
	// numstat shows a line for each changed file, also when the change
	// has no added or removed lines (for example, a mode change).
	numstat, err := run(warm, nil, "diff", "--cached", "--numstat", "--no-ext-diff", head)
	if err != nil {
		return fail(err)
	}
	working = workingTree{
		Changed: len(bytes.TrimSpace(numstat)) > 0,
	}
	for _, line := range strings.Split(string(numstat), "\n") {
		working.addNumstat(line)
	}
	var workingPatch []byte
	if selected["working"] && working.Changed {
		workingPatch, err = run(warm, nil, "diff", "--cached", "--binary", "--full-index", "--no-ext-diff", "--no-color", head)
		if err != nil {
			return fail(err)
		}
	}
	// Keep new objects temporary, but permit reads from the warm object cache.
	var alternates string
	for _, value := range warm {
		if strings.HasPrefix(value, "GIT_OBJECT_DIRECTORY=") {
			alternates = strings.TrimPrefix(value, "GIT_OBJECT_DIRECTORY=")
		}
	}
	env = make([]string, 0, len(warm))
	for _, value := range warm {
		if strings.HasPrefix(value, "GIT_ALTERNATE_OBJECT_DIRECTORIES=") {
			alternates += string(os.PathListSeparator) + strings.TrimPrefix(value, "GIT_ALTERNATE_OBJECT_DIRECTORIES=")
		}
		if !strings.HasPrefix(value, "GIT_INDEX_FILE=") && !strings.HasPrefix(value, "GIT_OBJECT_DIRECTORY=") && !strings.HasPrefix(value, "GIT_ALTERNATE_OBJECT_DIRECTORIES=") {
			env = append(env, value)
		}
	}
	objects := filepath.Join(dir, "objects")
	if err := os.Mkdir(objects, 0700); err != nil {
		return fail(err)
	}
	env = append(env, "GIT_INDEX_FILE="+filepath.Join(dir, "index"), "GIT_OBJECT_DIRECTORY="+objects, "GIT_ALTERNATE_OBJECT_DIRECTORIES="+alternates)
	if _, err := run(env, nil, "read-tree", base); err != nil {
		return fail(err)
	}
	apply := func(patch []byte, label string) error {
		if len(patch) == 0 {
			return nil
		}
		if _, err := run(env, patch, "apply", "--cached", "--3way", "--whitespace=nowarn"); err != nil {
			return fmt.Errorf("cannot combine %s with the selected changes; select its prerequisite commits: %w", label, err)
		}
		return nil
	}
	for _, commit := range chosen {
		patch, e := run(env, nil, "diff", "--binary", "--full-index", "--no-ext-diff", "--no-color", commit.SHA+"^1", commit.SHA)
		if e != nil {
			return fail(e)
		}
		if e := apply(patch, commit.SHA[:12]); e != nil {
			return fail(e)
		}
	}
	if err := apply(workingPatch, "working tree"); err != nil {
		return fail(err)
	}
	return env, base, working, cleanup, nil
}

// commitMessage is the id, author, date, and full message of a commit.
type commitMessage struct {
	SHA    string
	Author string
	Date   string
	Body   string
}

// readCommitMessages returns the messages of the commits shas in repo,
// in the order of shas.
func readCommitMessages(ctx context.Context, repo string, shas []string) ([]commitMessage, error) {
	// Each record ends with the record separator 0x1e, and the fields
	// are separated by NUL. --no-walk=unsorted keeps the order of shas.
	args := []string{"-C", repo, "log", "--no-walk=unsorted", "--date=format:%Y-%m-%d %H:%M", "--format=%H%x00%an%x00%ad%x00%B%x1e"}
	args = append(args, shas...)
	out, err := exec.CommandContext(ctx, "git", args...).Output()
	if err != nil {
		return nil, err
	}
	var msgs []commitMessage
	for _, record := range strings.Split(string(out), "\x1e") {
		record = strings.TrimLeft(record, "\n")
		if record == "" {
			continue
		}
		fields := strings.SplitN(record, "\x00", 4)
		if len(fields) != 4 {
			return nil, fmt.Errorf("git log: bad output")
		}
		msgs = append(msgs, commitMessage{
			SHA:    fields[0],
			Author: fields[1],
			Date:   fields[2],
			Body:   strings.TrimRight(fields[3], "\n"),
		})
	}
	return msgs, nil
}

// commitMessagesID is the id of the "commit messages" section of the
// diff page. The pill of the section links to it.
const commitMessagesID = "commit-messages"

// writeCommitMessages writes the "commit messages" section of the diff
// page. It has the same form as the section of a file, so that it
// opens, closes, and has a pill as a file does. It has no data-file,
// so it does not get review comments. It shows the id, author, date,
// and full message of each commit in msgs. The id links to the commit
// view of the commit. refURL and n are the checkout and the session
// number of the page.
//
// If edit is not nil, each message has an edit button (see
// handleCommitMessage), and if edit.Working is true, the working tree
// has an entry after the commits, with no message.
func writeCommitMessages(body *strings.Builder, msgs []commitMessage, refURL string, n int, edit *commitEdit) {
	noun := "commits"
	if len(msgs) == 1 {
		noun = "commit"
	}
	fmt.Fprintf(body, `<details id="%s" open><summary><span class="filename">commit messages</span><span class="filestats"><span class="stats-files">%d %s</span></span></summary>`,
		commitMessagesID, len(msgs), noun)
	if edit != nil {
		fmt.Fprintf(body, `<div class="commit-messages" data-kind="%s" data-name="%s">`,
			template.HTMLEscapeString(edit.Kind), template.HTMLEscapeString(edit.Name))
	} else {
		body.WriteString(`<div class="commit-messages">`)
	}
	for _, m := range msgs {
		body.WriteString(`<div class="commit-view">`)
		fmt.Fprintf(body, `<div class="commit-meta"><a class="commit-sha" href="%s" title="view commit %s">%s</a><span>%s</span><span>%s</span>`,
			template.HTMLEscapeString(commitViewURL(refURL, n, m.SHA)),
			template.HTMLEscapeString(m.SHA),
			template.HTMLEscapeString(m.SHA[:min(12, len(m.SHA))]),
			template.HTMLEscapeString(m.Author),
			template.HTMLEscapeString(m.Date))
		if edit != nil {
			fmt.Fprintf(body, `<button class="toggle commit-edit" type="button" data-commit="%s" title="edit the message; the commits after it get new ids">edit</button>`,
				template.HTMLEscapeString(m.SHA))
		}
		body.WriteString(`</div>`)
		fmt.Fprintf(body, `<pre class="commit-message">%s</pre>`, template.HTMLEscapeString(m.Body))
		body.WriteString(`</div>`)
	}
	if edit != nil && edit.Working {
		body.WriteString(`<div class="commit-view commit-view-working">`)
		body.WriteString(`<div class="commit-meta"><span class="commit-sha">working tree</span><span>not committed</span>`)
		body.WriteString(`<button class="toggle commit-edit" type="button" data-commit="working" title="write a message and commit the working tree">edit</button></div>`)
		body.WriteString(`<pre class="commit-message"></pre>`)
		body.WriteString(`</div>`)
	}
	body.WriteString(`</div></details>`)
}

// commitEdit tells writeCommitMessages to add the controls that edit
// the commit messages of the checkout Kind/Name.
type commitEdit struct {
	Kind string
	Name string
	// Working tells to add an entry for the working tree.
	Working bool
}

// writeSendToGitHubButton writes the "send to github" button of the
// diff page. The desktop client does the send (see githubSender). The
// button sends the commits of the branch, up to HEAD. It does not send
// the working tree. commits are the commits of the branch, the oldest
// first, and working tells about the working tree.
func writeSendToGitHubButton(body *strings.Builder, ref repoRef, commits []branchCommit, working workingTree) {
	remote, _ := defaultRemote(ref.abs)
	main := mainBranch(ref.abs, remote)
	branch := currentBranch(ref.abs)
	if branch == "HEAD" || branch == "unknown" || branch == main {
		branch = ""
	}
	head := ""
	if len(commits) > 0 {
		head = commits[len(commits)-1].SHA
	}
	attrs := ""
	title := "push the commits of this branch to a branch on GitHub"
	if head == "" {
		attrs = " disabled"
		title = "the branch has no commits to send"
	}
	if working.Changed {
		attrs += ` data-dirty="1"`
	}
	fmt.Fprintf(body, `<button class="toggle" type="button" id="send-github" data-kind="%s" data-name="%s" data-branch="%s" data-main="%s" data-head="%s" data-commits="%d" title="%s"%s>send to github</button>`,
		template.HTMLEscapeString(ref.kind),
		template.HTMLEscapeString(ref.name),
		template.HTMLEscapeString(branch),
		template.HTMLEscapeString(main),
		template.HTMLEscapeString(head),
		len(commits),
		template.HTMLEscapeString(title),
		attrs)
}

// commitViewURL returns the URL of the commit view of commit sha in the
// checkout at refURL. The URL keeps session number n, so the comments
// of the commit view go to the same session.
func commitViewURL(refURL string, n int, sha string) string {
	v := url.Values{}
	if n > 1 {
		v.Set("n", strconv.Itoa(n))
	}
	v.Set("commit", sha)
	return refURL + "?" + v.Encode()
}

// writeDiffPicker writes the "changes" menu. working tells if the
// working tree has changes. When it has none, the working tree choice
// is disabled and the count does not include it.
//
// The choices are in the order of the changes: the oldest commit
// first, then the newer commits, and the working tree last. The text of
// each commit is a link to its commit view. refURL and n are the
// checkout and the session number of the page. current is the commit
// that the commit view shows, or "".
//
// reset tells that the selection in the URL could not be shown, and
// that the page shows all changes instead. Then app.js removes the
// saved selection and the changes parameter.
func writeDiffPicker(body *strings.Builder, commits []branchCommit, selected map[string]bool, working workingTree, reset bool, refURL string, n int, current string) {
	count := len(selected)
	if selected["working"] && !working.Changed {
		count--
	}
	resetAttr := ""
	if reset {
		resetAttr = ` data-reset="1"`
	}
	// data-commit tells app.js that the page is the commit view. Then
	// app.js does not save the selection of the page and does not
	// change the URL.
	if current != "" {
		resetAttr += ` data-commit="1"`
	}
	fmt.Fprintf(body, `<div class="diff-picker" id="diff-picker"%s>`, resetAttr)
	fmt.Fprintf(body, `<button class="toggle" type="button" id="diff-picker-toggle" aria-expanded="false" aria-controls="diff-picker-menu">changes (%d)</button><div class="diff-picker-menu" id="diff-picker-menu" hidden>`, count)
	// href is "" for the working tree, which has no commit view.
	writeChoice := func(value, label, href string, stat lineStat, enabled bool) {
		attrs := ""
		if selected[value] && enabled {
			attrs = " checked"
		}
		if !enabled {
			// app.js does not read or save disabled choices, so the
			// saved selection keeps the working tree when it has
			// changes again.
			attrs += " disabled"
			label += " (no changes)"
		}
		cls := ""
		if value == current {
			cls = ` class="diff-picker-current"`
		}
		fmt.Fprintf(body, `<label%s><input type="checkbox" value="%s"%s> `, cls, template.HTMLEscapeString(value), attrs)
		// A click on a link in a <label> follows the link and does
		// not change the checkbox.
		if href != "" {
			fmt.Fprintf(body, `<a class="diff-picker-label" href="%s" title="view this commit">%s</a>`, template.HTMLEscapeString(href), template.HTMLEscapeString(label))
		} else {
			fmt.Fprintf(body, `<span class="diff-picker-label">%s</span>`, template.HTMLEscapeString(label))
		}
		writeLineStat(body, stat)
		body.WriteString(`</label>`)
	}
	for _, commit := range commits {
		writeChoice(commit.SHA, commit.SHA[:12]+" "+commit.Subject, commitViewURL(refURL, n, commit.SHA), commit.lineStat, true)
	}
	writeChoice("working", "working tree", "", working.lineStat, working.Changed)
	body.WriteString(`<div class="header-buttons"><button class="toggle" type="button" id="diff-picker-all">all</button><button class="toggle" type="button" id="diff-picker-none">none</button><button class="toggle send-btn" type="button" id="diff-picker-apply">apply</button></div></div></div>`)
}

// writeLineStat writes the line counts of one change. The counts are in
// data-stat and not in a text node; see .stats-ins in style.css.
func writeLineStat(body *strings.Builder, stat lineStat) {
	if stat.Ins == 0 && stat.Del == 0 {
		return
	}
	body.WriteString(`<span class="diff-picker-stat">`)
	if stat.Ins > 0 {
		fmt.Fprintf(body, `<span class="stats-ins" data-stat="+%d"></span>`, stat.Ins)
	}
	if stat.Del > 0 {
		fmt.Fprintf(body, `<span class="stats-del" data-stat="-%d"></span>`, stat.Del)
	}
	body.WriteString(`</span>`)
}
