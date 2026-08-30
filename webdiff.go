package main

import (
	"compress/gzip"
	"context"
	_ "embed"
	"flag"
	"fmt"
	"html"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	terminal "github.com/buildkite/terminal-to-html/v3"
	"github.com/tomhjp/webdiff/internal/gitindex"
	"github.com/tomhjp/webdiff/internal/repostats"
	"tailscale.com/safeweb"
)

//go:embed assets/style.css
var styleCSS string

//go:embed assets/page.html.tmpl
var pageTmplSrc string

//go:embed assets/app.js
var appJS string

// iconSprite holds the <symbol> definitions the icon() helper points `<use>`
// at. It has to be inlined into each page rather than served as its own file:
// `<use href="external.svg#id">` is blocked cross-document in Safari and
// WebKit generally, and a same-document fragment is the only form that works
// everywhere. Traced from the Zed and Ghostty app icons.
//
//go:embed assets/icons.svg
var iconSprite string

// pageTmpl is the HTML wrapper for every rendered view. Parsed once at
// init time so a malformed template panics on startup rather than on
// the first request. The CSS blocks are still inlined (each from its
// own `<style>` tag), but the JavaScript lives at /app.js so we can
// keep `script-src` strict.
var pageTmpl = template.Must(template.New("page").Parse(pageTmplSrc))

// xterm256 is the per-index .term-fgxN / .term-bgxN ruleset for the
// 256-colour palette, computed once at init. It depends only on string
// arithmetic, not runtime config, so caching it is just an optimisation
// — the result is identical on every call.
var xterm256 = computeXterm256CSS()

// paletteCSS is the contents of ~/.config/webdiff/palette.css read once
// at startup. Empty when the user hasn't generated one — that's the
// common case, not an error. Loaded after styleCSS so any :root
// custom-properties it defines override the defaults baked into style.css.
var paletteCSS string

// rootDir is the directory the server was started in. All requests resolve
// repo paths relative to this.
var rootDir string

// agentCmd is the full shell command spawned in each repo's tmux pane
// (e.g. "claude" or "opencode --foo --bar"); set from the -agent flag in
// main. agentName is the first whitespace-separated token, used for the
// header button label, status messages, and matched against tmux's
// pane_current_command for readiness detection. Both are the *default*
// — a session started from the UI can pick any knownAgents entry.
var (
	agentCmd  = "claude"
	agentName = "claude"
)

// knownAgents are the agents selectable per session from the UI, keyed
// by the id that travels in `?agent=`. Doubling as the allowlist for
// that parameter is the point: the value reaches `bash -lc`, so a
// request can only ever name a command that appears here.
var knownAgents = map[string]string{
	"claude": "claude",
	"pi":     "pi",
}

// resolveAgent maps an `?agent=` id to the command to spawn and the
// name tmux will report for it. Anything unknown — including "" —
// falls back to the -agent flag's value, so a hand-edited URL degrades
// to the default rather than erroring.
func resolveAgent(id string) (name, cmd string) {
	if c, ok := knownAgents[id]; ok {
		return id, c
	}
	return agentName, agentCmd
}

// agentChoices lists the ids offered in the UI's agent drop-downs,
// sorted for a stable order. agentName is included even when -agent
// named something outside knownAgents, so the running default is
// always selectable.
func agentChoices() []string {
	ids := make([]string, 0, len(knownAgents)+1)
	for id := range knownAgents {
		ids = append(ids, id)
	}
	if _, ok := knownAgents[agentName]; !ok {
		ids = append(ids, agentName)
	}
	slices.Sort(ids)
	return ids
}

// writeAgentSelect emits the agent drop-down with selected pre-picked.
// Shares #diff-mode-select's styling via .chrome-select so the two read
// as the same kind of control.
func writeAgentSelect(b *strings.Builder, id, selected string) {
	fmt.Fprintf(b, `<select class="chrome-select" id="%s" title="which agent to start">`, id)
	for _, choice := range agentChoices() {
		sel := ""
		if choice == selected {
			sel = ` selected`
		}
		fmt.Fprintf(b, `<option value="%s"%s>%s</option>`,
			template.HTMLEscapeString(choice), sel, template.HTMLEscapeString(choice))
	}
	b.WriteString(`</select>`)
}

// agentQuery returns the "?agent=<id>" suffix for a non-default agent,
// mirroring modeQuery. Unlike the diff mode this isn't threaded onto
// general navigation links — it's a one-shot choice for the session
// about to be spawned, carried only far enough that a reload of the
// stream page still shows which agent is running.
func agentQuery(id string) string {
	if id == "" || id == agentName {
		return ""
	}
	if _, ok := knownAgents[id]; !ok {
		return ""
	}
	return "?agent=" + url.QueryEscape(id)
}

// sshHost is how the machine running the _browser_ reaches this one over
// SSH, for the hand-off links writeOpenButtons emits. The MagicDNS name is
// only a guess at that — it's wrong as soon as the user's ~/.ssh/config
// needs a different user, port or key — so -ssh-host overrides it.
var sshHost string

// cacheRoot is the top-level webdiff cache directory (e.g.
// ~/.cache/webdiff), holding only things webdiff can rebuild: per-repo
// warm git index/object caches, pane attachments, session archives.
//
// worktreesRoot is deliberately *not* under it — managed worktrees hold
// real uncommitted work, so they live at `<rootDir>/wt/<repo>/<leaf>`,
// next to the repos they belong to. That nesting inside rootDir means
// any code classifying a path by root prefix has to test worktreesRoot
// first; see worktreeSlug.
var (
	cacheRoot       string
	repoCacheRoot   string
	worktreesRoot   string
	attachmentsRoot string
	indexCache      *gitindex.Cache
	statsManager    *repostats.Manager
)

// isGitRepo reports whether dir contains a .git entry (file or directory).
func isGitRepo(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

// repoRef names one diffable target — either a regular repo under
// rootDir or a managed worktree under worktreesRoot. The two are
// disjoint URL namespaces (/repos/<name>/, /worktrees/<name>/) so the
// kind field doubles as the URL's first segment.
type repoRef struct {
	abs  string // absolute filesystem path
	kind string // "repos" or "worktrees"
	name string // URL slug: see worktreeSlug
	url  string // canonical URL with trailing slash, e.g. "/repos/foo/"
}

// resolveRepoRef parses a URL path like "/repos/foo/" or
// "/worktrees/foo-bar/" and resolves it to a repoRef. The caller must
// have already stripped any handler-specific prefix (e.g. "/stream",
// "/agent") so the remaining path starts with the kind segment. Returns
// ok=false for unknown kinds, missing names, traversal attempts, or
// non-existent dirs.
func resolveRepoRef(urlPath string) (repoRef, bool) {
	trimmed := strings.Trim(urlPath, "/")
	parts := strings.SplitN(trimmed, "/", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return repoRef{}, false
	}
	if len(parts) == 3 && parts[2] != "" {
		return repoRef{}, false
	}
	kind, name := parts[0], parts[1]
	if strings.HasPrefix(name, ".") || strings.ContainsAny(name, "/\\") {
		return repoRef{}, false
	}
	var abs string
	if statsManager != nil {
		if target, found := statsManager.Lookup(repostats.Kind(kind), name); found {
			abs = target.Path
			return repoRef{abs: abs, kind: kind, name: name, url: "/" + kind + "/" + name + "/"}, true
		}
	}
	switch kind {
	case "repos":
		baseAbs, err := filepath.Abs(rootDir)
		if err != nil {
			return repoRef{}, false
		}
		abs = filepath.Join(baseAbs, name)
		if abs != filepath.Clean(abs) || filepath.Dir(abs) != baseAbs {
			return repoRef{}, false
		}
		info, err := os.Stat(abs)
		if err != nil || !info.IsDir() {
			return repoRef{}, false
		}
		// Without this, worktreesRoot ("<rootDir>/wt") would itself be
		// addressable as /repos/wt/.
		if !isGitRepo(abs) {
			return repoRef{}, false
		}
	case "worktrees":
		// Match the requested slug against what's on disk rather than
		// joining it onto worktreesRoot — the slug flattens two path
		// segments, so it can't be turned back into a path directly.
		// No isGitRepo check: a worktree whose gitfile is dangling still
		// has to resolve so the home page's remove button can reach it.
		for _, p := range managedWorktreePaths() {
			if worktreeSlug(p) == name {
				abs = p
				break
			}
		}
		if abs == "" {
			return repoRef{}, false
		}
	default:
		return repoRef{}, false
	}
	return repoRef{abs: abs, kind: kind, name: name, url: "/" + kind + "/" + name + "/"}, true
}

// diffEnv acquires the same private warm index used by the background stats
// manager. Detailed diff pages therefore never touch the checkout's real
// index or race a stats refresh against the shared private index.
func diffEnv(repo string) (env []string, cleanup func()) {
	if indexCache == nil {
		return os.Environ(), func() {}
	}
	env, cleanup, err := indexCache.Acquire(context.Background(), repo, indexKey(repo))
	if err != nil {
		return os.Environ(), func() {}
	}
	return env, cleanup
}

func indexKey(repo string) string {
	kind := "repos"
	if root, err := filepath.Abs(worktreesRoot); err == nil {
		if rel, relErr := filepath.Rel(root, repo); relErr == nil && rel != "." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			kind = "worktrees"
		}
	}
	return kind + "-" + worktreeSlug(repo)
}

// branchBase returns the merge-base SHA between HEAD and the remote's
// main branch. It prefers <remote>/main (remote resolved via
// defaultRemote, so repos whose sole remote is "ro" rather than "origin"
// work) over a local "main" branch, which is often stale relative to the
// remote and would otherwise inflate the diff with hundreds of upstream
// commits. Falls back to "HEAD" when no candidate resolves, giving the
// original working-copy-only behaviour.
func branchBase(repo string) (sha, label string) {
	var refs []string
	if remote, ok := defaultRemote(repo); ok {
		refs = append(refs, remote+"/main")
	}
	refs = append(refs, "origin/main", "main")
	for _, ref := range refs {
		out, err := exec.Command("git", "-C", repo, "merge-base", "HEAD", ref).Output()
		if err == nil {
			if s := strings.TrimSpace(string(out)); s != "" {
				return s, "main"
			}
		}
	}
	return "HEAD", ""
}

// modeQuery returns the "?mode=<mode>" suffix for non-default diff
// modes, used when emitting in-app navigation links so a user's
// selected mode survives switching between the home and diff pages.
// Returns "" for the default ("working") mode so plain URLs stay clean.
func modeQuery(mode string) string {
	if mode == "" || mode == "working" {
		return ""
	}
	return "?mode=" + url.QueryEscape(mode)
}

// upstreamRef returns the symbolic name of the upstream tracking branch
// (e.g. "origin/feature") if one is configured for HEAD, or if there is
// exactly one remote and the current branch exists on it.
func upstreamRef(repo string) (ref string, ok bool) {
	out, err := exec.Command("git", "-C", repo, "rev-parse",
		"--abbrev-ref", "--symbolic-full-name", "@{u}").Output()
	if err == nil {
		ref = strings.TrimSpace(string(out))
		if ref != "" && ref != "@{u}" {
			return ref, true
		}
	}
	// No configured upstream: if there is exactly one remote and the
	// current branch exists on it, use <remote>/<branch>.
	remoteOut, err := exec.Command("git", "-C", repo, "remote").Output()
	if err != nil {
		return "", false
	}
	remotes := strings.Fields(strings.TrimSpace(string(remoteOut)))
	if len(remotes) != 1 {
		return "", false
	}
	branch := currentBranch(repo)
	if branch == "" || branch == "HEAD" {
		return "", false
	}
	candidate := remotes[0] + "/" + branch
	if _, err := exec.Command("git", "-C", repo, "rev-parse", "--verify", candidate).Output(); err != nil {
		return "", false
	}
	return candidate, true
}

// diffBase returns the git ref and display label to diff against for the
// given mode. "working" diffs against HEAD (all uncommitted changes),
// "branch" against the merge-base with main (full branch diff), and
// "remote" against the upstream tracking branch (what a push would add).
func diffBase(repo, mode string) (sha, label string) {
	switch mode {
	case "working":
		return "HEAD", ""
	case "remote":
		if ref, ok := upstreamRef(repo); ok {
			return ref, ref
		}
		return branchBase(repo)
	default:
		return branchBase(repo)
	}
}

// currentBranch returns the current git branch name for repo.
func currentBranch(repo string) string {
	cmd := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
	cmd.Dir = repo
	out, err := cmd.Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

// diffStat parses git's --shortstat output into file/insertion/deletion counts.
func diffStat(env []string, repo, base string) (files, ins, del int) {
	cmd := exec.Command("git", "diff", "--cached", "--shortstat", base)
	cmd.Env = env
	cmd.Dir = repo
	out, err := cmd.Output()
	if err != nil {
		return
	}
	for _, part := range strings.Split(strings.TrimSpace(string(out)), ",") {
		part = strings.TrimSpace(part)
		var n int
		if _, err := fmt.Sscanf(part, "%d", &n); err != nil {
			continue
		}
		switch {
		case strings.Contains(part, "file"):
			files = n
		case strings.Contains(part, "insertion"):
			ins = n
		case strings.Contains(part, "deletion"):
			del = n
		}
	}
	return
}

// fileDiffStat returns the per-file numstat: added/removed line counts, or
// binary=true for binary files.
func fileDiffStat(env []string, repo, base, file string) (added, removed string, binary bool) {
	cmd := exec.Command("git", "diff", "--cached", "--numstat", base, "--", file)
	cmd.Env = env
	cmd.Dir = repo
	out, err := cmd.Output()
	if err != nil {
		return
	}
	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) < 2 {
		return
	}
	if fields[0] == "-" {
		binary = true
		return
	}
	added, removed = fields[0], fields[1]
	return
}

// changedFiles returns the list of files changed since base.
func changedFiles(env []string, repo, base string) []string {
	cmd := exec.Command("git", "diff", "--cached", "--name-only", base)
	cmd.Env = env
	cmd.Dir = repo
	out, err := cmd.Output()
	if err != nil || len(strings.TrimSpace(string(out))) == 0 {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(out)), "\n")
}

// fileDiff returns the canonical structure for one file (parsed from
// `git diff --no-color`, environment-stable) and the styled bytes from
// the same diff piped through the user's pager (delta etc.). The two
// halves drive different things: canonical owns line numbers and the
// add/del/context kind that comments anchor to; styled is purely
// visual and can vary freely from one machine to another.
//
// Wrapping is disabled for delta (`--wrap-max-lines 0`) so each source
// line maps to exactly one rendered line — the zipper in wrapDiffLines
// then does a straight 1:1 walk against canonical.
func fileDiff(env []string, repo, base, file string, cols int, fullContext bool) ([]diffLine, []byte) {
	colorArgs := []string{"diff", "--cached", "--color=always"}
	noColorArgs := []string{"diff", "--cached", "--no-color"}
	if fullContext {
		colorArgs = append(colorArgs, "-U99999")
		noColorArgs = append(noColorArgs, "-U99999")
	}
	colorArgs = append(colorArgs, base, "--", file)
	noColorArgs = append(noColorArgs, base, "--", file)

	canonCmd := exec.Command("git", noColorArgs...)
	canonCmd.Env = env
	canonCmd.Dir = repo
	canonOut, err := canonCmd.Output()
	if err != nil || len(canonOut) == 0 {
		return nil, nil
	}
	canon := parseCanonical(canonOut)

	colorCmd := exec.Command("git", colorArgs...)
	colorCmd.Env = env
	colorCmd.Dir = repo
	raw, err := colorCmd.Output()
	if err != nil || len(raw) == 0 {
		return canon, nil
	}

	pagerCmd := exec.Command("git", "var", "GIT_PAGER")
	pagerCmd.Dir = repo
	pagerOut, _ := pagerCmd.Output()
	pager := strings.TrimSpace(string(pagerOut))
	if pager == "" || pager == "cat" || pager == "less" || pager == "more" {
		return canon, raw
	}

	colsEnv := append(env, fmt.Sprintf("COLUMNS=%d", cols), "LANG=en_US.UTF-8", "LC_ALL=en_US.UTF-8")
	pagerWithWidth := pager
	if strings.Contains(pager, "delta") {
		pagerWithWidth = fmt.Sprintf("%s --width %d --wrap-max-lines 0", pager, cols)
	}

	shell := exec.Command("sh", "-c", pagerWithWidth)
	shell.Env = colsEnv
	shell.Stdin = strings.NewReader(string(raw))
	colored, err := shell.Output()
	if err != nil {
		return canon, raw
	}
	return canon, colored
}

// diffLine is one source line of a unified diff: an add (`+`), a del
// (`-`), or a context line (` `). text is the line content with its
// leading kind byte stripped. oldLine / newLine are 1-based; only the
// applicable side is set (add has newLine only, del has oldLine only).
type diffLine struct {
	kind    byte
	oldLine int
	newLine int
	text    string
}

var hunkHeaderRE = regexp.MustCompile(`^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

// parseCanonical walks a `git diff --no-color` body and emits one
// diffLine per source line, tracking 1-based old/new line numbers via
// the @@ hunk headers. File headers (`diff --git`, `index`, `---`,
// `+++`, `Binary files differ`) sit before the first @@ and are
// skipped; `\ No newline at end of file` markers are skipped too.
func parseCanonical(raw []byte) []diffLine {
	out := make([]diffLine, 0, 64)
	var oldL, newL int
	inBody := false
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "@@ ") {
			if m := hunkHeaderRE.FindStringSubmatch(line); m != nil {
				oldL, _ = strconv.Atoi(m[1])
				newL, _ = strconv.Atoi(m[2])
				inBody = true
			}
			continue
		}
		if !inBody || line == "" || strings.HasPrefix(line, `\`) {
			continue
		}
		kind := line[0]
		text := ""
		if len(line) > 1 {
			text = line[1:]
		}
		switch kind {
		case ' ':
			out = append(out, diffLine{kind: ' ', oldLine: oldL, newLine: newL, text: text})
			oldL++
			newL++
		case '+':
			out = append(out, diffLine{kind: '+', newLine: newL, text: text})
			newL++
		case '-':
			out = append(out, diffLine{kind: '-', oldLine: oldL, text: text})
			oldL++
		}
	}
	return out
}

// htmlTagRE strips HTML tags so we can read the visible text of a single
// rendered line (for blank-detection and heading extraction).
var htmlTagRE = regexp.MustCompile(`<[^>]+>`)

// bgSpanRE matches the opening tag of a <span> whose class list carries
// any term-bgxN / term-bgN background — i.e. delta's diff body bg,
// regardless of which colour or emph variant. splitDiffLine uses this
// to peel the (no-bg) gutter off so CSS can paint the body without the
// inline ANSI bg fighting it.
var bgSpanRE = regexp.MustCompile(`<span [^>]*term-bg(?:x\d+|\d\d)`)

// extractHeading peels off delta's per-file leading heading — the
// "added: foo.go" / "src/foo.go" line and the single `─────` underline
// directly beneath it — so we can hoist that text into the <summary>
// instead of repeating it inside the diff body. We deliberately leave
// the trailing blank, the hunk box top (`────┐`), and everything below
// in place so the body still gets its visual breathing room and the
// hunk-header context box is intact. Returns ("", html) if the leading
// shape doesn't match (binary files, weird configs, etc) so the caller
// falls back to the path it already had.
func extractHeading(html string) (heading, rest string) {
	lines := strings.Split(strings.TrimRight(html, "\n"), "\n")
	i := 0
	for i < len(lines) && isVisuallyBlank(lines[i]) {
		i++
	}
	if i >= len(lines) {
		return "", html
	}
	heading = strings.TrimSpace(htmlText(lines[i]))
	if heading == "" || isSeparatorOnly(heading) {
		return "", html
	}
	j := i + 1
	if j < len(lines) && isSeparatorOnly(strings.TrimSpace(htmlText(lines[j]))) {
		j++
	}
	return heading, strings.Join(lines[j:], "\n")
}

// htmlText returns the visible text of an HTML fragment with entities
// decoded — &nbsp; collapses to a regular space so callers can treat it
// uniformly.
func htmlText(s string) string {
	return strings.ReplaceAll(html.UnescapeString(htmlTagRE.ReplaceAllString(s, "")), "\u00a0", " ")
}

// isSeparatorOnly reports whether a line is just a delta-style box
// drawing run (── ─ ┌ ┐ └ ┘ │ ┊ ⋮ etc) with optional whitespace. Used
// for stripping the heading separator and detecting hunk boxes.
func isSeparatorOnly(text string) bool {
	if text == "" {
		return false
	}
	for _, r := range text {
		switch {
		case r == ' ', r == '\t':
		case r == '─', r == '━', r == '│', r == '┊', r == '⋮':
		case r == '┌', r == '┐', r == '└', r == '┘', r == '├', r == '┤', r == '┬', r == '┴', r == '┼':
		default:
			return false
		}
	}
	return true
}

// isVisuallyBlank reports whether a line carries no information content
// — either a fully empty line or a delta line-number gutter with no
// code, in which case we can fill it with the surrounding diff bg.
func isVisuallyBlank(s string) bool {
	text := strings.TrimSpace(htmlText(s))
	if text == "" {
		return true
	}
	for _, r := range text {
		switch {
		case r == ' ', r >= '0' && r <= '9':
		case r == '⋮', r == '┊', r == '│':
		default:
			return false
		}
	}
	return true
}

// wrapDiffLines walks the styled HTML lines and zips them against the
// canonical diff records, attaching click-to-comment data attributes
// (data-file, data-old-line, data-new-line) and the line-add / line-del
// classes from canonical — never inferring them from styling. Styled
// lines that don't match canonical (delta's heading box, hunk-summary
// decorations, etc.) emit as plain `<span class="line">…</span>`; the
// first such line becomes the file-comment anchor.
//
// The match rule is a suffix check on visible text (htmlText strips
// tags + entities). Delta prepends a line-numbers gutter to the body
// and never appends, so suffix matching is robust across themes. Empty
// source lines are matched separately by shape: just a +/-/space prefix
// (raw `git diff`) or a 2+ separator line-numbers gutter (delta).
func wrapDiffLines(htmlIn, file string, canon []diffLine) string {
	htmlIn = strings.TrimRight(htmlIn, "\n")
	lines := strings.Split(htmlIn, "\n")
	fileAttr := ""
	if file != "" {
		fileAttr = fmt.Sprintf(` data-file="%s"`, template.HTMLEscapeString(file))
	}
	var b strings.Builder
	fileAnchorWritten := false
	canonIdx := 0
	for _, line := range lines {
		var matched *diffLine
		if canonIdx < len(canon) && matchesCanonical(htmlText(line), canon[canonIdx].text) {
			matched = &canon[canonIdx]
			canonIdx++
		}
		if matched == nil {
			if !fileAnchorWritten && file != "" {
				fmt.Fprintf(&b, `<span class="line line-file-anchor" data-file-comment="%s" title="comment on this whole file">%s</span>`,
					template.HTMLEscapeString(file), line)
				fileAnchorWritten = true
				continue
			}
			fmt.Fprintf(&b, `<span class="line">%s</span>`, line)
			continue
		}
		attrs := fileAttr
		if matched.oldLine > 0 {
			attrs += fmt.Sprintf(` data-old-line="%d"`, matched.oldLine)
		}
		if matched.newLine > 0 {
			attrs += fmt.Sprintf(` data-new-line="%d"`, matched.newLine)
		}
		var cls string
		switch matched.kind {
		case '+':
			cls = "add"
		case '-':
			cls = "del"
		}
		// Peel delta's line-number gutter off the code so the two can be
		// driven independently: the gutter is the click/drag target for
		// leaving comments and is excluded from text selection, while the
		// code column stays selectable for copying. splitGutter handles
		// delta (every body line, add/del/context alike); raw `git diff`
		// has no gutter, so add/del fall back to the bg-span split and
		// context lines emit whole.
		if gp, gt, ok := splitGutter(line); ok {
			lineCls := "line"
			if cls != "" {
				lineCls += " line-" + cls
			}
			fmt.Fprintf(&b,
				`<span class="%s"%s><span class="line-prefix line-gutter">%s</span><span class="line-tail">%s</span></span>`,
				lineCls, attrs, gp, gt)
			continue
		}
		if cls == "" {
			fmt.Fprintf(&b, `<span class="line"%s>%s</span>`, attrs, line)
			continue
		}
		prefix, tail := splitDiffLine(line)
		fmt.Fprintf(&b,
			`<span class="line line-%s"%s><span class="line-prefix">%s</span><span class="line-tail">%s</span></span>`,
			cls, attrs, prefix, tail)
	}
	return b.String()
}

// matchesCanonical decides whether a styled line carries the body of
// the next canonical record. terminal-to-html expands tabs to spaces
// based on screen-column tab stops, so byte-exact comparisons miss any
// line containing a tab — we collapse runs of whitespace on both sides
// before suffix-matching, which handles both leading indentation and
// embedded tabs uniformly.
//
// For empty source lines we fall back to a shape check: visible is
// just a +/- (raw `git diff` empty add/del) or a delta line-numbers
// gutter (≥2 separator characters). That rejects decorations like
// delta's hunk-box `1│` (only one separator) and the `───┐ ───┘`
// borders.
func matchesCanonical(visible, canonText string) bool {
	if stripped := strings.TrimSpace(canonText); stripped != "" {
		return strings.HasSuffix(collapseWS(visible), collapseWS(stripped))
	}
	trimmed := strings.TrimSpace(visible)
	if trimmed == "+" || trimmed == "-" {
		return true
	}
	seps := 0
	for _, r := range trimmed {
		switch r {
		case '⋮', '│', '┊', '⎜', '▏', '|':
			seps++
		}
	}
	return seps >= 2
}

// collapseWS normalises every run of spaces / tabs in s into a single
// space. terminal-to-html turns tabs into a column-dependent number of
// spaces; canonical content keeps the literal `\t`, so we have to
// erase that distinction before comparing.
func collapseWS(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inWS := false
	for _, r := range s {
		if r == ' ' || r == '\t' {
			if !inWS {
				b.WriteByte(' ')
				inWS = true
			}
			continue
		}
		b.WriteRune(r)
		inWS = false
	}
	return b.String()
}

// splitDiffLine partitions a single rendered line into the gutter
// prefix (rendered without a diff bg) and the code tail (rendered with
// the diff bg, extending to end of line via flex). Splitting at the
// first bg-span — regardless of which colour or emph variant — keeps
// the leading whitespace inside the body where the CSS bg covers it,
// even when delta paints emph words with a different bg class than the
// surrounding regular bg.
//
// When the line has no bg span at all (raw `git diff`, or a blank line
// with only the line-numbers gutter) the whole line goes into the
// prefix and CSS bg fills the (empty) tail to the right edge.
func splitDiffLine(line string) (prefix, tail string) {
	if loc := bgSpanRE.FindStringIndex(line); loc != nil {
		return line[:loc[0]], line[loc[0]:]
	}
	return line, ""
}

// splitGutter peels delta's line-number gutter off a rendered diff line,
// returning the gutter HTML and the code HTML on a clean tag boundary.
// Delta closes the gutter with a `│` column separator immediately before
// the code, sitting in its own coloured span; we cut after that span's
// closing tag so neither side is left with a dangling element. ok is
// false when there's no delta gutter (raw `git diff` output), leaving the
// caller to fall back to its non-delta handling.
func splitGutter(line string) (prefix, tail string, ok bool) {
	const sep = "│" // U+2502, delta's gutter→code column separator
	i := strings.Index(line, sep)
	if i < 0 {
		return "", "", false
	}
	rest := line[i+len(sep):]
	c := strings.Index(rest, "</span>")
	if c < 0 {
		return "", "", false
	}
	cut := i + len(sep) + c + len("</span>")
	return line[:cut], line[cut:], true
}

// renderHomeHandler is the root-only fallback. Anything other than "/"
// 404s — there is no nested directory navigation, just the two named
// URL namespaces (/repos/, /worktrees/) and the home page's three
// sections (repos, worktrees, tmux).
func renderHomeHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	renderHome(w, r)
}

// renderRepoDiff is the shared handler for /repos/<name>/ and
// /worktrees/<name>/. Both resolve to a repoRef and dispatch to
// renderRepo; the kind field on the ref is what later distinguishes
// "add a worktree" (on a repo) from "remove this worktree" (on a
// worktree) in the diff page chrome.
func renderRepoDiff(w http.ResponseWriter, r *http.Request) {
	ref, ok := resolveRepoRef(r.URL.Path)
	if !ok || !isGitRepo(ref.abs) {
		http.NotFound(w, r)
		return
	}
	renderRepo(w, r, ref)
}

// renderHome renders the authoritative target snapshot maintained by the
// background stats manager. Current flushes any pending debounce work and
// waits for it, so every row in this single response is current; no browser
// fan-out or progressive stats loading is needed.
func renderHome(w http.ResponseWriter, r *http.Request) {
	targets, err := statsManager.Current(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	mode := r.URL.Query().Get("mode")
	if mode != "working" && mode != "branch" && mode != "remote" {
		mode = "working"
	}
	modeQS := modeQuery(mode)

	var body strings.Builder
	body.WriteString(`<div class="header"><div class="header-left">`)
	fmt.Fprintf(&body, `<span class="branch">%s</span>`, template.HTMLEscapeString(filepath.Base(rootDir)))
	fmt.Fprintf(&body, `<span class="workdir">%s</span>`, template.HTMLEscapeString(tildePath(rootDir)))
	body.WriteString(`</div><div class="header-right">`)
	body.WriteString(`<a class="toggle" href="/history/" title="archived agent sessions">history</a>`)
	body.WriteString(`<select id="diff-mode-select">`)
	for _, opt := range []struct{ val, label string }{
		{"working", "uncommitted"},
		{"branch", "branch"},
		{"remote", "remote"},
	} {
		sel := ""
		if opt.val == mode {
			sel = ` selected`
		}
		fmt.Fprintf(&body, `<option value="%s"%s>%s</option>`, opt.val, sel, opt.label)
	}
	body.WriteString(`</select></div></div>`)

	body.WriteString(`<h2 class="section-title">repos</h2><div class="dir-list">`)
	repos := 0
	for _, target := range targets {
		if target.Kind != repostats.Repo {
			continue
		}
		repos++
		writeDirRow(&body, target, target.Name, mode, modeQS)
	}
	if repos == 0 {
		body.WriteString(`<p class="empty">No git repos under this directory.</p>`)
	}
	body.WriteString(`</div>`)

	worktrees := 0
	for _, target := range targets {
		if target.Kind == repostats.Worktree {
			worktrees++
		}
	}
	if worktrees > 0 {
		body.WriteString(`<h2 class="section-title">worktrees</h2><div class="dir-list">`)
		for _, target := range targets {
			if target.Kind != repostats.Worktree {
				continue
			}
			label := target.ParentRepo
			if label == "" {
				label = target.Name
			}
			writeDirRow(&body, target, label, mode, modeQS)
		}
		body.WriteString(`</div>`)
	}

	writeTmuxSection(&body, r.Context())

	writePage(w, body.String())
}

// writeDirRow emits one <a class="dir-row repo-row"> for a repo or
// worktree on the home page. slug is the URL/stats path component; label
// is what shows on the left (the repo name for a repo, the parent repo
// for a worktree, so every row reads "repo … branch"); dir is the
// checkout's full path, shown in place of label on wide viewports. The
// branch and diff stats come from the authoritative repostats snapshot.
// Broken worktrees get the dir-broken class so they're greyed out but
// still clickable through to the diff page where the
// remove button lives.
//
// The copy and open buttons are siblings of the anchor rather than
// children: nesting a <button> or <a> inside an <a> is invalid HTML, and
// as siblings they need no click interception to stop the row
// navigating. The .dir-row-wrap flex row carries the border the anchor
// would otherwise draw, so the rule runs the full width underneath all
// of them.
func writeDirRow(b *strings.Builder, target repostats.Target, label, mode, modeQS string) {
	url := "/" + string(target.Kind) + "/" + target.Name + "/" + modeQS
	cls := "dir-row repo-row"
	if target.Broken {
		cls += " dir-broken"
	}
	b.WriteString(`<div class="dir-row-wrap">`)
	fmt.Fprintf(b, `<a class="%s" href="%s"><span class="dir-name">`, cls, template.HTMLEscapeString(url))
	writeTitle(b, label, target.Path)
	b.WriteString(`</span><span class="dir-stats">`)
	if mode != "remote" && !target.Broken {
		s := target.Working
		if mode == "branch" {
			s = target.BranchDiff
		}
		writeSummary(b, s)
	}
	b.WriteString(`</span>`)
	if target.Broken {
		b.WriteString(`<span class="dir-branch dir-folder-tag">broken</span>`)
	} else {
		fmt.Fprintf(b, `<span class="dir-branch">%s</span>`, template.HTMLEscapeString(target.Branch))
	}
	b.WriteString(`</a>`)
	writeOpenButtons(b, target.Path, "dir-open")
	writeCopyButton(b, target.Path, "dir-copy")
	b.WriteString(`</div>`)
}

func writeSummary(b *strings.Builder, s repostats.Summary) {
	if s.Files == 0 {
		return
	}
	noun := "files"
	if s.Files == 1 {
		noun = "file"
	}
	fmt.Fprintf(b, `<span class="stats-files">%d %s</span>`, s.Files, noun)
	if s.Ins > 0 {
		fmt.Fprintf(b, ` <span class="stats-ins" data-stat="+%d"></span>`, s.Ins)
	}
	if s.Del > 0 {
		fmt.Fprintf(b, ` <span class="stats-del" data-stat="-%d"></span>`, s.Del)
	}
}

// writeCopyButton emits a button that copies dir to the clipboard, which
// app.js wires up by the data-copy attribute. What lands on the clipboard
// is the tilde-abbreviated form, matching what's on screen — these paths
// get pasted into a shell, which expands the ~ back. Below 900px the path
// itself isn't rendered at all (.title-full is hidden), so on a phone this
// is the only way to get at it.
func writeCopyButton(b *strings.Builder, dir, cls string) {
	if dir == "" {
		return
	}
	short := template.HTMLEscapeString(tildePath(dir))
	fmt.Fprintf(b, `<button class="%s" type="button" data-copy="%s" title="copy %s">📋</button>`,
		cls, short, short)
}

// writeOpenButtons emits links that open dir in Zed and in Ghostty on the
// machine running the browser. They have to be plain anchors to a custom
// scheme rather than a fetch: this page comes straight off the sandbox, so
// a request would land back here instead of on the desktop, and only a
// top-level navigation reaches the client's OS at all.
//
// Zed registers zed:// itself and documents the zed://ssh/<host>/<path>
// hotlink form, so it needs nothing installed. Ghostty has no inbound
// scheme, hence webdiff:// and deploy/install-url-handler.sh. Neither
// resolves on iOS, so .open-link hides both below 900px.
//
// Without an sshHost there's no host to name, and we'd rather emit nothing
// than a link that can't connect.
func writeOpenButtons(b *strings.Builder, dir, cls string) {
	if dir == "" || sshHost == "" {
		return
	}
	// dir is absolute, so it supplies the separator after the host.
	zed := template.HTMLEscapeString("zed://ssh/" + sshHost + dir)
	ghostty := template.HTMLEscapeString("webdiff://ghostty?host=" +
		url.QueryEscape(sshHost) + "&dir=" + url.QueryEscape(dir))
	short := template.HTMLEscapeString(tildePath(dir))
	fmt.Fprintf(b, `<a class="%s open-link" href="%s" title="open %s in Zed">%s</a>`,
		cls, zed, short, icon("icon-zed"))
	fmt.Fprintf(b, `<a class="%s open-link" href="%s" title="open a shell in %s">%s</a>`,
		cls, ghostty, short, icon("icon-ghostty"))
}

// icon references one of the <symbol>s in the assets/icons.svg sprite that
// writeIconSprite drops at the top of every page. The marks are traced from
// the apps' own icons, so they're a few KB of path data each — far too much
// to inline per row when the home page has one pair per repo.
func icon(id string) string {
	return `<svg class="icon" aria-hidden="true"><use href="#` + id + `"/></svg>`
}

// writeTitle emits label twice — once bare and once as the checkout's
// full directory — and CSS (.title-short/.title-full) picks one by
// viewport width: the short name on phones, the full path on large
// screens where the extra width is free disambiguation between repos
// and their worktrees. With no dir (e.g. an arbitrary tmux pane) only
// the label is emitted.
func writeTitle(b *strings.Builder, label, dir string) {
	if dir == "" {
		b.WriteString(template.HTMLEscapeString(label))
		return
	}
	fmt.Fprintf(b, `<span class="title-short">%s</span><span class="title-full">%s</span>`,
		template.HTMLEscapeString(label), template.HTMLEscapeString(tildePath(dir)))
}

// tildePath abbreviates the user's home directory to "~" for display.
// Paths are only ever shown, never fed back to the server, so the lossy
// form is fine.
func tildePath(dir string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return dir
	}
	if dir == home {
		return "~"
	}
	if rest, ok := strings.CutPrefix(dir, home+string(filepath.Separator)); ok {
		return "~" + string(filepath.Separator) + rest
	}
	return dir
}

// worktreeEntry describes one managed worktree as rendered on the home
// page. Repo is the parent repo's basename for display (e.g. "webdiff")
// — purely informational, not used for resolution. Broken means we
// couldn't talk to git for it; the entry is still listed so it can be
// removed via the worktree's own diff page.
type worktreeEntry struct {
	Name   string
	Path   string
	Repo   string
	Broken bool
}

// worktreeSlug maps a checkout's absolute path to the identity webdiff
// uses everywhere a single safe token is needed: the URL slug in
// /worktrees/<slug>/, the `<slug>-wd` tmux session name, and the diff
// cache dir. Managed worktrees live two levels down, at
// `<worktreesRoot>/<repo>/<leaf>`, and flatten to "<repo>-<leaf>";
// anything else — a plain repo, or a path that isn't exactly two levels
// under worktreesRoot — is just its basename.
//
// Flattening rather than nesting the slug is what keeps /file/,
// /stream/, the git-http sync endpoints and app.js working on a
// two-segment "<kind>/<name>" URL shape. It isn't injective
// ("corp/fix-x" and "corp-fix/x" both give "corp-fix-x"), so
// handleWorktreeCreate rejects a slug that's already taken.
//
// The depth check has to be exact, not a prefix test: repoSessionName
// feeds this paths that tmux reported, which may be stale or arbitrary.
// worktreesRoot is inside rootDir, so it must be tested first.
func worktreeSlug(abs string) string {
	abs = filepath.Clean(abs)
	base := filepath.Base(abs)
	if worktreesRoot == "" {
		return base
	}
	root, err := filepath.Abs(worktreesRoot)
	if err != nil {
		return base
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return base
	}
	parent, leaf, ok := strings.Cut(rel, string(filepath.Separator))
	if !ok || parent == "" || leaf == "" || strings.ContainsRune(leaf, filepath.Separator) {
		return base
	}
	if parent == "." || parent == ".." || strings.HasPrefix(leaf, ".") {
		return base
	}
	return parent + "-" + leaf
}

// managedWorktreePaths returns the absolute path of every managed
// worktree, sorted, by scanning `<worktreesRoot>/*/*`. Forks nothing, so
// it's cheap enough to call per request from resolveRepoRef; deciding
// whether an entry is a *healthy* worktree costs a git call and is left
// to listManagedWorktrees.
func managedWorktreePaths() []string {
	if worktreesRoot == "" {
		return nil
	}
	repos, err := os.ReadDir(worktreesRoot)
	if err != nil {
		return nil
	}
	var out []string
	for _, repo := range repos {
		if !repo.IsDir() || strings.HasPrefix(repo.Name(), ".") {
			continue
		}
		parent := filepath.Join(worktreesRoot, repo.Name())
		leaves, err := os.ReadDir(parent)
		if err != nil {
			continue
		}
		for _, leaf := range leaves {
			if !leaf.IsDir() || strings.HasPrefix(leaf.Name(), ".") {
				continue
			}
			out = append(out, filepath.Join(parent, leaf.Name()))
		}
	}
	slices.Sort(out)
	return out
}

// allManagedRefs returns a repoRef for every repo and worktree webdiff
// manages, for callers that need to search by something other than the
// URL slug (a tmux session name, typically).
func allManagedRefs() []repoRef {
	var out []repoRef
	if entries, err := os.ReadDir(rootDir); err == nil {
		for _, e := range entries {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			abs := filepath.Join(rootDir, e.Name())
			if !isGitRepo(abs) {
				continue
			}
			out = append(out, repoRef{
				abs:  abs,
				kind: "repos",
				name: e.Name(),
				url:  "/repos/" + e.Name() + "/",
			})
		}
	}
	for _, p := range managedWorktreePaths() {
		slug := worktreeSlug(p)
		out = append(out, repoRef{
			abs:  p,
			kind: "worktrees",
			name: slug,
			url:  "/worktrees/" + slug + "/",
		})
	}
	return out
}

// listManagedWorktrees returns one entry per managed worktree, sorted by
// path. Healthy worktrees have a parent repo we can name; broken ones
// fall back to "" and render greyed out on the home page.
func listManagedWorktrees() []worktreeEntry {
	var out []worktreeEntry
	for _, path := range managedWorktreePaths() {
		repo, ok := worktreeParentRepo(path)
		out = append(out, worktreeEntry{
			Name:   worktreeSlug(path),
			Path:   path,
			Repo:   repo,
			Broken: !ok,
		})
	}
	return out
}

// worktreeParentRepo returns the basename of the repo a worktree
// belongs to, derived from `git rev-parse --git-common-dir`. That
// command prints the absolute path to the parent repo's .git
// directory; its parent is the repo's top-level. ok=false means git
// couldn't make sense of the directory — typically because the parent
// repo has been deleted, leaving the worktree's gitfile dangling.
func worktreeParentRepo(wtPath string) (string, bool) {
	out, err := exec.Command("git", "-C", wtPath, "rev-parse", "--git-common-dir").Output()
	if err != nil {
		return "", false
	}
	common := strings.TrimSpace(string(out))
	if common == "" {
		return "", false
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(wtPath, common)
	}
	// --git-common-dir points at <parentRepo>/.git; the parent of that
	// is the repo's top-level dir whose basename we want to display.
	return filepath.Base(filepath.Dir(common)), true
}

// writeTmuxSection appends a "sessions" block to the root listing: every
// live pane on the host, followed by agent sessions we remember as having
// been running but which are no longer in tmux (see stoppedSessions).
// Each live row links to the streaming page for that pane so the reviewer
// can peek at session state without having to be the one who kicked off a
// review-comment send; each stopped row links to its archived scrollback.
// Hidden entirely when there's nothing in either group so the page stays
// clean on hosts that don't use tmux.
//
// Live rows are per-pane and labelled "session:window.pane"; stopped rows
// are per-session and labelled by name, because a session that's gone has
// no pane left to name.
func writeTmuxSection(b *strings.Builder, ctx context.Context) {
	panes, err := listAllPanes(ctx)
	if err != nil {
		panes = nil
	}
	// Liveness comes from the panes we just listed rather than the
	// watcher's maps: those are populated by a 1 Hz poll, so a session
	// that's live but not yet polled (or polled since a webdiff restart)
	// would render as both a live and a stopped row. This also gets the
	// tmux-down case right — listAllPanes returns no panes, so everything
	// we remember correctly reads as stopped.
	liveNames := make(map[string]bool, len(panes))
	for _, p := range panes {
		liveNames[p.Session] = true
	}
	stopped := stoppedSessions(liveNames)
	if len(panes) == 0 && len(stopped) == 0 {
		return
	}
	b.WriteString(`<h2 class="section-title">sessions</h2>`)
	b.WriteString(`<div class="dir-list">`)
	now := time.Now()
	for _, p := range panes {
		label := p.Session + ":" + p.Index
		desc := p.Command
		if p.Title != "" && p.Title != p.Command {
			desc += " · " + p.Title
		}
		// /pane is the arbitrary-pane viewer; the handler looks up the
		// session name from the paneID at request time so we don't need
		// to pass it as a query param.
		href := "/pane?paneID=" + url.QueryEscape(p.PaneID)
		fmt.Fprintf(b, `<a class="dir-row tmux-row" href="%s" data-session="%s"><span class="dir-name">%s</span><span class="dir-stats">%s</span><span class="dir-branch">%s</span></a>`,
			template.HTMLEscapeString(href),
			template.HTMLEscapeString(p.Session),
			template.HTMLEscapeString(label),
			template.HTMLEscapeString(desc),
			template.HTMLEscapeString(formatLastUsed(p.LastUsed, now)),
		)
	}
	// No data-session on stopped rows: app.js selects `.tmux-row
	// [data-session]` for the spinner poll, so leaving it off keeps them
	// out of it without the JS needing to know they exist.
	// Columns line up with the live rows above: name, description, then
	// time on the right. "stopped" sits where a live row shows its running
	// command, which is exactly the distinction being drawn.
	for _, m := range stopped {
		desc := "stopped"
		if m.Branch != "" {
			desc += " · " + m.Branch
		}
		fmt.Fprintf(b, `<a class="dir-row tmux-row session-stopped" href="%s"><span class="dir-name">%s</span><span class="dir-stats">%s</span><span class="dir-branch">%s</span></a>`,
			template.HTMLEscapeString("/history/"+m.ID),
			template.HTMLEscapeString(m.Session),
			template.HTMLEscapeString(desc),
			template.HTMLEscapeString(formatLastUsed(m.Updated, now)),
		)
	}
	b.WriteString(`</div>`)
}

// formatLastUsed renders a tmux activity timestamp as a short relative
// string for the listing's right column. Empty when ts is zero so the
// "no data" case (older tmux, or unparseable field) doesn't show a
// misleading "55y" relative to the unix epoch.
func formatLastUsed(ts int64, now time.Time) string {
	if ts == 0 {
		return ""
	}
	d := now.Sub(time.Unix(ts, 0))
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// renderRepo renders the diff view for one repoRef (a regular repo or
// a managed worktree). The kind on the ref drives the small chrome
// differences between the two: only repos get the "+ worktree"
// button; only worktrees get the "remove" button.
func renderRepo(w http.ResponseWriter, r *http.Request, ref repoRef) {
	repo := ref.abs
	_, hasUpstream := upstreamRef(repo)

	// The chosen diff mode lives in the URL (?mode=working|branch|remote).
	// Default is HEAD (working). app.js handles persistence via
	// localStorage + redirect-on-load when no mode is set in the URL but
	// one is stored — so within an in-app navigation flow the URL always
	// carries the user's choice without us needing a server-side state file.
	mode := r.URL.Query().Get("mode")
	if mode == "remote" && !hasUpstream {
		mode = "branch"
	}
	if mode != "working" && mode != "branch" && mode != "remote" {
		mode = "working"
	}

	// `context=full` switches git diff to -U<MAX> so each file diff shows
	// the entire file with hunks inlined. Per-view toggle, not threaded
	// through other links — full context is rarely what you want when
	// navigating somewhere else.
	fullContext := r.URL.Query().Get("context") == "full"

	base, baseLabel := diffBase(repo, mode)

	env, cleanup := diffEnv(repo)
	defer cleanup()

	files := changedFiles(env, repo, base)

	cols := 120
	if c := r.URL.Query().Get("cols"); c != "" {
		fmt.Sscanf(c, "%d", &cols)
	}

	urlPath := ref.url
	modeQS := modeQuery(mode)

	// Agent header button — always visible. When the repo's
	// webdiff-<base> session is already running it short-circuits
	// straight to /stream<repoUrl>; otherwise it points at
	// /agent<repoUrl>, which spawns the session and 303-redirects to
	// the same place. The dashed-border "to-be-created" treatment (via
	// add-comment-btn) signals the click will spend ~5 s spawning a new
	// pane rather than instantly attaching to a live one. Both URLs are
	// path-based: each repo maps 1-1 to its tmux session, so neither
	// side needs to thread a paneID through the URL.
	paneID, _, liveAgent := repoStreamPane(r.Context(), repo)

	// Which agent this page's copy names. A running session wins — it's
	// a fact, not a preference — otherwise it's the `?agent=` the spawn
	// picker will act on, defaulting to the -agent flag's value.
	agentPick := r.URL.Query().Get("agent")
	if paneID != "" && liveAgent != "" {
		agentPick = liveAgent
	}
	pageAgent, _ := resolveAgent(agentPick)

	var agentHref, agentClass, agentTitle string
	if paneID != "" {
		agentHref = "/stream" + urlPath
		agentClass = "toggle"
		agentTitle = "open the running " + pageAgent + " session for this repo"
	} else {
		agentHref = "/agent" + urlPath + agentQuery(pageAgent)
		agentClass = "toggle add-comment-btn"
		agentTitle = "start a " + pageAgent + " session for this repo"
	}

	var stats string
	if len(files) > 0 {
		nfiles, ins, del := diffStat(env, repo, base)
		if nfiles > 0 {
			noun := "files"
			if nfiles == 1 {
				noun = "file"
			}
			var sb strings.Builder
			sb.WriteString(`<span class="stats">`)
			fmt.Fprintf(&sb, `<span class="stats-files">%d %s</span>`, nfiles, noun)
			// The counts live in `data-stat` and are rendered from CSS
			// pseudo-content (see .stats-ins/.stats-del) so no digits sit
			// in a text node for iOS Safari's data detector to turn into
			// a tap-to-dial phone link.
			if ins > 0 {
				fmt.Fprintf(&sb, ` <span class="stats-ins" data-stat="+%d"></span>`, ins)
			}
			if del > 0 {
				fmt.Fprintf(&sb, ` <span class="stats-del" data-stat="-%d"></span>`, del)
			}
			sb.WriteString(`</span>`)
			stats = sb.String()
		}
	}

	var body strings.Builder
	// Sticky page heading: row 1 is title (left) + branch/stats meta
	// (right, smaller font); row 2 is the home/agent nav (left) +
	// mode/context/expand controls (right). Everything that used to
	// live in a separate info bar is folded in here so only the
	// streaming diff body scrolls.
	// Title is the repo name. A worktree's leaf dir is its branch, which
	// is redundant next to the branch shown in the meta; show its parent
	// repo instead so the heading reads "repo … branch" like the home-page
	// rows do. Broken worktrees have no resolvable parent — fall back to
	// the slug, which at least identifies which worktree you're looking at.
	title := worktreeSlug(repo)
	if ref.kind == "worktrees" {
		if parent, ok := worktreeParentRepo(repo); ok {
			title = parent
		}
	}
	body.WriteString(`<div class="page-heading"><div class="page-heading-row">`)
	body.WriteString(`<span class="branch">`)
	writeTitle(&body, title, repo)
	body.WriteString(`</span>`)
	body.WriteString(`<span class="head-meta">`)
	fmt.Fprintf(&body, `<span class="head-branch">%s</span>`, template.HTMLEscapeString(currentBranch(repo)))
	body.WriteString(stats)
	body.WriteString(`</span></div>`)

	// Row 2 scrolls horizontally on narrow viewports rather than
	// wrapping. Order on the right side puts mode-select first
	// (most-used), then expand-all, then full-context — full-context is
	// the rare button so it's the one that drops off-screen first when
	// space runs out.
	body.WriteString(`<div class="page-heading-row page-heading-row-scroll">`)
	body.WriteString(`<div class="header-buttons">`)
	fmt.Fprintf(&body, `<a class="toggle" href="/%s">home</a>`, modeQS)
	fmt.Fprintf(&body, `<a class="%s" id="agent-btn" href="%s" title="%s">%s</a>`,
		agentClass, template.HTMLEscapeString(agentHref), template.HTMLEscapeString(agentTitle), template.HTMLEscapeString(pageAgent))
	// The picker only makes sense before a session exists: once one is
	// running the button just attaches to it, and switching agents is
	// the restart modal's job. app.js rewrites #agent-btn's href on
	// change so this stays a plain navigation.
	if paneID == "" {
		writeAgentSelect(&body, "agent-select", pageAgent)
	}
	fmt.Fprintf(&body, `<a class="toggle" href="/file/%s/%s/" title="browse repo files">files</a>`,
		ref.kind, template.HTMLEscapeString(ref.name))
	writeOpenButtons(&body, ref.abs, "toggle")
	switch ref.kind {
	case "repos":
		fmt.Fprintf(&body, `<button class="toggle add-comment-btn" type="button" id="wt-new-btn" data-repo="%s" title="create a worktree of this repo">+ worktree</button>`,
			template.HTMLEscapeString(ref.name))
	case "worktrees":
		fmt.Fprintf(&body, `<button class="toggle danger" type="button" id="wt-remove-btn" data-name="%s" title="remove this worktree">remove</button>`,
			template.HTMLEscapeString(ref.name))
	}
	body.WriteString(`</div>`)

	body.WriteString(`<div class="header-buttons">`)
	body.WriteString(`<select id="diff-mode-select">`)
	for _, opt := range []struct{ val, label string }{
		{"working", "uncommitted"},
		{"branch", "branch"},
		{"remote", "remote"},
	} {
		sel, dis := "", ""
		if opt.val == mode {
			sel = ` selected`
		}
		if opt.val == "remote" && !hasUpstream {
			dis = ` disabled`
		}
		fmt.Fprintf(&body, `<option value="%s"%s%s>%s</option>`, opt.val, sel, dis, opt.label)
	}
	body.WriteString(`</select>`)
	if len(files) > 0 {
		body.WriteString(`<button class="toggle" type="button" id="toggle-all">expand all</button>`)
		ctxLabel := "full context"
		if fullContext {
			ctxLabel = "diff only"
		}
		fmt.Fprintf(&body, `<button class="toggle" type="button" id="toggle-context" data-full="%t">%s</button>`,
			fullContext, ctxLabel)
	}
	// Sync buttons: hand-off to the desktop client's /_local/sync mux.
	// The browser is reverse-proxied through the client, so a relative
	// POST lands on the desktop, never on the sandbox. data-sync-dir
	// is the only differentiator the JS needs. Only emit them when the
	// request came through the client's proxy (it tags requests with
	// X-Webdiff-Client) — on direct web-app access the /_local/sync
	// endpoints don't exist, so the buttons would be dead.
	if r.Header.Get("X-Webdiff-Client") != "" {
		fmt.Fprintf(&body, `<button class="toggle" type="button" data-sync-dir="pull" data-kind="%s" data-name="%s" title="pull sandbox state into this desktop repo">&#x2193; pull</button>`,
			template.HTMLEscapeString(ref.kind), template.HTMLEscapeString(ref.name))
		fmt.Fprintf(&body, `<button class="toggle" type="button" data-sync-dir="push" data-kind="%s" data-name="%s" title="push this desktop repo into the sandbox">&#x2191; push</button>`,
			template.HTMLEscapeString(ref.kind), template.HTMLEscapeString(ref.name))
	}
	body.WriteString(`</div></div>`)

	// Worktree create form — hidden until "+ worktree" is clicked.
	// Inline rather than modal so it works the same on mobile Safari
	// as on desktop, and stays visually attached to the button that
	// reveals it. data-repo on the form picks up which repo the
	// worktree-add call should target.
	if ref.kind == "repos" {
		fmt.Fprintf(&body, `<div class="page-heading-row wt-create-row" id="wt-create-row" data-repo="%s" hidden>`,
			template.HTMLEscapeString(ref.name))
		body.WriteString(`<input type="text" id="wt-branch" class="wt-branch-input" placeholder="branch name (e.g. tongue/foo)" autocomplete="off" autocapitalize="off" autocorrect="off" spellcheck="false">`)
		body.WriteString(`<label class="wt-from-main"><input type="checkbox" id="wt-from-main" checked> from main</label>`)
		body.WriteString(`<button type="button" class="toggle send-btn" id="wt-create-submit">create</button>`)
		body.WriteString(`<button type="button" class="toggle" id="wt-create-cancel">cancel</button>`)
		body.WriteString(`</div>`)
	}
	body.WriteString(`</div>`)

	// send-comments is always visible (no `hidden`) so the modal — which
	// owns the "+ overall review comment" composer in the new layout —
	// is reachable even before any line/file comments are queued. The
	// floating bottom-right chip just opens the modal; the modal's
	// footer button does the actual send and is the one that
	// disables/enables based on comment count.
	fmt.Fprintf(&body, `<div class="send-bar"><button class="toggle send-btn" type="button" id="send-comments">send to %s (<span class="send-count">0</span>)</button></div>`, template.HTMLEscapeString(pageAgent))

	if len(files) == 0 {
		msg := "No uncommitted changes."
		if baseLabel != "" {
			msg = "No changes since " + baseLabel + "."
		}
		fmt.Fprintf(&body, `<p style="color:#6c7086;padding:16px">%s</p>`, msg)
		writePageForAgent(w, body.String(), pageAgent)
		return
	}

	body.WriteString(`<nav class="pillbar">`)
	for _, file := range files {
		slug := strings.ReplaceAll(file, "/", "-")
		slug = strings.ReplaceAll(slug, ".", "-")
		name := file
		if idx := strings.LastIndex(file, "/"); idx >= 0 {
			name = file[idx+1:]
		}
		fmt.Fprintf(&body, `<a class="pill" href="#file-%s">%s</a>`,
			template.HTMLEscapeString(slug), template.HTMLEscapeString(name))
	}
	body.WriteString(`</nav>`)

	for _, file := range files {
		canon, raw := fileDiff(env, repo, base, file, cols, fullContext)
		if len(raw) == 0 {
			continue
		}
		screen, _ := terminal.NewScreen(terminal.WithMaxSize(cols, 0))
		screen.Write(raw)
		heading, rest := extractHeading(screen.AsHTML())
		rendered := wrapDiffLines(rest, file, canon)

		added, removed, binary := fileDiffStat(env, repo, base, file)

		slug := strings.ReplaceAll(file, "/", "-")
		slug = strings.ReplaceAll(slug, ".", "-")

		display := file
		if heading != "" {
			display = heading
		}
		// data-file on <details> lets the JS look up the file's section
		// (and its <summary>) from a stored Comment without recomputing
		// the slug — important because the slug is lossy (slashes and
		// dots both collapse to dashes) and can't be inverted.
		fmt.Fprintf(&body, `<details id="file-%s" data-file="%s" open><summary><span class="filename">%s</span><span class="filestats">`,
			template.HTMLEscapeString(slug),
			template.HTMLEscapeString(file),
			template.HTMLEscapeString(display))
		switch {
		case binary:
			body.WriteString(`<span class="stats-files">binary</span>`)
		case added != "" || removed != "":
			fmt.Fprintf(&body, `<span class="stats-ins" data-stat="+%s"></span> <span class="stats-del" data-stat="-%s"></span>`,
				template.HTMLEscapeString(added), template.HTMLEscapeString(removed))
		}
		body.WriteString(`</span></summary><div class="term-container"><div class="term-inner">`)
		body.WriteString(rendered)
		body.WriteString(`</div></div></details>`)
	}

	writePageForAgent(w, body.String(), pageAgent)
}

func writePage(w http.ResponseWriter, body string) {
	writePageForAgent(w, body, agentName)
}

// writePageForAgent renders the page with data-agent-name set to agent
// rather than the global default, so client-side copy on a page tied to
// a live session names the agent actually running there.
func writePageForAgent(w http.ResponseWriter, body, agent string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := struct {
		StyleCSS    template.CSS
		Xterm256CSS template.CSS
		PaletteCSS  template.CSS
		IconSprite  template.HTML
		Body        template.HTML
		AgentName   string
	}{
		StyleCSS:    template.CSS(styleCSS),
		Xterm256CSS: template.CSS(xterm256),
		PaletteCSS:  template.CSS(paletteCSS),
		IconSprite:  template.HTML(iconSprite),
		Body:        template.HTML(body),
		AgentName:   agent,
	}
	if err := pageTmpl.Execute(w, data); err != nil {
		// Header is already written; logging is the best we can do.
		fmt.Fprintln(os.Stderr, "page template:", err)
	}
}

// serveAppJS hands out the embedded application JavaScript at
// /app.js. Cached briefly client-side — the source only changes when
// the binary is rebuilt and the user reloads.
func serveAppJS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write([]byte(appJS))
}

type gzipResponseWriter struct {
	http.ResponseWriter
	gz *gzip.Writer
}

func (w *gzipResponseWriter) Write(b []byte) (int, error) {
	return w.gz.Write(b)
}

// Flush satisfies http.Flusher so handlers that stream partial pages
// keep working under compression; the gzip writer has to be flushed
// first or the buffered bytes never reach the client.
func (w *gzipResponseWriter) Flush() {
	w.gz.Flush()
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// gzipMiddleware compresses browser-page responses; a big worktree diff
// page is several MB of highly repetitive HTML. /api/ is exempt: its
// payloads are small, and the SSE endpoints depend on each event hitting
// the wire as written — a compression buffer in that path would hold
// events back and break the live pane view.
func gzipMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") ||
			!strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Add("Vary", "Accept-Encoding")
		gz := gzip.NewWriter(w)
		defer gz.Close()
		next.ServeHTTP(&gzipResponseWriter{ResponseWriter: w, gz: gz}, r)
	})
}

// paneRepoURL derives the diff page URL from a tmux session label of
// the form "<slug>-wd" or "<slug>-wd:<window>" by searching every
// managed ref for the one whose repoSessionName matches (which also
// handles names containing "." or ":"). Returns "" when the label
// doesn't resolve to anything webdiff is currently managing.
func paneRepoURL(label string) string {
	session := strings.SplitN(label, ":", 2)[0]
	if !strings.HasSuffix(session, "-wd") {
		return ""
	}
	for _, ref := range allManagedRefs() {
		if repoSessionName(ref.abs) == session {
			return ref.url
		}
	}
	return ""
}

// renderRepoStreamPage renders the live tail of the tmux pane paired
// with one repo or worktree. Mounted at /stream/ as a prefix handler
// so the URL suffix is the diff URL (/repos/<name>/ or
// /worktrees/<name>/) — each ref maps 1-1 to its `<base>-wd` tmux
// session, so the server resolves the paneID from the ref instead of
// accepting it as a query param. If the session hasn't been spawned
// yet the request 303-redirects to /agent<refUrl>, which spawns it
// and bounces back here.
//
// Layout: sticky page heading with nav links, streaming term tail in
// the middle, and a bottom-fixed toolbar with esc / ↑ / ↓ / enter keys
// plus a growing textarea. The actual SSE consumption and auto-follow
// behaviour live in app.js; this handler just serves the chrome and
// the empty container the JS fills in.
func renderRepoStreamPage(w http.ResponseWriter, r *http.Request) {
	repoURL := strings.TrimPrefix(r.URL.Path, "/stream")
	ref, ok := resolveRepoRef(repoURL)
	if !ok || !isGitRepo(ref.abs) {
		http.NotFound(w, r)
		return
	}
	paneID, _, liveAgent := repoStreamPane(r.Context(), ref.abs)
	if paneID == "" {
		http.Redirect(w, r, "/agent"+ref.url+agentQuery(r.URL.Query().Get("agent")), http.StatusSeeOther)
		return
	}
	// What's in the pane is the truth; `?agent=` is only a fallback for
	// a session started with a command we can't attribute.
	if liveAgent == "" {
		liveAgent = r.URL.Query().Get("agent")
	}
	writeStreamPage(w, paneID, ref.url, "", liveAgent)
}

// renderArbitraryPanePage is the streaming view for tmux panes that
// aren't paired with a repo — opened from the home page's "tmux
// sessions" listing. The session name is looked up from paneID via
// tmux so the URL stays at just /pane?paneID=... (no label query
// param). The "diff" back-link is still shown when the pane's session
// matches a known webdiff repo or worktree (paneRepoURL), so clicking
// a `<base>-wd` row from the listing still gets the same chrome as
// the ref-paired view.
func renderArbitraryPanePage(w http.ResponseWriter, r *http.Request) {
	paneID := r.URL.Query().Get("paneID")
	if paneID == "" {
		http.Error(w, "paneID required", http.StatusBadRequest)
		return
	}
	out, err := tmuxOut(r.Context(), "display-message", "-t", paneID, "-p", "#{session_name}\t#{pane_start_command}")
	session, start, _ := strings.Cut(strings.TrimSpace(out), "\t")
	if err != nil || session == "" {
		writeStreamPage(w, paneID, "", paneID, "")
		return
	}
	writeStreamPage(w, paneID, paneRepoURL(session), session, agentFromStartCommand(start))
}

// writeStreamPage derives the heading from repoURL (so the title is the
// repo basename, matching the diff page) regardless of which entry
// point routed us here. fallbackTitle is only used for arbitrary panes
// that don't resolve to a known repo. liveAgent is the agent detected
// in the pane, "" when it isn't one we recognise.
func writeStreamPage(w http.ResponseWriter, paneID, repoURL, fallbackTitle, liveAgent string) {
	title := strings.Trim(repoURL, "/")
	if title == "" {
		title = fallbackTitle
	}
	// The remove button targets worktrees only; derive the slug from
	// repoURL ("/worktrees/<name>/") so both entry points — the
	// ref-paired /stream view and the arbitrary /pane view whose
	// paneRepoURL resolves to a worktree — get it identically.
	var worktreeName string
	if rest, ok := strings.CutPrefix(repoURL, "/worktrees/"); ok {
		worktreeName = strings.Trim(rest, "/")
	}
	var dir string
	if ref, ok := resolveRepoRef(repoURL); ok {
		dir = ref.abs
	}
	pageAgent, _ := resolveAgent(liveAgent)
	var body strings.Builder
	body.WriteString(`<div class="page-heading"><div class="page-heading-row">`)
	body.WriteString(`<span class="branch">`)
	writeTitle(&body, title, dir)
	body.WriteString(`</span>`)
	body.WriteString(`</div><div class="header-buttons">`)
	body.WriteString(`<a class="toggle" href="/">home</a>`)
	if repoURL != "" {
		fmt.Fprintf(&body, `<a class="toggle" href="%s">diff</a>`, template.HTMLEscapeString(repoURL))
	}
	if worktreeName != "" {
		fmt.Fprintf(&body, `<button class="toggle danger" type="button" id="wt-remove-btn" data-name="%s" title="remove this worktree">remove</button>`,
			template.HTMLEscapeString(worktreeName))
	}
	body.WriteString(`<div class="header-buttons-right">`)
	body.WriteString(`<button class="toggle" type="button" id="jump-bottom" hidden>jump to bottom</button>`)
	if repoURL != "" {
		// Restart is webdiff-managed-only: server gates on the `-wd`
		// session suffix, and we hide the button when there's no repo
		// URL to navigate to after the kill.
		//
		// The agent ids ride along as data attributes so the modal's
		// drop-down can be built without a second request: data-agent-id
		// is what's running now (the default the modal opens on),
		// data-agent-choices is everything selectable.
		fmt.Fprintf(&body, `<button class="toggle danger" type="button" id="restart-session" data-repo-url="%s" data-agent-id="%s" data-agent-choices="%s">restart session</button>`,
			template.HTMLEscapeString(repoURL),
			template.HTMLEscapeString(pageAgent),
			template.HTMLEscapeString(strings.Join(agentChoices(), ",")))
	}
	body.WriteString(`</div></div></div>`)
	fmt.Fprintf(&body, `<div id="pane-tail" data-pane-id="%s" class="term-container"><div class="term-inner"></div></div>`,
		template.HTMLEscapeString(paneID))
	body.WriteString(`<div class="stream-toolbar">`)
	body.WriteString(`<div class="stream-toolbar-row">`)
	body.WriteString(`<button class="toggle stream-key" type="button" data-key="Escape" title="send Escape">esc</button>`)
	body.WriteString(`<button class="toggle stream-key" type="button" data-key="Up" title="send up arrow">↑</button>`)
	body.WriteString(`<button class="toggle stream-key" type="button" data-key="Down" title="send down arrow">↓</button>`)
	body.WriteString(`<button class="toggle stream-key" type="button" data-key="Enter" title="send Enter">enter</button>`)
	body.WriteString(`</div>`)
	body.WriteString(`<div class="stream-attachments" id="stream-attachments" hidden></div>`)
	body.WriteString(`<div class="stream-toolbar-row">`)
	body.WriteString(`<button class="toggle stream-attach" type="button" id="pane-attach" title="attach a large paste or file">📎</button>`)
	body.WriteString(`<textarea id="pane-msg" class="stream-msg" rows="1" placeholder="message…" autocomplete="off"></textarea>`)
	body.WriteString(`<button class="toggle stream-send" type="button" id="pane-send">send</button>`)
	body.WriteString(`</div>`)
	body.WriteString(`</div>`)
	writePageForAgent(w, body.String(), pageAgent)
}

// computeXterm256CSS emits per-index .term-fgxN / .term-bgxN rules for
// the 256-colour palette. Indices 0–15 reference --color-N custom
// properties from style.css (so the user's palette.css override flows
// through); 16–231 are the 6×6×6 RGB cube and 232–255 are the 24-step
// grey ramp. Used once at init via the xterm256 package var.
func computeXterm256CSS() string {
	colors := make([]string, 256)
	for i := 0; i < 16; i++ {
		colors[i] = fmt.Sprintf("var(--color-%d)", i)
	}
	for i := 16; i < 232; i++ {
		idx := i - 16
		colors[i] = fmt.Sprintf("rgb(%d,%d,%d)", (idx/36)*51, ((idx%36)/6)*51, (idx%6)*51)
	}
	for i := 232; i < 256; i++ {
		v := 8 + (i-232)*10
		colors[i] = fmt.Sprintf("rgb(%d,%d,%d)", v, v, v)
	}
	var b strings.Builder
	for i, c := range colors {
		fmt.Fprintf(&b, ".term-fgx%d{color:%s}.term-bgx%d{background:%s}\n", i, c, i, c)
	}
	return b.String()
}

func main() {
	agentFlag := flag.String("agent", "claude", "agent command spawned in each repo's tmux pane; the first whitespace-separated token is used as the display name")
	sshHostFlag := flag.String("ssh-host", "", "how the browser's machine reaches this one over SSH (e.g. an ~/.ssh/config alias); defaults to this node's MagicDNS name")
	flag.Parse()
	args := flag.Args()

	if len(args) > 0 && args[0] == "palette" {
		if err := runPalette(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if len(args) > 0 && args[0] == "client" {
		if err := runClient(args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	agentCmd = strings.TrimSpace(*agentFlag)
	if agentCmd == "" {
		agentCmd = "claude"
	}
	if i := strings.IndexAny(agentCmd, " \t"); i > 0 {
		agentName = agentCmd[:i]
	} else {
		agentName = agentCmd
	}

	port := "9418"
	if len(args) > 0 {
		port = args[0]
	}

	paletteCSS = loadPalette()

	wd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot determine working directory:", err)
		os.Exit(1)
	}
	rootDir = wd
	if isGitRepo(rootDir) {
		fmt.Fprintln(os.Stderr, "webdiff must be run from a directory containing git repos, not from inside a git repo itself")
		os.Exit(1)
	}

	if ucd, err := os.UserCacheDir(); err == nil {
		cacheRoot = filepath.Join(ucd, "webdiff")
	} else {
		cacheRoot = filepath.Join(os.TempDir(), "webdiff-cache")
	}
	repoCacheRoot = filepath.Join(cacheRoot, "repos")
	indexCache = gitindex.New(repoCacheRoot)
	attachmentsRoot = filepath.Join(cacheRoot, "attachments")
	historyRoot = filepath.Join(cacheRoot, "history")
	worktreesRoot = filepath.Join(rootDir, "wt")
	for _, d := range []string{cacheRoot, repoCacheRoot, attachmentsRoot, historyRoot, worktreesRoot} {
		if err := os.MkdirAll(d, 0700); err != nil {
			fmt.Fprintln(os.Stderr, "cannot create cache dir:", err)
			os.Exit(1)
		}
	}
	if isGitRepo(worktreesRoot) {
		fmt.Fprintf(os.Stderr, "%s is a git repo, but webdiff needs it for managed worktrees\n", worktreesRoot)
		os.Exit(1)
	}

	auth, err := newOwnerAuth(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, "tailscale identity check failed:", err)
		os.Exit(1)
	}

	sshHost = strings.TrimSpace(*sshHostFlag)
	if sshHost == "" {
		sshHost = auth.dnsName
	}

	statsManager, err = repostats.New(repostats.Config{
		RootDir: rootDir, WorktreesRoot: worktreesRoot,
	}, indexCache)
	if err != nil {
		fmt.Fprintln(os.Stderr, "repo stats:", err)
		os.Exit(1)
	}
	if err := statsManager.Start(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "initial repo stats:", err)
		os.Exit(1)
	}

	browserMux := http.NewServeMux()
	browserMux.HandleFunc("/app.js", serveAppJS)
	browserMux.HandleFunc("/stream/", renderRepoStreamPage)
	browserMux.HandleFunc("/pane", renderArbitraryPanePage)
	browserMux.HandleFunc("/agent/", handleStartSession)
	browserMux.HandleFunc("/repos/", renderRepoDiff)
	browserMux.HandleFunc("/worktrees/", renderRepoDiff)
	browserMux.HandleFunc("/file/", renderFileBrowser)
	browserMux.HandleFunc("/history/", handleHistory)
	browserMux.HandleFunc("/", renderHomeHandler)

	startSessionWatcher(context.Background())

	apiMux := http.NewServeMux()
	registerAgentRoutes(apiMux)
	registerSyncRoutes(apiMux)

	// safeweb's DefaultCSP is strict-by-default; everything except
	// same-origin URLs is blocked. The application JavaScript is now
	// served from /app.js so script-src can stay strict ('self' only
	// — no 'unsafe-inline' needed). The three CSS blocks are still
	// inlined into the page, so style-src allows inline via the
	// CSPAllowInlineStyles config flag below.
	csp := safeweb.DefaultCSP()
	srv, err := safeweb.NewServer(safeweb.Config{
		BrowserMux:           browserMux,
		APIMux:               apiMux,
		CSP:                  csp,
		CSPAllowInlineStyles: true,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "safeweb:", err)
		os.Exit(1)
	}

	// Bind to each Tailscale IP rather than 0.0.0.0. The auth
	// middleware already gates by Tailscale identity, but binding
	// only to the Tailscale interface means non-Tailscale traffic
	// (other LAN interfaces, public addresses if any, processes on
	// localhost run by other Unix users) never reaches us in the
	// first place — defence in depth. Tailscale typically assigns
	// one IPv4 in 100.64.0.0/10 plus one IPv6 in fd7a::/8; we listen
	// on each so peers can reach us over either family.
	listeners := make([]net.Listener, 0, len(auth.ips))
	addrs := make([]string, 0, len(auth.ips))
	for _, ip := range auth.ips {
		addr := net.JoinHostPort(ip.String(), port)
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "listen %s: %v\n", addr, err)
			os.Exit(1)
		}
		listeners = append(listeners, ln)
		addrs = append(addrs, addr)
	}
	fmt.Printf("Serving on %s (root: %s, repos: %s, worktrees: %s, owner: %d)\n",
		strings.Join(addrs, ", "), rootDir, repoCacheRoot, worktreesRoot, auth.ownerID)
	// Wrap safeweb's handler with the owner check so the auth gate runs
	// before any security-headers / CSRF / mux dispatch logic. gzip sits
	// inside the auth gate so unauthenticated requests never reach it.
	httpSrv := &http.Server{Handler: auth.middleware(gzipMiddleware(srv))}
	errCh := make(chan error, len(listeners))
	for _, ln := range listeners {
		go func(l net.Listener) { errCh <- httpSrv.Serve(l) }(ln)
	}
	if err := <-errCh; err != nil {
		fmt.Fprintln(os.Stderr, "serve:", err)
		os.Exit(1)
	}
}
