package main

import (
	"cmp"
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

//go:embed assets/favicon.svg
var faviconSVG string

//go:embed assets/favicon-progress.svg
var faviconProgressSVG string

//go:embed assets/favicon-complete.svg
var faviconCompleteSVG string

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

// defaultBranchPrefix is prepopulated when the user creates a worktree. It is
// also hidden from branch names on the home page and from managed worktree
// directory names.
var defaultBranchPrefix string

// onMainRepos holds the names of the repos under rootDir that are
// developed directly on main, set from -develop-on-main. The home page
// shows them above the repos section, with their branch and diff stats.
// Work in all other repos happens in worktrees, so the home page shows
// those repos only with buttons to start a worktree or an agent, and
// their agent sessions (see writeRepoActionsGroup).
var onMainRepos = map[string]bool{}

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

func indexKey(repo string) string {
	kind := "repos"
	if root, err := filepath.Abs(worktreesRoot); err == nil {
		if rel, relErr := filepath.Rel(root, repo); relErr == nil && rel != "." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			kind = "worktrees"
		}
	}
	return kind + "-" + worktreeSlug(repo)
}

// branchBase returns the merge-base SHA between HEAD and the main
// branch ("main" or "master"). It tries the refs from repostats.MainRefs
// in order, so <remote>/main (remote resolved via defaultRemote, so
// repos whose sole remote is "ro" rather than "origin" work) comes
// before a local "main" branch, which is often stale relative to the
// remote and would otherwise inflate the diff with hundreds of upstream
// commits. Falls back to "HEAD" when no candidate resolves, giving the
// original working-copy-only behaviour.
func branchBase(repo string) (sha, label string) {
	remote, _ := defaultRemote(repo)
	for _, ref := range repostats.MainRefs(remote) {
		out, err := exec.Command("git", "-C", repo, "merge-base", "HEAD", ref).Output()
		if err == nil {
			if s := strings.TrimSpace(string(out)); s != "" {
				return s, ref[strings.LastIndex(ref, "/")+1:]
			}
		}
	}
	return "HEAD", ""
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

// firstChangedLine returns the line in the new version of the file at
// which the first change of canon starts. A deleted line has no line in
// the new version, so for a deletion it returns the line that is now
// where the deleted lines were.
func firstChangedLine(canon []diffLine) int {
	next := 1
	for _, l := range canon {
		switch l.kind {
		case ' ':
			next = l.newLine + 1
		case '+':
			return l.newLine
		case '-':
			return next
		}
	}
	return 1
}

// isFileOnDisk reports whether file, relative to dir, is a regular file.
func isFileOnDisk(dir, file string) bool {
	fi, err := os.Stat(filepath.Join(dir, filepath.FromSlash(file)))
	return err == nil && fi.Mode().IsRegular()
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
// URL namespaces (/repos/, /worktrees/) and the home page's
// sections (repos and worktrees, each with their sessions, then the
// other sessions).
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

	var body strings.Builder
	body.WriteString(`<div class="header"><div class="header-left">`)
	fmt.Fprintf(&body, `<span class="branch">%s</span>`, template.HTMLEscapeString(filepath.Base(rootDir)))
	fmt.Fprintf(&body, `<span class="workdir">%s</span>`, template.HTMLEscapeString(tildePath(rootDir)))
	body.WriteString(`</div><div class="header-right">`)
	body.WriteString(`<a class="toggle" href="/history/" title="archived agent sessions">history</a>`)
	body.WriteString(`</div></div>`)

	// Each live or stopped session shows under the repo or worktree it
	// belongs to. The sessions section shows only the sessions that are
	// left. The map holds a list for each checkout, keyed by the name of
	// its session 1 (see sessionGroupKey), so a checkout can show all of
	// its sessions.
	sessions := homeSessions(r.Context(), time.Now())
	bySession := make(map[string][]homeSession, len(sessions))
	for _, s := range sessions {
		key := sessionGroupKey(s.Session)
		bySession[key] = append(bySession[key], s)
	}
	takeSessions := func(target repostats.Target) []homeSession {
		name := repoSessionName(target.Path)
		rows := bySession[name]
		delete(bySession, name)
		return checkoutSessionRows(rows, target.Path)
	}

	// Repos that are developed on main come first. The other repos
	// follow, without their branch and diff stats, because their work
	// happens in worktrees.
	var onMain, repos, worktrees []repostats.Target
	for _, target := range targets {
		switch {
		case target.Kind == repostats.Repo && onMainRepos[target.Name]:
			onMain = append(onMain, target)
		case target.Kind == repostats.Repo:
			repos = append(repos, target)
		case target.Kind == repostats.Worktree:
			worktrees = append(worktrees, target)
		}
	}

	// The repos that are developed on main show above all sections,
	// without a heading, because they are where most of the work is.
	if len(onMain) > 0 {
		body.WriteString(`<div class="dir-list on-main-list">`)
		for _, target := range onMain {
			writeDirGroup(&body, target, target.Name, takeSessions(target))
		}
		body.WriteString(`</div>`)
	}

	if len(repos) > 0 || len(onMain) == 0 {
		writeSectionStart(&body, "repos", "repos", len(repos))
		for _, target := range repos {
			writeRepoActionsGroup(&body, target, takeSessions(target))
		}
		if len(repos) == 0 {
			body.WriteString(`<p class="empty">No git repos under this directory.</p>`)
		}
		writeSectionEnd(&body)
	}

	// Worktrees show in groups by branch, so a branch that has a
	// worktree in more than one repo shows as one group. The group with
	// the most recent agent activity comes first. The worktrees with no
	// agent follow, the most recently created first.
	sortByAgentActivity(worktrees, bySession)
	if len(worktrees) > 0 {
		writeSectionStart(&body, "worktrees", "worktrees", len(worktrees))
		groups := groupByBranch(worktrees)
		lookupGroupPRs(r.Context(), groups)
		lookupGroupIssues(groups)
		for _, g := range groups {
			writeBranchGroup(&body, g, takeSessions)
		}
		writeSectionEnd(&body)
	}

	// Keep the order from homeSessions for the sessions that are left.
	var other []homeSession
	for _, s := range sessions {
		if _, ok := bySession[sessionGroupKey(s.Session)]; ok {
			other = append(other, s)
		}
	}
	if len(other) > 0 {
		writeSectionStart(&body, "sessions", "other sessions", len(other))
		for _, s := range other {
			writeSessionRow(&body, s, false)
		}
		writeSectionEnd(&body)
	}

	writePage(w, body.String())
}

// checkoutSessionRows prepares the session rows that show below the
// checkout at dir. A live row names its agent and directory, the same
// label that the Rex apps show (see sessionLabel). A stopped row of a
// session other than session 1 shows its session tag. The live sessions
// come first, in the order of their session numbers, then the stopped
// sessions in the same order.
func checkoutSessionRows(rows []homeSession, dir string) []homeSession {
	for i := range rows {
		n := sessionNumber(rows[i].Session)
		if rows[i].Stopped {
			if tag := sessionTag(n); tag != "" {
				rows[i].Desc += " " + tag
			}
		} else {
			rows[i].Desc = sessionLabel(rows[i].Agent, dir, n)
		}
	}
	slices.SortStableFunc(rows, func(a, b homeSession) int {
		if a.Stopped != b.Stopped {
			if a.Stopped {
				return 1
			}
			return -1
		}
		return sessionNumber(a.Session) - sessionNumber(b.Session)
	})
	return rows
}

// sortByAgentActivity puts the targets with the most recent agent
// activity first. The activity of a target is the latest LastUsed of its
// sessions in bySession, live or stopped. The targets with no session
// follow, the most recently created first (see repostats.Target.Created).
// Targets with the same activity time and the same creation time keep the
// name order from the stats snapshot.
func sortByAgentActivity(targets []repostats.Target, bySession map[string][]homeSession) {
	activity := make(map[string]int64, len(targets))
	for _, target := range targets {
		var latest int64
		for _, s := range bySession[repoSessionName(target.Path)] {
			latest = max(latest, s.LastUsed)
		}
		activity[target.Path] = latest
	}
	slices.SortStableFunc(targets, func(a, b repostats.Target) int {
		if c := cmp.Compare(activity[b.Path], activity[a.Path]); c != 0 {
			return c
		}
		return b.Created.Compare(a.Created)
	})
}

// writeSectionStart opens a home page section that the user can collapse.
// key identifies the section in app.js, which keeps the open or closed
// state in localStorage. The section has no id, because the diff page
// code in app.js uses details[id] for the file sections.
func writeSectionStart(b *strings.Builder, key, title string, count int) {
	fmt.Fprintf(b, `<details class="home-section" data-section="%s" open>`, template.HTMLEscapeString(key))
	fmt.Fprintf(b, `<summary class="section-title">%s<span class="section-count" data-stat="%d"></span></summary>`,
		template.HTMLEscapeString(title), count)
	b.WriteString(`<div class="dir-list">`)
}

func writeSectionEnd(b *strings.Builder) {
	b.WriteString(`</div></details>`)
}

// writeDirGroup emits a repo row and, below it, the rows of the sessions
// that belong to it.
func writeDirGroup(b *strings.Builder, target repostats.Target, label string, sessions []homeSession) {
	b.WriteString(`<div class="dir-group">`)
	writeDirRow(b, target, label)
	for _, s := range sessions {
		writeSessionRow(b, s, true)
	}
	b.WriteString(`</div>`)
}

// branchGroup is one group of the worktrees section on the home page:
// the worktrees that have the same branch, one for each repo.
type branchGroup struct {
	// Title is the branch without the configured prefix. For a worktree
	// with no known branch, or with a detached HEAD, it is the name of
	// the worktree directory.
	Title     string
	Worktrees []repostats.Target
	// PRs are the pull requests of the worktrees, in the order of
	// Worktrees. A worktree with no pull request has no entry.
	PRs []*pullRequest
	// Issues are the GitHub issues of the branch.
	Issues groupIssues
}

// lookupGroupPRs sets the PRs of each group.
func lookupGroupPRs(ctx context.Context, groups []branchGroup) {
	var queries []prQuery
	for _, g := range groups {
		for _, target := range g.Worktrees {
			queries = append(queries, prQuery{
				Dir:    target.Path,
				Branch: target.Branch,
			})
		}
	}
	found := prs.lookup(ctx, queries)
	for i := range groups {
		for range groups[i].Worktrees {
			if pr := found[0]; pr != nil {
				groups[i].PRs = append(groups[i].PRs, pr)
			}
			found = found[1:]
		}
	}
}

// groupByBranch puts the worktrees that have the same branch into one
// group. The groups and the worktrees in each group keep the order of
// targets. A worktree with no known branch, or with a detached HEAD, is
// in a group of its own.
func groupByBranch(targets []repostats.Target) []branchGroup {
	var groups []branchGroup
	index := make(map[string]int)
	for _, target := range targets {
		title := homeBranchName(target.Branch)
		key := "branch:" + title
		if target.Branch == "" || target.Branch == "HEAD" {
			title = filepath.Base(target.Path)
			key = "path:" + target.Path
		}
		i, ok := index[key]
		if !ok {
			i = len(groups)
			index[key] = i
			groups = append(groups, branchGroup{
				Title: title,
			})
		}
		groups[i].Worktrees = append(groups[i].Worktrees, target)
	}
	return groups
}

// writeBranchGroup emits one group of the worktrees section: the branch
// as the title, then for each worktree a row for its repo and the rows
// of its sessions. sessions returns the session rows of a worktree.
func writeBranchGroup(b *strings.Builder, g branchGroup, sessions func(repostats.Target) []homeSession) {
	b.WriteString(`<div class="dir-group branch-group">`)
	fmt.Fprintf(b, `<div class="branch-title"><span class="branch-title-name">%s</span>`, template.HTMLEscapeString(g.Title))
	// Show the repo of each pull request only when they are not all
	// of the same repo.
	withRepo := false
	for _, pr := range g.PRs {
		withRepo = withRepo || pr.Repo != g.PRs[0].Repo
	}
	for _, pr := range g.PRs {
		b.WriteString(prBadgeHTML(pr, withRepo))
	}
	writeGroupIssues(b, g.Issues)
	b.WriteString(`</div>`)
	for _, target := range g.Worktrees {
		label := target.ParentRepo
		if label == "" {
			label = target.Name
		}
		writeWorktreeRow(b, target, label)
		writeCommitList(b, target)
		for _, s := range sessions(target) {
			writeSessionRow(b, s, true)
		}
	}
	b.WriteString(`</div>`)
}

// writeRepoActionsGroup emits a repo that is not developed on main:
// one row with the repo name and inline buttons to start a worktree or
// an agent, and below it one row for each agent session of the repo.
// The rows show no branch or diff stats: work in these repos happens in
// worktrees, and the daily pull (see pull.go) keeps the repo on its main
// branch. The agent page of the repo links to its diff and file views.
//
// "+ agent" goes to /agent with ?new=1, which starts a new session with
// the lowest free session number. Thus it starts session 1 if no session
// runs, and another session if one or more run. Each session has its own
// row, which links to it.
func writeRepoActionsGroup(b *strings.Builder, target repostats.Target, sessions []homeSession) {
	formID := "wt-create-" + string(target.Kind) + "-" + target.Name
	b.WriteString(`<div class="dir-group"><div class="dir-row-wrap">`)
	b.WriteString(`<div class="dir-row repo-row repo-head"><span class="dir-name">`)
	writeTitle(b, target.Name, target.Path)
	b.WriteString(`</span></div><span class="repo-actions">`)
	fmt.Fprintf(b, `<button class="toggle add-comment-btn" type="button" data-wt-form="%s" title="create a worktree of %s">+ worktree</button>`,
		template.HTMLEscapeString(formID), template.HTMLEscapeString(target.Name))
	fmt.Fprintf(b, `<a class="toggle add-comment-btn" href="%s" title="start a new %s session in %s">+ agent</a>`,
		template.HTMLEscapeString(newAgentURL("/"+string(target.Kind)+"/"+target.Name+"/", "")),
		template.HTMLEscapeString(agentName), template.HTMLEscapeString(target.Name))
	b.WriteString(`</span>`)
	writeOpenButtons(b, target.Path, "dir-open")
	writeCopyButton(b, target.Path, "dir-copy")
	b.WriteString(`</div>`)
	writeHomeWorktreeForm(b, formID, target)
	for _, s := range sessions {
		writeSessionRow(b, s, true)
	}
	b.WriteString(`</div>`)
}

// writeDirRow emits one <a class="dir-row repo-row"> for a repo on the
// home page. label is what shows on the left; the checkout's full path
// shows in place of label on wide viewports. At the right are the diff
// stats and the branch. These values come from the authoritative
// repostats snapshot.
// A failed query hides stale stats and adds an unavailable label to that row.
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
func writeDirRow(b *strings.Builder, target repostats.Target, label string) {
	url := "/" + string(target.Kind) + "/" + target.Name + "/"
	cls := "dir-row repo-row"
	if target.Broken {
		cls += " dir-broken"
	}
	b.WriteString(`<div class="dir-row-wrap">`)
	fmt.Fprintf(b, `<a class="%s" href="%s"><span class="dir-name">`, cls, template.HTMLEscapeString(url))
	writeTitle(b, label, target.Path)
	b.WriteString(`</span><span class="dir-stats">`)
	if !target.Broken && target.Error == "" {
		writeSummary(b, target.AllChanges)
	}
	b.WriteString(`</span>`)
	if target.Broken {
		b.WriteString(`<span class="dir-branch dir-folder-tag">broken</span>`)
	} else if target.Error != "" {
		fmt.Fprintf(b, `<span class="dir-branch dir-folder-tag" title="%s">unavailable</span>`, template.HTMLEscapeString(target.Error))
	} else {
		fmt.Fprintf(b, `<span class="dir-branch">%s</span>`, template.HTMLEscapeString(homeBranchName(target.Branch)))
	}
	b.WriteString(`</a>`)
	writeOpenButtons(b, target.Path, "dir-open")
	writeCopyButton(b, target.Path, "dir-copy")
	b.WriteString(`</div>`)
}

// writeWorktreeRow emits the row of one worktree in a branch group. It
// looks like the row of writeDirRow, but at the right it shows the
// number of commits and a mark for uncommitted changes in place of the
// branch, because the title of the group shows the branch. One link to
// the diff page holds the repo name and the diff stats. The full row is
// not a link, because the commit count is a button that shows and hides
// the commit rows (see writeCommitList).
func writeWorktreeRow(b *strings.Builder, target repostats.Target, label string) {
	url := "/" + string(target.Kind) + "/" + target.Name + "/"
	cls := "dir-row repo-row"
	if target.Broken {
		cls += " dir-broken"
	}
	b.WriteString(`<div class="dir-row-wrap">`)
	fmt.Fprintf(b, `<div class="%s"><a class="dir-link" href="%s"><span class="dir-name">`, cls, template.HTMLEscapeString(url))
	writeTitle(b, label, target.Path)
	b.WriteString(`</span><span class="dir-stats">`)
	if !target.Broken && target.Error == "" {
		writeSummary(b, target.AllChanges)
	}
	b.WriteString(`</span></a>`)
	if target.Broken {
		b.WriteString(`<span class="dir-branch dir-folder-tag">broken</span>`)
	} else if target.Error != "" {
		fmt.Fprintf(b, `<span class="dir-branch dir-folder-tag" title="%s">unavailable</span>`, template.HTMLEscapeString(target.Error))
	} else {
		writeCommitState(b, target)
	}
	b.WriteString(`</div>`)
	writeOpenButtons(b, target.Path, "dir-open")
	writeCopyButton(b, target.Path, "dir-copy")
	b.WriteString(`</div>`)
}

// commitListID returns the id of the commit rows of target.
func commitListID(target repostats.Target) string {
	return "commits-" + string(target.Kind) + "-" + target.Name
}

// hasCommitList reports whether writeCommitList writes rows for target.
func hasCommitList(target repostats.Target) bool {
	return !target.Broken && (target.Commits > 0 || target.Dirty())
}

// writeCommitState emits the number of commits of target that are not
// on the main branch, then a mark if target has uncommitted changes.
// The mark element is always there, so that the commit counts of the
// rows in a group align. If target has commit rows, the count is a
// button that shows and hides them. app.js keeps the state of each
// button.
func writeCommitState(b *strings.Builder, target repostats.Target) {
	noun := "commits"
	if target.Commits == 1 {
		noun = "commit"
	}
	if hasCommitList(target) {
		fmt.Fprintf(b, `<button class="dir-commits commit-toggle" type="button" aria-expanded="false" aria-controls="%s" title="show or hide the commits">%d %s</button>`,
			template.HTMLEscapeString(commitListID(target)), target.Commits, noun)
	} else {
		fmt.Fprintf(b, `<span class="dir-commits">%d %s</span>`, target.Commits, noun)
	}
	if target.Dirty() {
		b.WriteString(`<span class="dir-dirty" title="uncommitted changes" aria-label="uncommitted changes">●</span>`)
	} else {
		b.WriteString(`<span class="dir-dirty"></span>`)
	}
}

// writeCommitList emits a row for each commit of target, the oldest
// first, and then a row for the uncommitted changes if there are some.
// Each commit row links to the commit view of its commit. The list is
// hidden until the commit count of the row above it shows it (see
// writeCommitState). A broken target, or a target with no commits and
// no changes, has no list.
func writeCommitList(b *strings.Builder, target repostats.Target) {
	if !hasCommitList(target) {
		return
	}
	refURL := "/" + string(target.Kind) + "/" + target.Name + "/"
	fmt.Fprintf(b, `<div class="commit-list" id="%s" hidden>`, template.HTMLEscapeString(commitListID(target)))
	if earlier := target.Commits - len(target.CommitList); earlier > 0 {
		noun := "commits"
		if earlier == 1 {
			noun = "commit"
		}
		fmt.Fprintf(b, `<div class="dir-row commit-row commit-more"><span class="dir-name">… %d earlier %s</span></div>`, earlier, noun)
	}
	for _, c := range target.CommitList {
		fmt.Fprintf(b, `<a class="dir-row commit-row" href="%s" title="view commit %s"><span class="commit-row-sha">%s</span><span class="dir-name">%s</span><span class="dir-stats">`,
			template.HTMLEscapeString(commitViewURL(refURL, 0, c.SHA)),
			template.HTMLEscapeString(c.SHA),
			template.HTMLEscapeString(c.SHA[:min(7, len(c.SHA))]),
			template.HTMLEscapeString(c.Subject))
		writeLineCounts(b, c.Summary)
		b.WriteString(`</span></a>`)
	}
	// The uncommitted changes have no view of their own, so the row is
	// not a link. A link with ?changes=working would change the saved
	// selection of the diff page.
	if target.Dirty() {
		b.WriteString(`<div class="dir-row commit-row commit-uncommitted"><span class="commit-row-sha">●</span><span class="dir-name">uncommitted</span><span class="dir-stats">`)
		writeLineCounts(b, target.Uncommitted)
		b.WriteString(`</span></div>`)
	}
	b.WriteString(`</div>`)
}

// writeLineCounts emits the inserted and deleted line counts of s. A
// count of zero does not show.
func writeLineCounts(b *strings.Builder, s repostats.Summary) {
	if s.Ins > 0 {
		fmt.Fprintf(b, `<span class="stats-ins" data-stat="+%d"></span>`, s.Ins)
	}
	if s.Ins > 0 && s.Del > 0 {
		b.WriteString(` `)
	}
	if s.Del > 0 {
		fmt.Fprintf(b, `<span class="stats-del" data-stat="-%d"></span>`, s.Del)
	}
}

// homeBranchName removes the configured prefix from a branch shown on the
// home page. Other branch labels remain complete.
func homeBranchName(branch string) string {
	return strings.TrimPrefix(branch, defaultBranchPrefix)
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

// writeWorktreeNewButton emits the "+ worktree" button. It shows and
// hides the form from writeWorktreeCreateRow, so a page that writes one
// must also write the other.
func writeWorktreeNewButton(b *strings.Builder) {
	b.WriteString(`<button class="toggle add-comment-btn" type="button" id="wt-new-btn" title="create a worktree from this checkout">+ worktree</button>`)
}

// writeNewAgentButton emits the "+ agent" link, which starts a new agent
// session in the checkout at refURL, beside the sessions that already
// run there. The new session runs agent.
func writeNewAgentButton(b *strings.Builder, refURL, agent string) {
	fmt.Fprintf(b, `<a class="toggle add-comment-btn" id="new-agent-btn" href="%s" title="start a new %s session in this checkout">+ agent</a>`,
		template.HTMLEscapeString(newAgentURL(refURL, agent)), template.HTMLEscapeString(agent))
}

// writeWorktreeCreateRow emits the worktree create form. It is hidden
// until "+ worktree" is clicked. It is inline rather than a modal, so it
// works the same on mobile Safari as on desktop, and it stays attached
// to the button that shows it. data-repo and data-kind tell the
// worktree-add call which checkout to use.
func writeWorktreeCreateRow(b *strings.Builder, ref repoRef) {
	fmt.Fprintf(b, `<div class="page-heading-row wt-create-row" id="wt-create-row" data-repo="%s" data-kind="%s" hidden>`,
		template.HTMLEscapeString(ref.name), template.HTMLEscapeString(ref.kind))
	fmt.Fprintf(b, `<input type="text" id="wt-branch" class="wt-branch-input" value="%s" placeholder="branch name (e.g. tongue/foo)" autocomplete="off" autocapitalize="off" autocorrect="off" spellcheck="false">`,
		template.HTMLEscapeString(defaultBranchPrefix))
	checked := ""
	if ref.kind == "repos" {
		checked = " checked"
	}
	fmt.Fprintf(b, `<label class="wt-from-main"><input type="checkbox" id="wt-from-main"%s> from main (otherwise this HEAD)</label>`, checked)
	b.WriteString(`<button type="button" class="toggle send-btn wt-create-submit" id="wt-create-submit">create</button>`)
	b.WriteString(`<button type="button" class="toggle wt-create-cancel" id="wt-create-cancel">cancel</button>`)
	b.WriteString(`</div>`)
}

// writeHomeWorktreeForm emits the worktree create form for one repo on
// the home page. It is hidden until its "+ worktree" button is clicked.
// It has the same parts as writeWorktreeCreateRow, found by class rather
// than id, because the home page has one form for each repo.
// data-open="agent" tells app.js to open the agent page of the new
// worktree, because the next step is to start work in it.
func writeHomeWorktreeForm(b *strings.Builder, id string, target repostats.Target) {
	fmt.Fprintf(b, `<div class="wt-create-row home-wt-create" id="%s" data-repo="%s" data-kind="%s" data-open="agent" hidden>`,
		template.HTMLEscapeString(id), template.HTMLEscapeString(target.Name), template.HTMLEscapeString(string(target.Kind)))
	fmt.Fprintf(b, `<input type="text" class="wt-branch-input" value="%s" placeholder="branch name (e.g. tongue/foo)" aria-label="branch name" autocomplete="off" autocapitalize="off" autocorrect="off" spellcheck="false">`,
		template.HTMLEscapeString(defaultBranchPrefix))
	b.WriteString(`<label class="wt-from-main"><input type="checkbox" checked> from main (otherwise this HEAD)</label>`)
	b.WriteString(`<button type="button" class="toggle send-btn wt-create-submit">create</button>`)
	b.WriteString(`<button type="button" class="toggle wt-create-cancel">cancel</button>`)
	b.WriteString(`</div>`)
}

// writeOpenButtons emits links that open dir in Zed, VS Code, and Ghostty on
// the machine running the browser. They have to be plain anchors to a custom
// scheme rather than a fetch: this page comes straight off the sandbox, so
// a request would land back here instead of on the desktop, and only a
// top-level navigation reaches the client's OS at all.
//
// Zed and VS Code register their own schemes and support SSH remote links.
// Ghostty has no inbound scheme, hence webdiff:// and
// deploy/install-url-handler.sh. None resolve on iOS, so .open-link hides
// them below 900px.
//
// Without an sshHost there's no host to name, and we'd rather emit nothing
// than a link that can't connect.
func writeOpenButtons(b *strings.Builder, dir, cls string) {
	if dir == "" || sshHost == "" {
		return
	}
	short := tildePath(dir)
	writeEditorLinks(b, dir, short, cls)
	ghostty := template.HTMLEscapeString("webdiff://ghostty?host=" +
		url.QueryEscape(sshHost) + "&dir=" + url.QueryEscape(dir))
	fmt.Fprintf(b, `<a class="%s open-link" href="%s" title="open a shell in %s">%s</a>`,
		cls, ghostty, template.HTMLEscapeString(short), icon("icon-ghostty"))
}

// writeFileOpenLinks emits links that open file at line in Zed and VS
// Code. file is relative to dir. The diff page puts them in the heading
// of each file.
func writeFileOpenLinks(b *strings.Builder, dir, file string, line int) {
	if dir == "" || sshHost == "" {
		return
	}
	// The wrapper is one item of the flex <summary>, so the gap between
	// the two links is smaller than the gap between the summary items.
	b.WriteString(`<span class="file-open-links open-link">`)
	writeEditorLinks(b, editorFileTarget(dir, file, line), file, "file-open")
	b.WriteString(`</span>`)
}

// writeEditorLinks emits the Zed and VS Code links that open target
// over SSH. target is an absolute path, with an optional ":line"
// suffix. name is the text that the link titles use for target.
func writeEditorLinks(b *strings.Builder, target, name, cls string) {
	// target is absolute, so it supplies the separator after the host.
	zed := template.HTMLEscapeString("zed://ssh/" + sshHost + target)
	vscode := template.HTMLEscapeString("vscode://vscode-remote/ssh-remote+" + sshHost + target)
	name = template.HTMLEscapeString(name)
	fmt.Fprintf(b, `<a class="%s open-link" href="%s" title="open %s in Zed">%s</a>`,
		cls, zed, name, icon("icon-zed"))
	fmt.Fprintf(b, `<a class="%s open-link" href="%s" title="open %s in VS Code">%s</a>`,
		cls, vscode, name, icon("icon-vscode"))
}

// editorFileTarget returns the editor link target for line of file.
// file is relative to dir. Each segment of file is percent-encoded, so
// characters such as "#" and "?" stay in the path. Both editors decode
// the path.
//
// The target always has a ":line" suffix. VS Code opens a remote path
// as a file only if it has this suffix. Without it, VS Code opens the
// path as a folder.
func editorFileTarget(dir, file string, line int) string {
	segs := strings.Split(file, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.TrimSuffix(dir, "/") + "/" + strings.Join(segs, "/") + ":" + strconv.Itoa(max(line, 1))
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

// writeHeadingTitleRow writes the first row of the page heading for a
// checkout: the title at the left, and the branch and stats at the
// right. The diff page and the stream page both use it, so the two
// pages have the same first row. stats is HTML, or "" for no stats.
//
// The title is the repo name. A worktree's leaf dir is its branch, which
// is redundant next to the branch shown in the meta; show its parent
// repo instead so the heading reads "repo … branch" like the home-page
// rows do. Broken worktrees have no resolvable parent — fall back to
// the slug, which at least identifies which worktree you're looking at.
func writeHeadingTitleRow(ctx context.Context, b *strings.Builder, ref repoRef, stats string) {
	title := worktreeSlug(ref.abs)
	if ref.kind == "worktrees" {
		if parent, ok := worktreeParentRepo(ref.abs); ok {
			title = parent
		}
	}
	b.WriteString(`<div class="page-heading-row">`)
	b.WriteString(`<span class="branch">`)
	writeTitle(b, title, ref.abs)
	b.WriteString(`</span>`)
	branch := currentBranch(ref.abs)
	b.WriteString(`<span class="head-meta">`)
	b.WriteString(headMetaHTML(branch, prs.lookupOne(ctx, ref.abs, branch), stats))
	b.WriteString(`</span></div>`)
}

// headMetaHTML returns the contents of the .head-meta element: the
// branch, then the pull request of the branch if pr is not nil, then
// the stats HTML. The pane stream sends the same HTML when the branch,
// the pull request or the stats change, so the stream page can replace
// it.
func headMetaHTML(branch string, pr *pullRequest, stats string) string {
	return `<span class="head-branch">` + template.HTMLEscapeString(branch) + `</span>` + prBadgeHTML(pr, false) + stats
}

// summaryHTML returns the stats HTML for s, or "" when s has no files.
func summaryHTML(s repostats.Summary) string {
	if s.Files == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(`<span class="stats">`)
	writeSummary(&b, s)
	b.WriteString(`</span>`)
	return b.String()
}

// cachedStats returns the stats HTML for all the changes of a checkout,
// from the repostats cache. It does not run git, so a page that does
// not show a diff can show stats cheaply. It returns "" when the cache
// has no stats for the checkout.
func cachedStats(ref repoRef) string {
	if statsManager == nil {
		return ""
	}
	target, ok := statsManager.Lookup(repostats.Kind(ref.kind), ref.name)
	if !ok {
		return ""
	}
	return summaryHTML(target.AllChanges)
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

// homeSession is one row in a home page session list: a live pane, or an
// agent session that we remember as running but that is no longer in
// tmux (see stoppedSessions).
type homeSession struct {
	Session string // session name, used to find the repo or worktree
	Label   string // "session:window.pane" for a live pane, else the name
	Href    string
	Desc    string // command and title, or "stopped"
	Agent   string // command of a live pane, for example "pi"
	Branch  string // branch of a stopped session at its first capture
	When    string // time since last use, from formatLastUsed
	// LastUsed is the unix time of the last activity: the last output
	// of a live pane, or the last capture of a stopped session. It is 0
	// if not known.
	LastUsed int64
	Stopped  bool
}

// homeSessions returns every live pane on the host, followed by the
// stopped agent sessions. Each live row links to the streaming page for
// that pane, so the reviewer can see the session state without having to
// be the one who sent a review comment. Each stopped row links to its
// archived scrollback.
//
// Live rows are per pane. Stopped rows are per session, because a
// session that is gone has no pane left to name.
func homeSessions(ctx context.Context, now time.Time) []homeSession {
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
	var out []homeSession
	for _, p := range panes {
		desc := p.Command
		if p.Title != "" && p.Title != p.Command {
			desc += " · " + p.Title
		}
		// /pane is the arbitrary-pane viewer; the handler looks up the
		// session name from the paneID at request time so we don't need
		// to pass it as a query param.
		out = append(out, homeSession{
			Session:  p.Session,
			Label:    p.Session + ":" + p.Index,
			Href:     "/pane?paneID=" + url.QueryEscape(p.PaneID),
			Desc:     desc,
			Agent:    p.Command,
			When:     formatLastUsed(p.LastUsed, now),
			LastUsed: p.LastUsed,
		})
	}
	for _, m := range stoppedSessions(liveNames) {
		out = append(out, homeSession{
			Session:  m.Session,
			Label:    m.Session,
			Href:     "/history/" + m.ID,
			Desc:     "stopped",
			Branch:   m.Branch,
			When:     formatLastUsed(m.Updated, now),
			LastUsed: m.Updated,
			Stopped:  true,
		})
	}
	return out
}

// writeSessionRow emits one session row. A nested row shows below the
// repo or worktree that it belongs to, so it does not repeat the session
// name or branch. The name stays in the title attribute.
//
// No data-session on stopped rows: app.js selects `[data-session]`
// rows for the spinner poll, so leaving it off keeps them
// out of it without the JS needing to know they exist.
func writeSessionRow(b *strings.Builder, s homeSession, nested bool) {
	cls := "dir-row tmux-row"
	if nested {
		cls += " session-row"
	}
	attrs := ""
	if s.Stopped {
		cls += " session-stopped"
	} else {
		attrs = fmt.Sprintf(` data-session="%s"`, template.HTMLEscapeString(s.Session))
	}
	fmt.Fprintf(b, `<a class="%s" href="%s" title="%s"%s>`,
		cls,
		template.HTMLEscapeString(s.Href),
		template.HTMLEscapeString(s.Label),
		attrs,
	)
	if nested {
		// The description is the part that tells sessions of one
		// checkout apart, so it takes the flexible name cell.
		fmt.Fprintf(b, `<span class="dir-name">%s</span>`, template.HTMLEscapeString(s.Desc))
	} else {
		// Columns: name, description, then time on the right. "stopped"
		// sits where a live row shows its running command, which is
		// exactly the distinction being drawn.
		desc := s.Desc
		if s.Branch != "" {
			desc += " · " + s.Branch
		}
		fmt.Fprintf(b, `<span class="dir-name">%s</span><span class="dir-stats">%s</span>`,
			template.HTMLEscapeString(s.Label),
			template.HTMLEscapeString(desc),
		)
	}
	fmt.Fprintf(b, `<span class="dir-branch">%s</span></a>`, template.HTMLEscapeString(s.When))
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
// a managed worktree). Both support worktree creation. Only managed
// worktrees have a remove button.
func renderRepo(w http.ResponseWriter, r *http.Request, ref repoRef) {
	repo := ref.abs
	commits, err := branchCommits(r.Context(), repo)
	if err != nil {
		http.Error(w, "list branch commits: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// `context=full` switches git diff to -U<MAX> so each file diff shows
	// the entire file with hunks inlined. Per-view toggle, not threaded
	// through other links — full context is rarely what you want when
	// navigating somewhere else.
	fullContext := r.URL.Query().Get("context") == "full"

	// A selection from the URL can stop working: a commit can leave the
	// branch, or the working tree can come to need a commit that is not
	// selected. Then show all changes, and tell why. resetErr is the
	// error of the selection from the URL.
	var (
		env     []string
		base    string
		working = workingTree{
			Changed: true,
		}
		cleanup      = func() {}
		resetErr     error
		selectionErr error
	)
	// viewCommit is the commit of the commit view (?commit=<sha>),
	// which shows one commit with its message.
	viewCommit := r.URL.Query().Get("commit")
	selected, selectionErr := selectedChanges(r.URL.Query(), commits)
	if selectionErr == nil {
		env, base, working, cleanup, selectionErr = selectionIndex(r.Context(), repo, commits, selected)
	}
	if selectionErr != nil && viewCommit == "" && r.URL.Query().Has("changes") {
		cleanup()
		resetErr = selectionErr
		selected = allChanges(commits)
		env, base, working, cleanup, selectionErr = selectionIndex(r.Context(), repo, commits, selected)
	}
	defer func() { cleanup() }()
	var files []string
	if selectionErr == nil {
		files = changedFiles(env, repo, base)
	}

	cols := 120
	if c := r.URL.Query().Get("cols"); c != "" {
		fmt.Sscanf(c, "%d", &cols)
	}

	urlPath := ref.url

	// Agent header button — always visible. When the repo's
	// webdiff-<base> session is already running it short-circuits
	// straight to /stream<repoUrl>; otherwise it points at
	// /agent<repoUrl>, which spawns the session and 303-redirects to
	// the same place. The dashed-border "to-be-created" treatment (via
	// add-comment-btn) signals the click will spend ~5 s spawning a new
	// pane rather than instantly attaching to a live one. Both URLs are
	// path-based: the ref and the session number select the session,
	// so neither side needs to thread a paneID through the URL.
	//
	// `?n=<n>` selects which agent session of the checkout the button
	// and the comments go to (see agent_sessions.go). The stream page's
	// diff link sets it for sessions other than session 1.
	sessionN, ok := sessionNumberParam(r.URL.Query())
	if !ok {
		http.Error(w, "bad session number", http.StatusBadRequest)
		return
	}
	paneID, _, liveAgent := repoStreamPane(r.Context(), repo, sessionN)

	// desktop tells that the page comes through the desktop client
	// (see runClient). Only then does the page show "send to github",
	// which needs the client.
	desktop := r.Header.Get(clientHeader) != ""

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
		agentHref = "/stream" + urlPath + sessionQuery(sessionN, "")
		agentClass = "toggle"
		agentTitle = "open the running " + pageAgent + " session for this repo"
	} else {
		agentHref = "/agent" + urlPath + sessionQuery(sessionN, pageAgent)
		agentClass = "toggle add-comment-btn"
		agentTitle = "start a " + pageAgent + " session for this repo"
	}
	// agentLabel tells the sessions of the checkout apart, for example
	// "pi #2".
	agentLabel := pageAgent
	if tag := sessionTag(sessionN); tag != "" {
		agentLabel += " " + tag
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
	body.WriteString(`<div class="page-heading">`)
	writeHeadingTitleRow(r.Context(), &body, ref, stats)

	// Keep diff controls on a separate row on narrow screens.
	body.WriteString(`<div class="page-heading-row diff-toolbar">`)
	body.WriteString(`<div class="header-buttons">`)
	body.WriteString(`<a class="toggle" href="/">home</a>`)
	fmt.Fprintf(&body, `<a class="%s" id="agent-btn" href="%s" title="%s">%s</a>`,
		agentClass, template.HTMLEscapeString(agentHref), template.HTMLEscapeString(agentTitle), template.HTMLEscapeString(agentLabel))
	writeOpenButtons(&body, ref.abs, "toggle")
	// "more" is next to home and the agent button, as on the stream page.
	body.WriteString(`<button class="toggle" type="button" id="diff-more-toggle" aria-expanded="false" aria-controls="diff-more-menu">more &#x25be;</button>`)
	body.WriteString(`<div class="diff-more-menu" id="diff-more-menu" hidden>`)
	writeWorktreeNewButton(&body)
	writeNewAgentButton(&body, urlPath, pageAgent)
	writeOtherSessionLinks(&body, urlPath, sessionN, liveCheckoutSessions(r.Context(), repo))
	fmt.Fprintf(&body, `<a class="toggle" href="/file/%s/%s/" title="browse repo files">files</a>`,
		ref.kind, template.HTMLEscapeString(ref.name))
	if ref.kind == "worktrees" {
		writeWorktreeRenameButton(&body, ref.name, ref.abs)
	}
	if len(files) > 0 {
		ctxLabel := "full context"
		if fullContext {
			ctxLabel = "diff only"
		}
		fmt.Fprintf(&body, `<button class="toggle" type="button" id="toggle-context" data-full="%t">%s</button>`,
			fullContext, ctxLabel)
	}
	body.WriteString(`</div>`)
	body.WriteString(`</div>`)

	// .diff-right holds the diff actions and remove at the right side of
	// the same row. Remove is last, so it is at the right edge. The
	// expand/collapse button is an icon, so the row does not wrap on
	// narrow screens. app.js changes the icon and the title.
	body.WriteString(`<div class="diff-right">`)
	body.WriteString(`<div class="header-buttons diff-primary">`)
	// The menu shows the selection of the page. In the commit view,
	// that is only the commit of the view.
	writeDiffPicker(&body, commits, selected, working, resetErr != nil, urlPath, sessionN, viewCommit)
	if len(files) > 0 {
		body.WriteString(`<button class="toggle icon-btn" type="button" id="toggle-all" title="expand all" aria-label="expand all"><svg class="icon" aria-hidden="true"><use href="#icon-expand-all"/></svg></button>`)
	}
	if desktop {
		writeSendToGitHubButton(&body, ref, commits, working)
	}
	body.WriteString(`</div>`)
	if ref.kind == "worktrees" {
		fmt.Fprintf(&body, `<button class="toggle danger diff-remove" type="button" id="wt-remove-btn" data-name="%s" title="remove this worktree">remove</button>`,
			template.HTMLEscapeString(ref.name))
	}
	body.WriteString(`</div></div>`)

	writeWorktreeCreateRow(&body, ref)
	body.WriteString(`</div>`)

	// send-comments is always visible (no `hidden`) so the modal — which
	// owns the "+ overall review comment" composer in the new layout —
	// is reachable even before any line/file comments are queued. The
	// floating bottom-right chip just opens the modal; the modal's
	// footer button does the actual send and is the one that
	// disables/enables based on comment count.
	fmt.Fprintf(&body, `<div class="send-bar"><button class="toggle send-btn" type="button" id="send-comments" data-session-n="%d">send to %s (<span class="send-count">0</span>)</button></div>`,
		sessionN, template.HTMLEscapeString(agentLabel))

	if resetErr != nil {
		fmt.Fprintf(&body, `<p class="selection-reset">The selected changes cannot be shown, so all changes are shown. %s</p>`,
			template.HTMLEscapeString(resetErr.Error()))
	}

	// The messages of the selected commits, oldest first. The commit
	// view selects one commit, so it shows one message.
	var msgs []commitMessage
	if selectionErr == nil {
		var shas []string
		for _, commit := range commits {
			if selected[commit.SHA] {
				shas = append(shas, commit.SHA)
			}
		}
		if len(shas) > 0 {
			var err error
			msgs, err = readCommitMessages(r.Context(), repo, shas)
			if err != nil {
				// The diff is still correct without the messages.
				fmt.Fprintf(&body, `<p class="selection-reset">Cannot read the commit messages: %s</p>`,
					template.HTMLEscapeString(err.Error()))
			}
		}
	}

	// Each commit message has an edit button. The working tree has an
	// entry in the commit messages section when it is selected and has
	// changes. Its edit button commits it with a new message.
	edit := &commitEdit{
		Kind:    ref.kind,
		Name:    ref.name,
		Working: selectionErr == nil && viewCommit == "" && selected["working"] && working.Changed,
	}
	hasMsgs := len(msgs) > 0 || edit.Working

	emptyMsg := "No changes selected, or the selected changes are empty."
	if len(files) == 0 && !hasMsgs {
		if selectionErr != nil {
			emptyMsg = selectionErr.Error()
		}
		fmt.Fprintf(&body, `<p class="empty">%s</p>`, template.HTMLEscapeString(emptyMsg))
		writePageForAgent(w, body.String(), pageAgent)
		return
	}

	// app.js matches the pills to the <details> sections by their
	// order, so the pill of the commit messages is first, as is its
	// section.
	body.WriteString(`<nav class="pillbar">`)
	if hasMsgs {
		fmt.Fprintf(&body, `<a class="pill" href="#%s">commits</a>`, commitMessagesID)
	}
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

	if hasMsgs {
		writeCommitMessages(&body, msgs, urlPath, sessionN, edit)
	}
	if len(files) == 0 {
		fmt.Fprintf(&body, `<p class="empty">%s</p>`, template.HTMLEscapeString(emptyMsg))
	}

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
		fmt.Fprintf(&body, `<details id="file-%s" data-file="%s" open><summary><span class="filename">%s</span>`,
			template.HTMLEscapeString(slug),
			template.HTMLEscapeString(file),
			template.HTMLEscapeString(display))
		// An editor cannot open a file that is not on disk, for
		// example a deleted file. A click on a link in a <summary>
		// follows the link and does not toggle the <details>.
		if isFileOnDisk(ref.abs, file) {
			writeFileOpenLinks(&body, ref.abs, file, firstChangedLine(canon))
		}
		body.WriteString(`<span class="filestats">`)
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

func serveFavicon(w http.ResponseWriter, r *http.Request) {
	var svg string
	switch r.URL.Path {
	case "/favicon-progress.svg":
		svg = faviconProgressSVG
	case "/favicon-complete.svg":
		svg = faviconCompleteSVG
	default:
		svg = faviconSVG
	}
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write([]byte(svg))
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
// the form "<slug>-wd" or "<slug>-wd:<window>" (or the same with a
// session number after "-wd") by searching every managed ref for the
// one whose repoSessionName matches (which also handles names
// containing "." or ":"). Returns "" when the label doesn't resolve to
// anything webdiff is currently managing.
func paneRepoURL(label string) string {
	session := strings.SplitN(label, ":", 2)[0]
	primary, _, ok := parseSessionName(session)
	if !ok {
		return ""
	}
	for _, ref := range allManagedRefs() {
		if repoSessionName(ref.abs) == primary {
			return ref.url
		}
	}
	return ""
}

// renderRepoStreamPage renders the live tail of the tmux pane paired
// with one repo or worktree. Mounted at /stream/ as a prefix handler
// so the URL suffix is the diff URL (/repos/<name>/ or
// /worktrees/<name>/) — the ref and the `?n=` session number (see
// agent_sessions.go) select the session, so the server resolves the
// paneID from them instead of accepting it as a query param. If the
// session hasn't been spawned
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
	n, ok := sessionNumberParam(r.URL.Query())
	if !ok {
		http.Error(w, "bad session number", http.StatusBadRequest)
		return
	}
	paneID, session, liveAgent := repoStreamPane(r.Context(), ref.abs, n)
	if paneID == "" {
		http.Redirect(w, r, "/agent"+ref.url+sessionQuery(n, r.URL.Query().Get("agent")), http.StatusSeeOther)
		return
	}
	// What's in the pane is the truth; `?agent=` is only a fallback for
	// a session started with a command we can't attribute.
	if liveAgent == "" {
		liveAgent = r.URL.Query().Get("agent")
	}
	writeStreamPage(r.Context(), w, paneID, ref.url, "", session, liveAgent)
}

// renderArbitraryPanePage is the streaming view for panes that
// aren't paired with a repo — opened from the home page's
// "sessions" listing. The session name is looked up from paneID
// so the URL stays at just /pane?paneID=... (no label query
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
	session, agent, err := paneSession(r.Context(), paneID)
	if err != nil {
		writeStreamPage(r.Context(), w, paneID, "", paneID, "", "")
		return
	}
	writeStreamPage(r.Context(), w, paneID, paneRepoURL(session), session, session, agent)
}

// writeStreamPage derives the heading from repoURL (so the title is the
// repo basename, matching the diff page) regardless of which entry
// point routed us here. fallbackTitle is only used for arbitrary panes
// that don't resolve to a known repo. liveAgent is the agent detected
// in the pane, "" when it isn't one we recognise.
func writeStreamPage(ctx context.Context, w http.ResponseWriter, paneID, repoURL, fallbackTitle, session, liveAgent string) {
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
	ref, hasRef := resolveRepoRef(repoURL)
	if hasRef {
		dir = ref.abs
	}
	pageAgent, _ := resolveAgent(liveAgent)
	agentSession := ""
	if liveAgent != "" {
		agentSession = session
	}
	// managed is true for a webdiff session. The links to the diff page
	// and to a restarted session keep its session number n.
	managed := isWebdiffSession(session)
	sessionN := sessionNumber(session)
	diffURL := repoURL
	if repoURL != "" && managed {
		diffURL += sessionQuery(sessionN, "")
	}
	var body strings.Builder
	body.WriteString(`<div class="page-heading">`)
	// A checkout gets the same first row as the diff page. A pane that
	// is not in a known checkout shows only its name.
	if hasRef {
		writeHeadingTitleRow(ctx, &body, ref, cachedStats(ref))
	} else {
		body.WriteString(`<div class="page-heading-row"><span class="branch">`)
		writeTitle(&body, title, dir)
		body.WriteString(`</span></div>`)
	}
	body.WriteString(`<div class="header-buttons">`)
	body.WriteString(`<a class="toggle" href="/">home</a>`)
	if repoURL != "" {
		fmt.Fprintf(&body, `<a class="toggle" href="%s">diff</a>`, template.HTMLEscapeString(diffURL))
	}
	if tag := sessionTag(sessionN); managed && tag != "" {
		fmt.Fprintf(&body, `<span class="session-tag" title="agent session %d of this checkout">%s</span>`, sessionN, tag)
	}
	// Same open and "+ worktree" buttons as the diff page, so the two
	// pages have the same buttons at the top left.
	writeOpenButtons(&body, dir, "toggle")
	// "+ worktree" and rename go in a "more" menu, so the header row
	// does not become too wide on narrow screens. The diff page uses
	// the same menu, and app.js sets up both with setupDiffMore.
	if hasRef {
		body.WriteString(`<button class="toggle" type="button" id="diff-more-toggle" aria-expanded="false" aria-controls="diff-more-menu">more &#x25be;</button>`)
		body.WriteString(`<div class="diff-more-menu" id="diff-more-menu" hidden>`)
		writeWorktreeNewButton(&body)
		writeNewAgentButton(&body, ref.url, pageAgent)
		// The other running sessions of the checkout.
		writeOtherSessionLinks(&body, ref.url, sessionN, liveCheckoutSessions(ctx, ref.abs))
		if worktreeName != "" {
			writeWorktreeRenameButton(&body, worktreeName, dir)
		}
		// Pi collapses or expands its thinking blocks on Ctrl-T.
		if pageAgent == piAgentName {
			body.WriteString(`<button class="toggle" type="button" id="pane-toggle-thinking" title="collapse or expand the agent's thinking blocks (ctrl+t)">toggle thinking</button>`)
		}
		body.WriteString(`</div>`)
	}
	body.WriteString(`<div class="header-buttons-right">`)
	if hasRef {
		// .wide-only hides this on narrow screens, where it makes the
		// row too wide. Restart stays because it is
		// the one used most.
		fmt.Fprintf(&body, `<a class="toggle wide-only" href="%s" title="previous sessions of this checkout">session history</a>`,
			template.HTMLEscapeString(historyListURL(ref.kind, ref.name)))
	}
	if repoURL != "" {
		// Restart is webdiff-managed-only: server gates on the `-wd`
		// session suffix, and we hide the button when there's no repo
		// URL to navigate to after the kill.
		//
		// The agent ids ride along as data attributes so the modal's
		// drop-down can be built without a second request: data-agent-id
		// is what's running now (the default the modal opens on),
		// data-agent-choices is everything selectable.
		//
		// data-can-stop tells the modal to show a "stop session" button.
		// It is the only stop control when the page has a repo URL, on
		// all screen widths.
		//
		// data-session-n is the session number, so the restart starts
		// the same session of the checkout again.
		canStop := ""
		if managed {
			canStop = ` data-can-stop="1"`
		}
		fmt.Fprintf(&body, `<button class="toggle danger" type="button" id="restart-session" data-repo-url="%s" data-session-n="%d" data-agent-id="%s" data-agent-choices="%s"%s>restart session</button>`,
			template.HTMLEscapeString(repoURL),
			sessionN,
			template.HTMLEscapeString(pageAgent),
			template.HTMLEscapeString(strings.Join(agentChoices(), ",")), canStop)
	}
	if managed && repoURL == "" {
		// Without a repo URL there is no restart button, and thus no
		// restart modal with its stop button. Stop only needs a
		// webdiff-managed session: the client goes to the home page
		// after the kill. The server does the same `-wd` check, so this
		// only hides a button that would fail.
		body.WriteString(`<button class="toggle danger" type="button" id="stop-session">stop session</button>`)
	}
	// Remove is the last button, so it is always at the right edge, as
	// on the diff page.
	if worktreeName != "" {
		fmt.Fprintf(&body, `<button class="toggle danger" type="button" id="wt-remove-btn" data-name="%s" title="remove this worktree">remove</button>`,
			template.HTMLEscapeString(worktreeName))
	}
	body.WriteString(`</div></div>`)
	if hasRef {
		writeWorktreeCreateRow(&body, ref)
	}
	body.WriteString(`</div>`)
	// data-repo-url asks the pane stream to also send the branch and
	// stats of the checkout when they change.
	var streamRepo string
	if hasRef {
		streamRepo = ref.url
	}
	fmt.Fprintf(&body, `<div id="pane-tail" data-pane-id="%s" data-agent-session="%s" data-repo-url="%s" class="term-container"><div class="term-inner"></div></div>`,
		template.HTMLEscapeString(paneID), template.HTMLEscapeString(agentSession), template.HTMLEscapeString(streamRepo))
	// The previous, next and jump buttons float over the top-right corner
	// of the pane, below the sticky heading. They are not in the header
	// row, because there they made the row too wide on narrow screens.
	// Previous and next go to each prompt and answer. Only Pi panes get
	// them: app.js finds the prompts and answers from how Pi draws them.
	// app.js hides all the buttons while the page follows the bottom of
	// the pane.
	body.WriteString(`<div class="pane-nav" id="pane-nav" hidden>`)
	if pageAgent == piAgentName {
		body.WriteString(`<button class="toggle" type="button" id="nav-prev" title="scroll to the previous prompt or answer">&#x2191; prev</button>`)
		body.WriteString(`<button class="toggle" type="button" id="nav-next" title="scroll to the next prompt or answer">&#x2193; next</button>`)
	}
	body.WriteString(`<button class="toggle" type="button" id="jump-bottom">jump to bottom</button>`)
	body.WriteString(`</div>`)
	body.WriteString(`<div class="stream-toolbar">`)
	body.WriteString(`<div class="stream-toolbar-row">`)
	body.WriteString(`<button class="toggle stream-key" type="button" data-key="Escape" title="send Escape">esc</button>`)
	body.WriteString(`<button class="toggle stream-key" type="button" data-key="Left" title="send left arrow">←</button>`)
	body.WriteString(`<button class="toggle stream-key" type="button" data-key="Up" title="send up arrow">↑</button>`)
	body.WriteString(`<button class="toggle stream-key" type="button" data-key="Down" title="send down arrow">↓</button>`)
	body.WriteString(`<button class="toggle stream-key" type="button" data-key="Right" title="send right arrow">→</button>`)
	body.WriteString(`<button class="toggle stream-key" type="button" data-key="Enter" title="send Enter">enter</button>`)
	body.WriteString(`<button class="toggle stream-quote" type="button" id="pane-quote" title="select text in the pane to quote it" disabled>quote</button>`)
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
	agentFlag := flag.String("agent", "claude", "agent command spawned in each repo's agent session; the first whitespace-separated token is used as the display name")
	terminalFlag := flag.String("terminal", "auto", "where new agent sessions run: rex, tmux, or auto (rex if the rex command is installed); sessions already running in tmux keep working either way")
	rexSocketFlag := flag.String("rex-socket", defaultRexSocket(), "Unix socket of the Rex server")
	branchPrefixFlag := flag.String("branch-prefix", "", "default branch prefix to prepopulate when creating worktrees and hide on the home page")
	onMainFlag := flag.String("develop-on-main", "", "comma-separated repos under the root that are developed directly on main; the home page shows them above the repos section with their branch and diff stats, and shows the other repos with buttons to start a worktree or an agent")
	sshHostFlag := flag.String("ssh-host", "", "how the browser's machine reaches this one over SSH (e.g. an ~/.ssh/config alias); defaults to this node's MagicDNS name")
	maintnerFlag := flag.String("maintner", "", "base URL of a maintner server (e.g. http://maintner.corp.ts.net) that supplies the GitHub pull request of each branch")
	prPollFlag := flag.Duration("pr-poll", 15*time.Minute, "interval of the git ls-remote poll for the pull requests of repos that maintner does not watch, or of all repos with no -maintner, while a branch of the repo has a commit from the last 3 hours; 0 turns the poll off")
	prPollIdleFlag := flag.Duration("pr-poll-idle", 24*time.Hour, "interval of the git ls-remote poll for a repo with no commit on its branches in the last 3 hours")
	gitProxyFlag := flag.String("git-proxy", "", "base URL of a read-only git proxy (e.g. git://rogitproxy.corp.ts.net) that the pull request poll uses for private repos; it tries public GitHub first")
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

	defaultBranchPrefix = strings.TrimSpace(*branchPrefixFlag)
	for _, name := range strings.Split(*onMainFlag, ",") {
		if name = strings.TrimSpace(name); name != "" {
			onMainRepos[name] = true
		}
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
	startRepoPuller(context.Background(), filepath.Join(cacheRoot, "pull.json"))
	branchIssues = loadBranchIssueStore(defaultBranchIssuesPath())
	if c := (prConfig{
		Maintner:    strings.TrimSpace(*maintnerFlag),
		GitPoll:     *prPollFlag,
		GitPollIdle: *prPollIdleFlag,
		GitProxy:    strings.TrimSpace(*gitProxyFlag),
	}); c.enabled() {
		prs = newPRTracker(c)
		startPRTracker(prs)
	}

	browserMux := http.NewServeMux()
	browserMux.HandleFunc("/app.js", serveAppJS)
	browserMux.HandleFunc("/favicon.svg", serveFavicon)
	browserMux.HandleFunc("/favicon-progress.svg", serveFavicon)
	browserMux.HandleFunc("/favicon-complete.svg", serveFavicon)
	browserMux.HandleFunc("/stream/", renderRepoStreamPage)
	browserMux.HandleFunc("/pane", renderArbitraryPanePage)
	browserMux.HandleFunc("/agent/", handleStartSession)
	browserMux.HandleFunc("/repos/", renderRepoDiff)
	browserMux.HandleFunc("/worktrees/", renderRepoDiff)
	browserMux.HandleFunc("/file/", renderFileBrowser)
	browserMux.HandleFunc("/history/", handleHistory)
	browserMux.HandleFunc("/", renderHomeHandler)

	switch *terminalFlag {
	case "rex", "tmux", "auto":
	default:
		fmt.Fprintf(os.Stderr, "-terminal must be rex, tmux, or auto, not %q\n", *terminalFlag)
		os.Exit(2)
	}
	_, rexErr := exec.LookPath("rex")
	if *terminalFlag == "rex" || rexErr == nil {
		// Rex sessions are followed even when new ones go to tmux, so
		// that switching the flag doesn't hide running sessions.
		rexc = newRexClient(*rexSocketFlag)
		go rexc.run(context.Background())
		rexSpawn = *terminalFlag != "tmux"
	}
	startSessionWatcher(context.Background())

	apiMux := http.NewServeMux()
	registerAgentRoutes(apiMux)
	registerGitRoutes(apiMux)

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
