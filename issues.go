package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/tomhjp/webdiff/internal/repostats"
)

// The home page shows the GitHub issues of each branch on the line of
// the branch. They come from two sources:
//
//   - The commits of the branch that are not on main. A line of a commit
//     message such as "Updates #123" or "Fixes tailscale/corp#45" names
//     issues.
//   - A list that the user writes on the home page. The user can write
//     the list when the commits name no issue. branchIssues keeps it.

// issueRef identifies a GitHub issue or pull request.
type issueRef struct {
	// Repo is the "owner/name" of the GitHub repo. It is "" for a
	// reference such as "#123" that is relative to a repo that is not
	// known yet (see resolve).
	Repo   string
	Number int
}

// String returns the full form of r, for example "tailscale/corp#123".
func (r issueRef) String() string {
	return r.Repo + "#" + strconv.Itoa(r.Number)
}

// URL returns the GitHub page of r. GitHub sends the issue URL of a pull
// request to the pull request.
func (r issueRef) URL() string {
	return "https://github.com/" + r.Repo + "/issues/" + strconv.Itoa(r.Number)
}

// issueAliases are short names for the main repos.
var issueAliases = map[string]string{
	"corp": "tailscale/corp",
	"oss":  "tailscale/tailscale",
}

// label returns the text that shows r on a line about the repo base:
// "#123" for an issue of base, "corp#123" or "oss#123" for an issue of
// a repo in issueAliases, or else "owner/name#123". base is "" when the
// line is about more than one repo.
func (r issueRef) label(base string) string {
	n := "#" + strconv.Itoa(r.Number)
	if r.Repo == base && base != "" {
		return n
	}
	for alias, repo := range issueAliases {
		if r.Repo == repo {
			return alias + n
		}
	}
	return r.Repo + n
}

// resolve returns r with base as its repo if r is relative. It returns
// false if r is relative and base is "".
func (r issueRef) resolve(base string) (issueRef, bool) {
	if r.Repo != "" {
		return r, true
	}
	if base == "" {
		return issueRef{}, false
	}
	r.Repo = base
	return r, true
}

var repoPartRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// parseIssueToken parses one reference: "#123", "123", "corp#123",
// "oss#123", "owner/name#123", or the URL of a GitHub issue or pull
// request. A reference with no repo has Repo "".
func parseIssueToken(tok string) (issueRef, bool) {
	tok = strings.TrimRight(tok, ".,:;)!")
	tok = strings.TrimLeft(tok, "(")
	if rest, ok := strings.CutPrefix(tok, "https://github.com/"); ok {
		parts := strings.Split(strings.Trim(rest, "/"), "/")
		if len(parts) != 4 || (parts[2] != "issues" && parts[2] != "pull") {
			return issueRef{}, false
		}
		ref, ok := parseIssueToken(parts[0] + "/" + parts[1] + "#" + parts[3])
		return ref, ok
	}
	repo, num, hasHash := strings.Cut(tok, "#")
	if !hasHash {
		repo, num = "", tok
	}
	n, err := strconv.Atoi(num)
	if err != nil || n <= 0 || strings.HasPrefix(num, "+") {
		return issueRef{}, false
	}
	switch {
	case repo == "":
	case issueAliases[repo] != "":
		repo = issueAliases[repo]
	default:
		owner, name, ok := strings.Cut(repo, "/")
		if !ok || !repoPartRE.MatchString(owner) || !repoPartRE.MatchString(name) {
			return issueRef{}, false
		}
	}
	return issueRef{
		Repo:   repo,
		Number: n,
	}, true
}

// issueSeparators splits a list of references.
var issueSeparators = regexp.MustCompile(`[\s,;]+`)

// parseIssueList parses the list that a user writes, with base as the
// repo of a reference with no repo. It returns an error for a word that
// is not a reference.
func parseIssueList(s, base string) ([]issueRef, error) {
	var out []issueRef
	for _, tok := range issueSeparators.Split(strings.TrimSpace(s), -1) {
		if tok == "" {
			continue
		}
		ref, ok := parseIssueToken(tok)
		if !ok {
			return nil, fmt.Errorf("%q is not an issue number", tok)
		}
		ref, ok = ref.resolve(base)
		if !ok {
			return nil, fmt.Errorf("%q has no repo; write it as owner/name#N", tok)
		}
		if !slices.Contains(out, ref) {
			out = append(out, ref)
		}
	}
	return out, nil
}

// issueLineRE matches a line of a commit message that names issues, for
// example "Updates #123" or "Fixes: tailscale/corp#45, #46".
var issueLineRE = regexp.MustCompile(`(?i)^\s*(?:updates?|fix(?:es|ed)?|close[sd]?|resolve[sd]?|refs?|see|related to|part of)\s*:?\s+(.+)$`)

// commitIssueRefs returns the issues that the lines of the commit
// message msg name. A reference with no repo has Repo "".
func commitIssueRefs(msg string) []issueRef {
	var out []issueRef
	for line := range strings.Lines(msg) {
		m := issueLineRE.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		for _, tok := range issueSeparators.Split(m[1], -1) {
			// A bare number is not a reference in a commit message.
			if !strings.Contains(tok, "#") && !strings.HasPrefix(tok, "https://") {
				continue
			}
			if ref, ok := parseIssueToken(tok); ok && !slices.Contains(out, ref) {
				out = append(out, ref)
			}
		}
	}
	return out
}

// commitIssues keeps the issues of each commit, by commit SHA. A commit
// message does not change, thus the cache is always correct.
var commitIssues = struct {
	sync.Mutex
	m map[string][]issueRef
}{
	m: make(map[string][]issueRef),
}

// targetCommitIssues returns the issues that the commits in the commit
// list of target name, the oldest commit first. A reference with no repo
// has Repo "". It runs git one time for the commits that are not in the
// cache.
func targetCommitIssues(target repostats.Target) []issueRef {
	commitIssues.Lock()
	var missing []string
	for _, c := range target.CommitList {
		if _, ok := commitIssues.m[c.SHA]; !ok {
			missing = append(missing, c.SHA)
		}
	}
	commitIssues.Unlock()

	if len(missing) > 0 {
		found, err := readCommitIssues(target.Path, missing)
		if err != nil {
			log.Printf("issues of %s: %v", target.Path, err)
		}
		commitIssues.Lock()
		for sha, refs := range found {
			commitIssues.m[sha] = refs
		}
		commitIssues.Unlock()
	}

	var out []issueRef
	commitIssues.Lock()
	defer commitIssues.Unlock()
	for _, c := range target.CommitList {
		for _, ref := range commitIssues.m[c.SHA] {
			if !slices.Contains(out, ref) {
				out = append(out, ref)
			}
		}
	}
	return out
}

// readCommitIssues reads the messages of the commits shas in the
// checkout at dir, and returns the issues of each commit.
func readCommitIssues(dir string, shas []string) (map[string][]issueRef, error) {
	args := append([]string{"-C", dir, "log", "--no-walk=unsorted", "--format=%H%x00%B%x01"}, shas...)
	out, err := exec.Command("git", args...).Output()
	if err != nil {
		return nil, err
	}
	found := make(map[string][]issueRef, len(shas))
	for rec := range strings.SplitSeq(string(out), "\x01") {
		sha, msg, ok := strings.Cut(strings.TrimSpace(rec), "\x00")
		if ok {
			found[sha] = commitIssueRefs(msg)
		}
	}
	return found, nil
}

// checkoutRepos keeps the GitHub repo of each checkout directory, or ""
// for a checkout with no GitHub remote.
var checkoutRepos sync.Map // dir → string

// cachedCheckoutRepo returns the GitHub repo ("owner/name") of the
// checkout at dir, or "".
func cachedCheckoutRepo(dir string) string {
	if v, ok := checkoutRepos.Load(dir); ok {
		return v.(string)
	}
	repo := checkoutGitHubRepo(dir)
	checkoutRepos.Store(dir, repo)
	return repo
}

// branchIssueStore keeps the issues that the user wrote for each
// branch, in a JSON file. The file holds a map of the branch name to
// the full forms of the issues, for example "tailscale/corp#123".
type branchIssueStore struct {
	path string

	mu       sync.Mutex
	branches map[string][]string
}

// branchIssues is nil until main loads it. A nil *branchIssueStore has
// no issues, and it cannot save.
var branchIssues *branchIssueStore

func loadBranchIssueStore(path string) *branchIssueStore {
	s := &branchIssueStore{
		path:     path,
		branches: make(map[string][]string),
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("branch issues: %v", err)
		}
		return s
	}
	if err := json.Unmarshal(b, &s.branches); err != nil || s.branches == nil {
		log.Printf("branch issues: bad file %s: %v", path, err)
		s.branches = make(map[string][]string)
	}
	return s
}

// get returns the issues of branch.
func (s *branchIssueStore) get(branch string) []issueRef {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []issueRef
	for _, v := range s.branches[branch] {
		if ref, ok := parseIssueToken(v); ok && ref.Repo != "" {
			out = append(out, ref)
		}
	}
	return out
}

// set replaces the issues of branch, and saves the file. An empty list
// removes the branch.
func (s *branchIssueStore) set(branch string, refs []issueRef) error {
	if s == nil {
		return errors.New("no store for branch issues")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(refs) == 0 {
		delete(s.branches, branch)
	} else {
		v := make([]string, len(refs))
		for i, ref := range refs {
			v[i] = ref.String()
		}
		s.branches[branch] = v
	}
	return s.saveLocked()
}

// rename gives the issues of branch from to branch to.
func (s *branchIssueStore) rename(from, to string) error {
	if s == nil || from == to {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.branches[from]
	if !ok {
		return nil
	}
	delete(s.branches, from)
	s.branches[to] = v
	return s.saveLocked()
}

func (s *branchIssueStore) saveLocked() error {
	b, err := json.MarshalIndent(s.branches, "", "\t")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	return writeFileAtomic(s.path, b)
}

// defaultBranchIssuesPath returns the path of the file of branch
// issues. The file is in the config directory and not in the cache
// directory, because the user wrote its contents.
func defaultBranchIssuesPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = cacheRoot
	}
	return filepath.Join(dir, "webdiff", "branch-issues.json")
}

// groupIssues is the issue data of a branch group on the home page.
type groupIssues struct {
	// Branch is the full name of the branch, or "" for a group that has
	// no branch.
	Branch string
	// Base is the GitHub repo of all the worktrees of the group, or ""
	// if they are not all of one repo.
	Base string
	// Issues are the issues from the commits, then the issues that the
	// user wrote.
	Issues []issueRef
	// Manual are the issues that the user wrote.
	Manual []issueRef
}

// lookupGroupIssues sets the issue data of each group.
func lookupGroupIssues(groups []branchGroup) {
	for i := range groups {
		g := &groups[i]
		var gi groupIssues
		if len(g.Worktrees) > 0 {
			if b := g.Worktrees[0].Branch; b != "" && b != "HEAD" {
				gi.Branch = b
			}
		}
		for j, target := range g.Worktrees {
			repo := cachedCheckoutRepo(target.Path)
			if j == 0 {
				gi.Base = repo
			} else if repo != gi.Base {
				gi.Base = ""
			}
			for _, ref := range targetCommitIssues(target) {
				if ref, ok := ref.resolve(repo); ok && !slices.Contains(gi.Issues, ref) {
					gi.Issues = append(gi.Issues, ref)
				}
			}
		}
		if gi.Branch != "" {
			gi.Manual = branchIssues.get(gi.Branch)
		}
		for _, ref := range gi.Manual {
			if !slices.Contains(gi.Issues, ref) {
				gi.Issues = append(gi.Issues, ref)
			}
		}
		g.Issues = gi
	}
}

// writeGroupIssues emits the issues of a branch group, for the line of
// the branch. If the group has no issues, it emits a button that lets
// the user write them. If the user wrote some, it emits a button to
// change them. app.js does the editing (see setupBranchIssues).
func writeGroupIssues(b *strings.Builder, gi groupIssues) {
	if len(gi.Issues) == 0 && gi.Branch == "" {
		return
	}
	manual := make([]string, len(gi.Manual))
	for i, ref := range gi.Manual {
		manual[i] = ref.label(gi.Base)
	}
	fmt.Fprintf(b, `<span class="branch-issues" data-branch="%s" data-repo="%s" data-manual="%s">`,
		template.HTMLEscapeString(gi.Branch),
		template.HTMLEscapeString(gi.Base),
		template.HTMLEscapeString(strings.Join(manual, " ")))
	for _, ref := range gi.Issues {
		fmt.Fprintf(b, `<a class="issue-link" href="%s" target="_blank" rel="noopener" title="%s">%s</a>`,
			template.HTMLEscapeString(ref.URL()),
			template.HTMLEscapeString(ref.String()),
			template.HTMLEscapeString(ref.label(gi.Base)))
	}
	switch {
	case gi.Branch == "":
	case len(gi.Issues) == 0:
		b.WriteString(`<button class="issue-edit issue-add" type="button" title="add the issue numbers of this branch">+ issue</button>`)
	case len(gi.Manual) > 0:
		b.WriteString(`<button class="issue-edit" type="button" title="change the issue numbers of this branch" aria-label="change the issue numbers">✎</button>`)
	}
	b.WriteString(`</span>`)
}

// handleBranchIssues sets the issues that the user wrote for a branch.
// The body is {"branch": ..., "repo": ..., "issues": ...}, where issues
// is the text that the user wrote and repo is the repo of a reference
// with no repo.
func handleBranchIssues(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed", nil)
		return
	}
	var req struct {
		Branch string `json:"branch"`
		Repo   string `json:"repo"`
		Issues string `json:"issues"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad JSON: "+err.Error(), nil)
		return
	}
	if req.Branch == "" {
		writeJSONError(w, http.StatusBadRequest, "no branch", nil)
		return
	}
	if req.Repo != "" {
		if _, err := url.Parse("https://github.com/" + req.Repo); err != nil || !strings.Contains(req.Repo, "/") {
			writeJSONError(w, http.StatusBadRequest, "bad repo", nil)
			return
		}
	}
	refs, err := parseIssueList(req.Issues, req.Repo)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error(), nil)
		return
	}
	if err := branchIssues.set(req.Branch, refs); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error(), nil)
		return
	}
	out := make([]string, len(refs))
	for i, ref := range refs {
		out[i] = ref.String()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"issues": out,
	})
}
