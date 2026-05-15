package main

import (
	"context"
	_ "embed"
	"encoding/json"
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
	"strconv"
	"strings"
	"sync"
	"time"

	terminal "github.com/buildkite/terminal-to-html/v3"
	"tailscale.com/safeweb"
)

//go:embed assets/style.css
var styleCSS string

//go:embed assets/page.html.tmpl
var pageTmplSrc string

//go:embed assets/app.js
var appJS string

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
// pane_current_command for readiness detection.
var (
	agentCmd  = "claude"
	agentName = "claude"
)

// cacheRoot is the top-level webdiff cache directory (e.g.
// ~/.cache/webdiff). repoCacheRoot and worktreesRoot are its two
// children, both webdiff-owned: per-repo warm git index/object caches
// in the first, managed worktree checkouts in the second. Splitting
// the two means a worktree's checkout never lives under its own
// diff-cache (or vice versa), and the on-disk layout mirrors the home
// page's "repos / worktrees" sections.
var (
	cacheRoot     string
	repoCacheRoot string
	worktreesRoot string
)

// repoCache holds the warm-path state for one repo: a per-repo mutex (so
// concurrent requests don't corrupt the shared index file) and the last
// HEAD we built the index against (for invalidation).
type repoCache struct {
	mu   sync.Mutex
	dir  string
	head string
}

var (
	cachesMu sync.Mutex
	caches   = map[string]*repoCache{}
)

// getRepoCache returns the warm cache for repo, lazily creating its
// in-memory entry on first access. The on-disk cache dir is
// `<repoCacheRoot>/<basename>` — a flat layout where collisions between
// two repos with the same basename across different rootDirs self-heal
// via the `head != c.head` check in diffEnv.
func getRepoCache(repo string) *repoCache {
	cachesMu.Lock()
	defer cachesMu.Unlock()
	c, ok := caches[repo]
	if !ok {
		c = &repoCache{dir: filepath.Join(repoCacheRoot, filepath.Base(repo))}
		caches[repo] = c
	}
	return c
}

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
	name string // basename of abs, doubles as URL slug
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
	var base string
	switch kind {
	case "repos":
		base = rootDir
	case "worktrees":
		base = worktreesRoot
	default:
		return repoRef{}, false
	}
	baseAbs, err := filepath.Abs(base)
	if err != nil {
		return repoRef{}, false
	}
	abs := filepath.Join(baseAbs, name)
	if abs != filepath.Clean(abs) || filepath.Dir(abs) != baseAbs {
		return repoRef{}, false
	}
	info, err := os.Stat(abs)
	if err != nil || !info.IsDir() {
		return repoRef{}, false
	}
	return repoRef{abs: abs, kind: kind, name: name, url: "/" + kind + "/" + name + "/"}, true
}

// diffEnv returns an environment pointing at a warm, per-repo index and
// object dir under cacheRoot. The index is rebuilt only when HEAD moves; on
// unchanged HEAD the existing stat cache makes `git add -A` an order of
// magnitude faster on large worktrees. The returned cleanup releases the
// per-repo lock — the cache itself persists across requests and restarts.
func diffEnv(repo string) (env []string, cleanup func()) {
	c := getRepoCache(repo)
	c.mu.Lock()
	unlocked := false
	bail := func() ([]string, func()) {
		if !unlocked {
			c.mu.Unlock()
			unlocked = true
		}
		return os.Environ(), func() {}
	}

	headOut, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		return bail()
	}
	head := strings.TrimSpace(string(headOut))

	objOut, err := exec.Command("git", "-C", repo, "rev-parse", "--git-path", "objects").Output()
	if err != nil {
		return bail()
	}
	objects := strings.TrimSpace(string(objOut))
	if !filepath.IsAbs(objects) {
		objects = filepath.Join(repo, objects)
	}

	indexFile := filepath.Join(c.dir, "index")
	objDir := filepath.Join(c.dir, "objects")
	headFile := filepath.Join(c.dir, "HEAD")

	// Survive process restarts: if in-memory state is empty, fall back to
	// the HEAD file on disk written by a previous run.
	if c.head == "" {
		if b, err := os.ReadFile(headFile); err == nil {
			c.head = strings.TrimSpace(string(b))
		}
	}

	rebuild := head != c.head
	if !rebuild {
		if _, err := os.Stat(indexFile); err != nil {
			rebuild = true
		}
	}

	if rebuild {
		// HEAD moved (or first run) — wipe and start fresh. The previous
		// HEAD's index entries reference blob OIDs that no longer match
		// the current tree, and the per-HEAD object dir would just leak.
		if err := os.RemoveAll(c.dir); err != nil {
			return bail()
		}
		if err := os.MkdirAll(objDir, 0700); err != nil {
			return bail()
		}
	}

	env = append(os.Environ(),
		"GIT_INDEX_FILE="+indexFile,
		"GIT_OBJECT_DIRECTORY="+objDir,
		"GIT_ALTERNATE_OBJECT_DIRECTORIES="+objects,
	)

	if rebuild {
		// `read-tree HEAD` discards stat-cache info, so we only run it on
		// rebuild — otherwise we'd throw away the warm path benefit.
		cmd := exec.Command("git", "read-tree", "HEAD")
		cmd.Env = env
		cmd.Dir = repo
		if err := cmd.Run(); err != nil {
			return bail()
		}
		if err := os.WriteFile(headFile, []byte(head), 0600); err != nil {
			return bail()
		}
		c.head = head
	}

	cmd := exec.Command("git", "add", "-A")
	cmd.Env = env
	cmd.Dir = repo
	cmd.Run()

	return env, func() {
		if !unlocked {
			c.mu.Unlock()
		}
	}
}

// branchBase returns the merge-base SHA between HEAD and origin/main (or
// main if origin/main doesn't exist). Falls back to "HEAD" when neither
// ref is present, giving the original working-copy-only behaviour.
func branchBase(repo string) (sha, label string) {
	for _, ref := range []string{"origin/main", "main"} {
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

// repoSummary is the per-repo info shown on a directory listing.
type repoSummary struct {
	branch       string
	files        int
	ins, del     int
}

// summarizeRepo computes the branch + diff-stat tuple shown on the listing
// row for a single repo. The mode arg is the user's chosen diff base
// ("working", "branch", "remote") so the row stats reflect what the
// diff page would show for the same repo.
func summarizeRepo(repo, mode string) repoSummary {
	env, cleanup := diffEnv(repo)
	defer cleanup()
	base, _ := diffBase(repo, mode)
	files, ins, del := diffStat(env, repo, base)
	return repoSummary{
		branch: currentBranch(repo),
		files:  files,
		ins:    ins,
		del:    del,
	}
}

// renderHome renders the root landing page. It has up to three
// sections: repos (git subdirs of rootDir), worktrees (managed
// checkouts under worktreesRoot), and tmux sessions (live *-wd panes).
// Per-repo branch and diff stats are NOT computed server-side — they
// load via /stats/repos/<name>/ (or /stats/worktrees/...) so the page
// itself renders instantly even when one repo's `git diff` is slow.
func renderHome(w http.ResponseWriter, r *http.Request) {
	entries, err := os.ReadDir(rootDir)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	mode := r.URL.Query().Get("mode")
	if mode != "working" && mode != "branch" && mode != "remote" {
		mode = "working"
	}
	modeQS := modeQuery(mode)

	var repoNames []string
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if !isGitRepo(filepath.Join(rootDir, e.Name())) {
			continue
		}
		repoNames = append(repoNames, e.Name())
	}

	var body strings.Builder
	body.WriteString(`<div class="header"><div class="header-left">`)
	fmt.Fprintf(&body, `<span class="branch">%s</span>`, template.HTMLEscapeString(filepath.Base(rootDir)))
	fmt.Fprintf(&body, `<span class="workdir">%s</span>`, template.HTMLEscapeString(rootDir))
	body.WriteString(`</div><div class="header-right">`)
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

	body.WriteString(`<h2 class="section-title">repos</h2>`)
	body.WriteString(`<div class="dir-list">`)
	for _, name := range repoNames {
		writeDirRow(&body, "repos", name, "", modeQS, false)
	}
	if len(repoNames) == 0 {
		body.WriteString(`<p class="empty">No git repos under this directory.</p>`)
	}
	body.WriteString(`</div>`)

	worktrees := listManagedWorktrees()
	if len(worktrees) > 0 {
		body.WriteString(`<h2 class="section-title">worktrees</h2>`)
		body.WriteString(`<div class="dir-list">`)
		for _, wt := range worktrees {
			writeDirRow(&body, "worktrees", wt.Name, wt.Repo, modeQS, wt.Broken)
		}
		body.WriteString(`</div>`)
	}

	writeTmuxSection(&body, r.Context())

	writePage(w, body.String())
}

// writeDirRow emits one <a class="dir-row"> for a repo or worktree on
// the home page. Stats come in via the data-stats-url that app.js
// fetches per row. parentRepo is shown in the right-side metadata only
// for worktrees (where it disambiguates which repo a branch belongs
// to); broken worktrees get the dir-broken class so they're greyed
// out but still clickable through to the diff page where the remove
// button lives.
func writeDirRow(b *strings.Builder, kind, name, parentRepo, modeQS string, broken bool) {
	url := "/" + kind + "/" + name + "/" + modeQS
	statsURL := "/stats/" + kind + "/" + name + "/" + modeQS
	cls := "dir-row"
	if broken {
		cls += " dir-broken"
	}
	fmt.Fprintf(b, `<a class="%s" href="%s" data-stats-url="%s"><span class="dir-name">%s</span>`,
		cls,
		template.HTMLEscapeString(url),
		template.HTMLEscapeString(statsURL),
		template.HTMLEscapeString(name))
	b.WriteString(`<span class="dir-stats"></span>`)
	if parentRepo != "" {
		fmt.Fprintf(b, `<span class="dir-parent">%s</span>`, template.HTMLEscapeString(parentRepo))
	}
	if broken {
		b.WriteString(`<span class="dir-branch dir-folder-tag">broken</span>`)
	} else {
		b.WriteString(`<span class="dir-branch"></span>`)
	}
	b.WriteString(`</a>`)
}

// handleRepoStats returns JSON {branch, files, ins, del} for one repo
// or worktree, computed via summarizeRepo. Mounted at /stats/ so the
// URL after the prefix is the same /repos/<name>/ or /worktrees/<name>/
// path used by the diff page itself; resolveRepoRef does the lookup.
// This is the lazy-load endpoint behind the home page's per-row stats;
// the diff page does not use it.
func handleRepoStats(w http.ResponseWriter, r *http.Request) {
	repoPath := strings.TrimPrefix(r.URL.Path, "/stats")
	ref, ok := resolveRepoRef(repoPath)
	if !ok || !isGitRepo(ref.abs) {
		http.NotFound(w, r)
		return
	}
	mode := r.URL.Query().Get("mode")
	if mode != "working" && mode != "branch" && mode != "remote" {
		mode = "working"
	}
	s := summarizeRepo(ref.abs, mode)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Branch string `json:"branch"`
		Files  int    `json:"files"`
		Ins    int    `json:"ins"`
		Del    int    `json:"del"`
	}{s.branch, s.files, s.ins, s.del})
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

// listManagedWorktrees scans worktreesRoot for direct subdirectories
// and returns one entry per managed worktree. The result is sorted by
// name. Dotfiles and non-directories are skipped. Healthy worktrees
// have a parent repo we can name; broken ones fall back to "" and
// render greyed out on the home page.
func listManagedWorktrees() []worktreeEntry {
	if worktreesRoot == "" {
		return nil
	}
	entries, err := os.ReadDir(worktreesRoot)
	if err != nil {
		return nil
	}
	var out []worktreeEntry
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		path := filepath.Join(worktreesRoot, e.Name())
		repo, ok := worktreeParentRepo(path)
		out = append(out, worktreeEntry{
			Name:   e.Name(),
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

// writeTmuxSection appends a "tmux sessions" block to the root listing
// listing every pane on the host. Each row links to the streaming page
// for that pane so the reviewer can peek at session state without
// having to be the one who kicked off a review-comment send. Hidden
// entirely when there are no panes (or when tmux isn't running) so the
// page stays clean on hosts that don't use tmux.
func writeTmuxSection(b *strings.Builder, ctx context.Context) {
	panes, err := listAllPanes(ctx)
	if err != nil || len(panes) == 0 {
		return
	}
	b.WriteString(`<h2 class="section-title">tmux sessions</h2>`)
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
	paneID, _ := repoStreamPane(r.Context(), repo)
	var agentHref, agentClass, agentTitle string
	if paneID != "" {
		agentHref = "/stream" + urlPath
		agentClass = "toggle"
		agentTitle = "open the running " + agentName + " session for this repo"
	} else {
		agentHref = "/agent" + urlPath
		agentClass = "toggle add-comment-btn"
		agentTitle = "start a " + agentName + " session for this repo"
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
			if ins > 0 {
				fmt.Fprintf(&sb, ` <span class="stats-ins">+%d</span>`, ins)
			}
			if del > 0 {
				fmt.Fprintf(&sb, ` <span class="stats-del">-%d</span>`, del)
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
	body.WriteString(`<div class="page-heading"><div class="page-heading-row">`)
	fmt.Fprintf(&body, `<span class="branch">%s</span>`, template.HTMLEscapeString(filepath.Base(repo)))
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
	fmt.Fprintf(&body, `<a class="%s" href="%s" title="%s">%s</a>`,
		agentClass, template.HTMLEscapeString(agentHref), template.HTMLEscapeString(agentTitle), template.HTMLEscapeString(agentName))
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
	// is the only differentiator the JS needs.
	fmt.Fprintf(&body, `<button class="toggle" type="button" data-sync-dir="pull" data-kind="%s" data-name="%s" title="pull sandbox state into this desktop repo">&#x2193; pull</button>`,
		template.HTMLEscapeString(ref.kind), template.HTMLEscapeString(ref.name))
	fmt.Fprintf(&body, `<button class="toggle" type="button" data-sync-dir="push" data-kind="%s" data-name="%s" title="push this desktop repo into the sandbox">&#x2191; push</button>`,
		template.HTMLEscapeString(ref.kind), template.HTMLEscapeString(ref.name))
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
	fmt.Fprintf(&body, `<div class="send-bar"><button class="toggle send-btn" type="button" id="send-comments">send to %s (<span class="send-count">0</span>)</button></div>`, template.HTMLEscapeString(agentName))

	if len(files) == 0 {
		msg := "No uncommitted changes."
		if baseLabel != "" {
			msg = "No changes since " + baseLabel + "."
		}
		fmt.Fprintf(&body, `<p style="color:#6c7086;padding:16px">%s</p>`, msg)
		writePage(w, body.String())
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
			fmt.Fprintf(&body, `<span class="stats-ins">+%s</span> <span class="stats-del">-%s</span>`,
				template.HTMLEscapeString(added), template.HTMLEscapeString(removed))
		}
		body.WriteString(`</span></summary><div class="term-container"><div class="term-inner">`)
		body.WriteString(rendered)
		body.WriteString(`</div></div></details>`)
	}

	writePage(w, body.String())
}

func writePage(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := struct {
		StyleCSS    template.CSS
		Xterm256CSS template.CSS
		PaletteCSS  template.CSS
		Body        template.HTML
		AgentName   string
	}{
		StyleCSS:    template.CSS(styleCSS),
		Xterm256CSS: template.CSS(xterm256),
		PaletteCSS:  template.CSS(paletteCSS),
		Body:        template.HTML(body),
		AgentName:   agentName,
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

// paneRepoURL derives the diff page URL from a tmux session label of
// the form "<basename>-wd" or "<basename>-wd:<window>" by checking
// rootDir's subdirs and worktreesRoot's entries for the one whose
// repoSessionName matches (which also handles names containing "." or
// ":"). Returns "" when the label doesn't resolve to anything
// webdiff is currently managing.
func paneRepoURL(label string) string {
	session := strings.SplitN(label, ":", 2)[0]
	if !strings.HasSuffix(session, "-wd") {
		return ""
	}
	for _, scope := range []struct {
		kind, root string
	}{
		{"repos", rootDir},
		{"worktrees", worktreesRoot},
	} {
		entries, err := os.ReadDir(scope.root)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			sub := filepath.Join(scope.root, e.Name())
			if isGitRepo(sub) && repoSessionName(sub) == session {
				return "/" + scope.kind + "/" + e.Name() + "/"
			}
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
	paneID, _ := repoStreamPane(r.Context(), ref.abs)
	if paneID == "" {
		http.Redirect(w, r, "/agent"+ref.url, http.StatusSeeOther)
		return
	}
	writeStreamPage(w, paneID, ref.url, "")
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
	out, err := tmuxOut(r.Context(), "display-message", "-t", paneID, "-p", "#{session_name}")
	session := strings.TrimSpace(out)
	if err != nil || session == "" {
		writeStreamPage(w, paneID, "", paneID)
		return
	}
	writeStreamPage(w, paneID, paneRepoURL(session), session)
}

// writeStreamPage derives the heading from repoURL (so the title is the
// repo basename, matching the diff page) regardless of which entry
// point routed us here. fallbackTitle is only used for arbitrary panes
// that don't resolve to a known repo.
func writeStreamPage(w http.ResponseWriter, paneID, repoURL, fallbackTitle string) {
	title := strings.Trim(repoURL, "/")
	if title == "" {
		title = fallbackTitle
	}
	var body strings.Builder
	body.WriteString(`<div class="page-heading"><div class="page-heading-row">`)
	fmt.Fprintf(&body, `<span class="branch">%s</span>`, template.HTMLEscapeString(title))
	body.WriteString(`</div><div class="header-buttons">`)
	body.WriteString(`<a class="toggle" href="/">home</a>`)
	if repoURL != "" {
		fmt.Fprintf(&body, `<a class="toggle" href="%s">diff</a>`, template.HTMLEscapeString(repoURL))
	}
	body.WriteString(`<div class="header-buttons-right">`)
	body.WriteString(`<button class="toggle" type="button" id="jump-bottom" hidden>jump to bottom</button>`)
	if repoURL != "" {
		// Restart is webdiff-managed-only: server gates on the `-wd`
		// session suffix, and we hide the button when there's no repo
		// URL to navigate to after the kill.
		fmt.Fprintf(&body, `<button class="toggle danger" type="button" id="restart-session" data-repo-url="%s">restart session</button>`,
			template.HTMLEscapeString(repoURL))
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
	body.WriteString(`<div class="stream-toolbar-row">`)
	body.WriteString(`<textarea id="pane-msg" class="stream-msg" rows="1" placeholder="message…" autocomplete="off"></textarea>`)
	body.WriteString(`<button class="toggle stream-send" type="button" id="pane-send">send</button>`)
	body.WriteString(`</div>`)
	body.WriteString(`</div>`)
	writePage(w, body.String())
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
	worktreesRoot = filepath.Join(cacheRoot, "worktrees")
	for _, d := range []string{cacheRoot, repoCacheRoot, worktreesRoot} {
		if err := os.MkdirAll(d, 0700); err != nil {
			fmt.Fprintln(os.Stderr, "cannot create cache dir:", err)
			os.Exit(1)
		}
	}

	auth, err := newOwnerAuth(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, "tailscale identity check failed:", err)
		os.Exit(1)
	}

	browserMux := http.NewServeMux()
	browserMux.HandleFunc("/app.js", serveAppJS)
	browserMux.HandleFunc("/stream/", renderRepoStreamPage)
	browserMux.HandleFunc("/pane", renderArbitraryPanePage)
	browserMux.HandleFunc("/agent/", handleStartSession)
	browserMux.HandleFunc("/stats/", handleRepoStats)
	browserMux.HandleFunc("/repos/", renderRepoDiff)
	browserMux.HandleFunc("/worktrees/", renderRepoDiff)
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
	// before any security-headers / CSRF / mux dispatch logic.
	httpSrv := &http.Server{Handler: auth.middleware(srv)}
	errCh := make(chan error, len(listeners))
	for _, ln := range listeners {
		go func(l net.Listener) { errCh <- httpSrv.Serve(l) }(ln)
	}
	if err := <-errCh; err != nil {
		fmt.Fprintln(os.Stderr, "serve:", err)
		os.Exit(1)
	}
}
