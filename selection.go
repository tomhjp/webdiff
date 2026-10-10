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
	"strings"

	"github.com/tomhjp/webdiff/internal/gitindex"
)

type branchCommit struct {
	SHA     string
	Subject string
}

// branchCommits lists first-parent commits. A merge is one selectable change.
func branchCommits(ctx context.Context, repo string) ([]branchCommit, error) {
	base, _ := branchBase(repo)
	cmd := exec.CommandContext(ctx, "git", "-C", repo, "log", "--first-parent", "--reverse", "--format=%H %s", base+"..HEAD")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var commits []branchCommit
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		sha, subject, ok := strings.Cut(line, " ")
		if ok {
			commits = append(commits, branchCommit{
				SHA:     sha,
				Subject: subject,
			})
		}
	}
	return commits, nil
}

func selectedChanges(q url.Values, commits []branchCommit) (map[string]bool, error) {
	selected := make(map[string]bool)
	if q.Has("changes") {
		for _, value := range strings.Split(q.Get("changes"), ",") {
			if value != "" {
				selected[value] = true
			}
		}
	} else {
		for _, commit := range commits {
			selected[commit.SHA] = true
		}
		selected["working"] = true
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
func selectionIndex(ctx context.Context, repo string, commits []branchCommit, selected map[string]bool) (env []string, base string, cleanup func(), err error) {
	dir, err := os.MkdirTemp("", "webdiff-selection-")
	if err != nil {
		return nil, "", func() {}, err
	}
	cleanup = func() { os.RemoveAll(dir) }
	fail := func(e error) ([]string, string, func(), error) {
		cleanup()
		return nil, "", func() {}, e
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
	var workingPatch []byte
	if selected["working"] {
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
	return env, base, cleanup, nil
}

func writeDiffPicker(body *strings.Builder, commits []branchCommit, selected map[string]bool) {
	fmt.Fprintf(body, `<div class="diff-picker" id="diff-picker"><button class="toggle" type="button" id="diff-picker-toggle" aria-expanded="false" aria-controls="diff-picker-menu">changes (%d)</button><div class="diff-picker-menu" id="diff-picker-menu" hidden>`, len(selected))
	writeChoice := func(value, label string) {
		checked := ""
		if selected[value] {
			checked = " checked"
		}
		fmt.Fprintf(body, `<label><input type="checkbox" value="%s"%s> %s</label>`, template.HTMLEscapeString(value), checked, template.HTMLEscapeString(label))
	}
	writeChoice("working", "working tree")
	for i := len(commits) - 1; i >= 0; i-- {
		commit := commits[i]
		writeChoice(commit.SHA, commit.SHA[:12]+" "+commit.Subject)
	}
	body.WriteString(`<div class="header-buttons"><button class="toggle" type="button" id="diff-picker-all">all</button><button class="toggle" type="button" id="diff-picker-none">none</button><button class="toggle send-btn" type="button" id="diff-picker-apply">apply</button></div></div></div>`)
}
