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
// own `<style>` tag), but the JavaScript lives at /_/app.js so we can
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

// cacheRoot is where per-repo warm git index/object caches live, e.g.
// ~/.cache/webdiff. Reused across requests and process restarts so the
// stat cache built by the first `git add -A` is available for subsequent
// page loads.
var cacheRoot string

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

func getRepoCache(repo string) *repoCache {
	cachesMu.Lock()
	defer cachesMu.Unlock()
	c, ok := caches[repo]
	if !ok {
		c = &repoCache{dir: filepath.Join(cacheRoot, strings.TrimPrefix(repo, string(filepath.Separator)))}
		caches[repo] = c
	}
	return c
}

// isGitRepo reports whether dir contains a .git entry (file or directory).
func isGitRepo(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

// resolvePath turns a URL path into an absolute directory under rootDir, or
// returns ok=false if it would escape the root or doesn't exist.
func resolvePath(urlPath string) (string, bool) {
	rel := strings.Trim(urlPath, "/")
	target := filepath.Clean(filepath.Join(rootDir, rel))
	rootAbs, _ := filepath.Abs(rootDir)
	abs, err := filepath.Abs(target)
	if err != nil {
		return "", false
	}
	if abs != rootAbs && !strings.HasPrefix(abs, rootAbs+string(filepath.Separator)) {
		return "", false
	}
	info, err := os.Stat(abs)
	if err != nil || !info.IsDir() {
		return "", false
	}
	return abs, true
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

// fileDiff gets the diff for a single file and pipes it through the user's
// configured pager (e.g. delta, diff-so-fancy) if available. fullContext
// asks git for unlimited unchanged context (effectively the whole file
// with hunks inlined) — driven by the diff page's "full context" toggle.
func fileDiff(env []string, repo, base, file string, cols int, fullContext bool) string {
	args := []string{"diff", "--cached", "--color=always"}
	if fullContext {
		args = append(args, "-U99999")
	}
	args = append(args, base, "--", file)
	cmd := exec.Command("git", args...)
	cmd.Env = env
	cmd.Dir = repo
	raw, err := cmd.Output()
	if err != nil || len(raw) == 0 {
		return ""
	}

	pagerCmd := exec.Command("git", "var", "GIT_PAGER")
	pagerCmd.Dir = repo
	pagerOut, err := pagerCmd.Output()
	pager := strings.TrimSpace(string(pagerOut))
	if err != nil || pager == "" || pager == "cat" || pager == "less" || pager == "more" {
		return string(raw)
	}

	colsEnv := append(env, fmt.Sprintf("COLUMNS=%d", cols), "LANG=en_US.UTF-8", "LC_ALL=en_US.UTF-8")

	pagerWithWidth := pager
	if strings.Contains(pager, "delta") {
		pagerWithWidth = fmt.Sprintf("%s --width %d", pager, cols)
	}

	shell := exec.Command("sh", "-c", pagerWithWidth)
	shell.Env = colsEnv
	shell.Stdin = strings.NewReader(string(raw))
	colored, err := shell.Output()
	if err != nil {
		return string(raw)
	}
	return string(colored)
}

// htmlTagRE strips HTML tags so we can read the visible text of a single
// rendered line (for blank-detection and heading extraction).
var htmlTagRE = regexp.MustCompile(`<[^>]+>`)

// bg256RE / bg16RE match the CSS classes terminal-to-html emits for
// 256-color (`term-bgxN`, N=0..255) and 16-color (`term-bgN`, N=40-47
// or 100-107) background spans. We classify a line as add / del by
// decoding the dominant bg's hue, which works across every delta theme
// without us hardcoding which colour the user picked.
//
// digitRunRE / gutterSepRE drive theme-agnostic line-number parsing
// from the gutter portion of a rendered line.
var (
	bg256RE     = regexp.MustCompile(`term-bgx(\d+)`)
	bg16RE      = regexp.MustCompile(`term-bg(\d\d)`)
	bgSpanRE    = regexp.MustCompile(`<span [^>]*term-bg(?:x\d+|\d\d)`)
	digitRunRE  = regexp.MustCompile(`\d+`)
	gutterSepRE = regexp.MustCompile(`[│┊⎜⋮▏|]`)
)

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

// wrapDiffLines wraps each line of terminal-to-html output in a
// <span class="line"> so we can extend diff backgrounds across the full
// line width — terminal-to-html collapses the trailing bg cells that
// delta paints with `\x1b[K`, leaving the green/red highlight stopping
// at the end of the text. We also fill blank lines that sit between two
// same-state diff lines (a 1-line gap inside an added or removed run)
// so the highlight stays continuous across the visual paragraph.
//
// Classification (add / del / context) is theme-agnostic on purpose —
// webdiff aims to honour whatever delta config the user runs locally,
// not to pin one theme. classifyLine layers a small ladder of signals
// (256-colour bg cube hue → 16-colour bg → literal +/- prefix → blank-
// body gutter position) so any reasonable diff renderer lights up the
// click-to-comment data-attributes correctly.
//
// With delta's line-numbers feature on, each diff line starts with a
// non-bg gutter prefix (`    ┊ 36 │` etc) and only the code portion gets
// the diff bg. We split at the first bg span so the gutter stays
// uncoloured (matching delta in a real terminal) while the code area
// extends to the end of the line.
func wrapDiffLines(htmlIn, file string) string {
	htmlIn = strings.TrimRight(htmlIn, "\n")
	lines := strings.Split(htmlIn, "\n")
	classes := make([]string, len(lines))
	for i, line := range lines {
		classes[i] = classifyLine(line)
	}
	for i := 1; i < len(lines)-1; i++ {
		if classes[i] != "" || !isVisuallyBlank(lines[i]) {
			continue
		}
		if classes[i-1] != "" && classes[i-1] == classes[i+1] {
			classes[i] = classes[i-1]
		}
	}
	fileAttr := ""
	if file != "" {
		fileAttr = fmt.Sprintf(` data-file="%s"`, template.HTMLEscapeString(file))
	}
	var b strings.Builder
	// fileAnchorWritten marks the first visually-quiet line (no diff
	// class, no line numbers) as the file-comment anchor — clicking it
	// opens a file-scope composer, replacing the dedicated "+" button
	// that used to live in the summary. We only stamp non-diff lines
	// because diff lines already participate in the line-level
	// drag-select flow and we don't want a click ambiguity.
	fileAnchorWritten := false
	for i, line := range lines {
		oldNum, newNum := parseLineNumbers(line, classes[i])
		if classes[i] == "" && oldNum == "" && newNum == "" {
			if !fileAnchorWritten && file != "" {
				fmt.Fprintf(&b, `<span class="line line-file-anchor" data-file-comment="%s" title="comment on this whole file">%s</span>`,
					template.HTMLEscapeString(file), line)
				fileAnchorWritten = true
				continue
			}
			fmt.Fprintf(&b, `<span class="line">%s</span>`, line)
			continue
		}
		lineAttrs := fileAttr
		if oldNum != "" {
			lineAttrs += fmt.Sprintf(` data-old-line="%s"`, oldNum)
		}
		if newNum != "" {
			lineAttrs += fmt.Sprintf(` data-new-line="%s"`, newNum)
		}
		if classes[i] == "" {
			fmt.Fprintf(&b, `<span class="line"%s>%s</span>`, lineAttrs, line)
			continue
		}
		prefix, tail := splitDiffLine(line)
		fmt.Fprintf(&b,
			`<span class="line line-%s"%s><span class="line-prefix">%s</span><span class="line-tail">%s</span></span>`,
			classes[i], lineAttrs, prefix, tail)
	}
	return b.String()
}

// classifyLine reports the diff class ("add", "del", or "") of one
// rendered HTML line.
//
// The signals layer like a ladder, cheapest first:
//  1. Background-colour palette: walk every term-bgxN / term-bgN class
//     on the line and decode their hues. Indices in the 256-colour
//     6×6×6 cube whose green coord beats red → add; red beats green →
//     del. The basic 16 colours (1/9, 2/10) and their 16-colour SGR
//     equivalents (41/42/101/102) are mapped explicitly. Most delta
//     themes set diff bgs from the standard palette, so this hits
//     regardless of which exact colour the user picked. Lines with
//     emph have BOTH a regular and an emph bg class; both decode to
//     the same hue so the count just adds up.
//  2. Literal +/- prefix: raw `git diff --color=always` (no pager) puts
//     a `+` or `-` as the first visible character of every diff line
//     and we exclude the file-header lines (+++ / ---).
//  3. Gutter-position fallback for delta blank-body added/removed
//     lines: the line-numbers gutter has digits on only one side of
//     the column separator (left = old/del, right = new/add). bg
//     detection misses these because terminal-to-html drops the
//     trailing \x1b[K cells when the body is empty.
func classifyLine(line string) string {
	classCount := map[string]int{}
	for _, m := range bg256RE.FindAllStringSubmatch(line, -1) {
		c := classifyXterm256(atoiOrNeg(m[1]))
		if c != "" {
			classCount[c]++
		}
	}
	if len(classCount) == 0 {
		for _, m := range bg16RE.FindAllStringSubmatch(line, -1) {
			c := classifyXterm16(m[1])
			if c != "" {
				classCount[c]++
			}
		}
	}
	if len(classCount) > 0 {
		return pickMostFrequent(classCount)
	}
	if c := classifyByPrefixChar(line); c != "" {
		return c
	}
	return classifyBlankByGutter(line)
}

// classifyXterm256 returns "add" / "del" for the 256-colour palette
// indices that read as decisively green or red. 0-15 are the system
// 16 (1/9 = red, 2/10 = green); 16-231 are the 6×6×6 cube; 232-255 is
// the grayscale ramp (always neutral).
func classifyXterm256(n int) string {
	switch n {
	case 1, 9:
		return "del"
	case 2, 10:
		return "add"
	}
	if n < 16 || n > 231 {
		return ""
	}
	idx := n - 16
	r, g, bl := idx/36, (idx/6)%6, idx%6
	if g > r && g > bl {
		return "add"
	}
	if r > g && r > bl {
		return "del"
	}
	return ""
}

// classifyXterm16 maps the SGR 16-colour bg codes (40-47 normal,
// 100-107 bright) to add / del by red/green hue.
func classifyXterm16(s string) string {
	switch s {
	case "42", "102":
		return "add"
	case "41", "101":
		return "del"
	}
	return ""
}

// classifyByPrefixChar handles raw `git diff --color=always` (no pager
// or a non-delta pager): the SGR-coloured spans wrap a literal +/- as
// the first visible character of the line. File-header lines (+++/---)
// are excluded since they carry no diff body.
func classifyByPrefixChar(line string) string {
	text := htmlTagRE.ReplaceAllString(line, "")
	if strings.HasPrefix(text, "+++") || strings.HasPrefix(text, "---") {
		return ""
	}
	if strings.HasPrefix(text, "+") {
		return "add"
	}
	if strings.HasPrefix(text, "-") {
		return "del"
	}
	return ""
}

// classifyBlankByGutter recovers the class of a delta blank-body
// added/removed line whose body had no \x1b[K cells for terminal-to-
// html to keep. delta's line-numbers gutter is shaped:
//
//	[old col] <inter-sep> [new col] <gutter-sep> [content]
//
// where the inter-sep (often ⋮) splits the two number columns and the
// gutter-sep (often │) divides the gutter from the line body. We
// require at least two separator runs so single-separator decorations
// like a hunk-header `1│` aren't pulled in. Digits in the old col with
// the new col empty → del; the symmetric case → add.
func classifyBlankByGutter(line string) string {
	text := htmlTagRE.ReplaceAllString(line, "")
	seps := gutterSepRE.FindAllStringIndex(text, -1)
	if len(seps) < 2 {
		return ""
	}
	interEnd := seps[0][1]
	gutterEnd := seps[len(seps)-1][0]
	if interEnd > gutterEnd {
		return ""
	}
	oldHas := digitRunRE.MatchString(text[:seps[0][0]])
	newHas := digitRunRE.MatchString(text[interEnd:gutterEnd])
	if oldHas && !newHas {
		return "del"
	}
	if newHas && !oldHas {
		return "add"
	}
	return ""
}

// parseLineNumbers extracts old/new line numbers from the gutter
// portion of a rendered line — everything before the first bg-span,
// where delta paints its line-numbers columns. We further trim to
// before the LAST gutter separator so digits in the line body (e.g.
// `var x = 42`) don't leak into the data attributes. The line's
// classification picks which slot a single digit run belongs to (add →
// new, del → old); context lines have two runs and we map them in
// document order.
func parseLineNumbers(line, cls string) (oldNum, newNum string) {
	gutter := line
	if loc := bgSpanRE.FindStringIndex(line); loc != nil {
		gutter = line[:loc[0]]
	}
	text := htmlTagRE.ReplaceAllString(gutter, "")
	if seps := gutterSepRE.FindAllStringIndex(text, -1); len(seps) > 0 {
		text = text[:seps[len(seps)-1][0]]
	}
	nums := digitRunRE.FindAllString(text, -1)
	switch cls {
	case "add":
		if len(nums) > 0 {
			newNum = nums[len(nums)-1]
		}
	case "del":
		if len(nums) > 0 {
			oldNum = nums[0]
		}
	default:
		if len(nums) >= 2 {
			oldNum, newNum = nums[0], nums[1]
		}
	}
	return
}

func atoiOrNeg(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return -1
		}
		n = n*10 + int(r-'0')
		if n > 255 {
			return -1
		}
	}
	return n
}

func pickMostFrequent(counts map[string]int) string {
	best, bestN := "", 0
	for k, v := range counts {
		if v > bestN || (v == bestN && k < best) {
			best, bestN = k, v
		}
	}
	return best
}

// splitDiffLine partitions a single rendered line into the gutter
// prefix (rendered without a diff bg) and the code tail (rendered with
// the diff bg, extending to end of line via flex). The split point is
// the first <span> whose class list carries any term-bgxN / term-bgN —
// not the dominant bg, since delta's emph mode mixes a regular bg
// (bgx52/bgx22) with an emph bg (bgx124/bgx28) on the same line, and
// splitting at the most-frequent class can land in the middle of the
// content with the leading whitespace stranded in the (no-bg) prefix.
//
// Three shapes:
//   - line has at least one bg span → split at its opening tag; prefix
//     is the gutter, tail is everything from that span onwards
//   - line has the line-number gutter but no body (blank added/removed
//     line) → prefix is the whole line, tail is empty
//   - line has no gutter at all (raw `git diff` line, or a blank line
//     filled by neighbour-inheritance) → prefix is empty, tail is the
//     whole line so the CSS bg covers it end-to-end
func splitDiffLine(line string) (prefix, tail string) {
	if loc := bgSpanRE.FindStringIndex(line); loc != nil {
		return line[:loc[0]], line[loc[0]:]
	}
	if gutterSepRE.MatchString(line) {
		return line, ""
	}
	return "", line
}

// parentURL returns the URL pointing to the parent listing of urlPath,
// or "" if urlPath is already at root.
func parentURL(urlPath string) string {
	trimmed := strings.Trim(urlPath, "/")
	if trimmed == "" {
		return ""
	}
	parts := strings.Split(trimmed, "/")
	if len(parts) <= 1 {
		return "/"
	}
	return "/" + strings.Join(parts[:len(parts)-1], "/") + "/"
}

// writeWorkdir renders the path line in the header, making it a back-link to
// the parent listing when one exists.
func writeWorkdir(b *strings.Builder, displayPath, urlPath string) {
	parent := parentURL(urlPath)
	if parent == "" {
		fmt.Fprintf(b, `<span class="workdir">%s</span>`, template.HTMLEscapeString(displayPath))
		return
	}
	fmt.Fprintf(b, `<a class="workdir workdir-back" href="%s" title="back to listing">%s</a>`,
		template.HTMLEscapeString(parent), template.HTMLEscapeString(displayPath))
}

func handler(w http.ResponseWriter, r *http.Request) {
	target, ok := resolvePath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if isGitRepo(target) {
		renderRepo(w, r, target)
	} else {
		renderListing(w, r, target)
	}
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

// renderListing renders a directory of subdirectories. Per-repo branch
// and diff stats are NOT computed server-side — they're filled in by
// the client via /_/repo-stats so the page itself renders instantly
// even when one repo's `git diff` is slow. The diff page (renderRepo)
// keeps blocking on its own data; this lazy-loading is listing-only.
func renderListing(w http.ResponseWriter, r *http.Request, dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	urlPath := r.URL.Path
	if !strings.HasSuffix(urlPath, "/") {
		urlPath += "/"
	}

	mode := r.URL.Query().Get("mode")
	if mode != "working" && mode != "branch" && mode != "remote" {
		mode = "working"
	}
	modeQS := modeQuery(mode)

	type row struct {
		name  string
		isGit bool
	}
	var rows []row
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		rows = append(rows, row{name: e.Name(), isGit: isGitRepo(filepath.Join(dir, e.Name()))})
	}

	var body strings.Builder
	body.WriteString(`<div class="header"><div class="header-left">`)
	fmt.Fprintf(&body, `<span class="branch">%s</span>`, template.HTMLEscapeString(filepath.Base(dir)))
	writeWorkdir(&body, dir, urlPath)
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
	for _, r := range rows {
		cls := "dir-row"
		if !r.isGit {
			cls += " dir-folder"
		}
		// Git rows leave dir-stats and dir-branch empty server-side; the
		// `data-stats-url` attribute is the cue for app.js to fetch and
		// fill them. Non-git folders have no stats to load, so they
		// render fully here. The empty spans are present from the start
		// so layout doesn't shift when stats arrive.
		statsAttr := ""
		if r.isGit {
			statsAttr = fmt.Sprintf(` data-stats-url="%s"`,
				template.HTMLEscapeString("/_/repo-stats"+urlPath+r.name+"/"+modeQS))
		}
		fmt.Fprintf(&body, `<a class="%s" href="%s"%s><span class="dir-name">%s</span>`,
			cls, template.HTMLEscapeString(urlPath+r.name+"/"+modeQS),
			statsAttr,
			template.HTMLEscapeString(r.name))
		if r.isGit {
			body.WriteString(`<span class="dir-stats"></span><span class="dir-branch"></span>`)
		} else {
			body.WriteString(`<span class="dir-branch dir-folder-tag">folder</span>`)
		}
		body.WriteString(`</a>`)
	}
	if len(rows) == 0 {
		body.WriteString(`<p class="empty">No subdirectories here.</p>`)
	}
	body.WriteString(`</div>`)

	// Only the actual root listing gets the tmux section — deeper
	// non-git folders are project subdirectories where surfacing
	// host-wide tmux state would be noise.
	if dir == rootDir {
		writeTmuxSection(&body, r.Context())
	}

	writePage(w, body.String())
}

// handleRepoStats returns JSON {branch, files, ins, del} for one repo,
// computed via summarizeRepo. The path to the repo comes from the URL
// path after /_/repo-stats (matching the listing routing), and the
// diff base is selected via ?mode=. This is the lazy-load endpoint
// behind the listing's per-row stats; the diff page does not use it.
func handleRepoStats(w http.ResponseWriter, r *http.Request) {
	repoPath := strings.TrimPrefix(r.URL.Path, "/_/repo-stats")
	target, ok := resolvePath(repoPath)
	if !ok || !isGitRepo(target) {
		http.NotFound(w, r)
		return
	}
	mode := r.URL.Query().Get("mode")
	if mode != "working" && mode != "branch" && mode != "remote" {
		mode = "working"
	}
	s := summarizeRepo(target, mode)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Branch string `json:"branch"`
		Files  int    `json:"files"`
		Ins    int    `json:"ins"`
		Del    int    `json:"del"`
	}{s.branch, s.files, s.ins, s.del})
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
		// /_/pane is the arbitrary-pane viewer; the handler looks up the
		// session name from the paneID at request time so we don't need
		// to pass it as a query param.
		href := "/_/pane?paneID=" + url.QueryEscape(p.PaneID)
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

// renderRepo renders the diff view for a single git repo.
func renderRepo(w http.ResponseWriter, r *http.Request, repo string) {
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

	urlPath := r.URL.Path
	if !strings.HasSuffix(urlPath, "/") {
		urlPath += "/"
	}
	modeQS := modeQuery(mode)

	// Agent header button — always visible. When the repo's
	// webdiff-<base> session is already running it short-circuits
	// straight to /_/stream<repoUrl>; otherwise it points at
	// /_/agent<repoUrl>, which spawns the session and 303-redirects to
	// the same place. The dashed-border "to-be-created" treatment (via
	// add-comment-btn) signals the click will spend ~5 s spawning a new
	// pane rather than instantly attaching to a live one. Both URLs are
	// path-based: each repo maps 1-1 to its tmux session, so neither
	// side needs to thread a paneID through the URL.
	paneID, _ := repoStreamPane(r.Context(), repo)
	var agentHref, agentClass, agentTitle string
	if paneID != "" {
		agentHref = "/_/stream" + urlPath
		agentClass = "toggle"
		agentTitle = "open the running " + agentName + " session for this repo"
	} else {
		agentHref = "/_/agent" + urlPath
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
	body.WriteString(`</div></div></div>`)

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
		raw := fileDiff(env, repo, base, file, cols, fullContext)
		if raw == "" {
			continue
		}
		screen, _ := terminal.NewScreen(terminal.WithMaxSize(cols, 0))
		screen.Write([]byte(raw))
		heading, rest := extractHeading(screen.AsHTML())
		rendered := wrapDiffLines(rest, file)

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
// /_/app.js. Cached briefly client-side — the source only changes when
// the binary is rebuilt and the user reloads.
func serveAppJS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write([]byte(appJS))
}

// paneRepoURL derives the diff page URL from a tmux session label of the
// form "<basename>-wd" or "<basename>-wd:<window>" by scanning rootDir's
// subdirectories for the one whose repoSessionName matches (which also
// handles repos with "." or ":" in their name). Returns "" when the
// label doesn't resolve to a known repo.
func paneRepoURL(label string) string {
	session := strings.SplitN(label, ":", 2)[0]
	if !strings.HasSuffix(session, "-wd") {
		return ""
	}
	entries, err := os.ReadDir(rootDir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		sub := filepath.Join(rootDir, e.Name())
		if isGitRepo(sub) && repoSessionName(sub) == session {
			return "/" + e.Name() + "/"
		}
	}
	return ""
}

// renderRepoStreamPage renders the live tail of the tmux pane paired
// with one repo. Mounted at /_/stream/ as a prefix handler so the URL
// suffix is the repo's diff URL path: /_/stream/ → root repo,
// /_/stream/foo/ → /foo/. Each repo maps 1-1 to its `<base>-wd` tmux
// session, so the server resolves the paneID from the repo path
// instead of accepting it as a query param. If the session hasn't been
// spawned yet the request 303-redirects to /_/agent<repoUrl>, which
// spawns it and bounces back here.
//
// Layout: sticky page heading with nav links, streaming term tail in
// the middle, and a bottom-fixed toolbar with esc / ↑ / ↓ / enter keys
// plus a growing textarea. The actual SSE consumption and auto-follow
// behaviour live in app.js; this handler just serves the chrome and
// the empty container the JS fills in.
func renderRepoStreamPage(w http.ResponseWriter, r *http.Request) {
	repoURL := strings.TrimPrefix(r.URL.Path, "/_/stream")
	if repoURL == "" {
		repoURL = "/"
	}
	if !strings.HasSuffix(repoURL, "/") {
		repoURL += "/"
	}
	abs, ok := resolvePath(repoURL)
	if !ok || !isGitRepo(abs) {
		http.NotFound(w, r)
		return
	}
	paneID, _ := repoStreamPane(r.Context(), abs)
	if paneID == "" {
		http.Redirect(w, r, "/_/agent"+repoURL, http.StatusSeeOther)
		return
	}
	writeStreamPage(w, paneID, repoURL, "")
}

// renderArbitraryPanePage is the streaming view for tmux panes that
// aren't paired with a repo — opened from the home page's "tmux
// sessions" listing. The session name is looked up from paneID via
// tmux so the URL stays at just /_/pane?paneID=... (no label query
// param). The "diff" back-link is still shown when the pane's session
// matches a known webdiff repo (paneRepoURL), so clicking a
// `<base>-wd` row from the listing still gets the same chrome as the
// repo-paired view.
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
	if err := os.MkdirAll(cacheRoot, 0700); err != nil {
		fmt.Fprintln(os.Stderr, "cannot create cache dir:", err)
		os.Exit(1)
	}

	auth, err := newOwnerAuth(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, "tailscale identity check failed:", err)
		os.Exit(1)
	}

	browserMux := http.NewServeMux()
	browserMux.HandleFunc("/_/app.js", serveAppJS)
	browserMux.HandleFunc("/_/stream/", renderRepoStreamPage)
	browserMux.HandleFunc("/_/pane", renderArbitraryPanePage)
	browserMux.HandleFunc("/_/agent/", handleStartSession)
	browserMux.HandleFunc("/_/repo-stats/", handleRepoStats)
	browserMux.HandleFunc("/", handler)

	startSessionWatcher(context.Background())

	apiMux := http.NewServeMux()
	registerAgentRoutes(apiMux)

	// safeweb's DefaultCSP is strict-by-default; everything except
	// same-origin URLs is blocked. The application JavaScript is now
	// served from /_/app.js so script-src can stay strict ('self' only
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
	fmt.Printf("Serving on %s (root: %s, cache: %s, owner: %d)\n",
		strings.Join(addrs, ", "), rootDir, cacheRoot, auth.ownerID)
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
