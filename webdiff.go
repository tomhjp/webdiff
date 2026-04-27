package main

import (
	_ "embed"
	"fmt"
	"html"
	"html/template"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	terminal "github.com/buildkite/terminal-to-html/v3"
)

//go:embed style.css
var styleCSS string

// paletteCSS is the contents of ~/.config/webdiff/palette.css read once
// at startup. Empty when the user hasn't generated one — that's the
// common case, not an error. Loaded after styleCSS so any :root
// custom-properties it defines override the defaults baked into style.css.
var paletteCSS string

// rootDir is the directory the server was started in. All requests resolve
// repo paths relative to this.
var rootDir string

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
func diffStat(env []string, repo string) (files, ins, del int) {
	cmd := exec.Command("git", "diff", "--cached", "--shortstat", "HEAD")
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
func fileDiffStat(env []string, repo, file string) (added, removed string, binary bool) {
	cmd := exec.Command("git", "diff", "--cached", "--numstat", "HEAD", "--", file)
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

// changedFiles returns the list of files with uncommitted changes
func changedFiles(env []string, repo string) []string {
	cmd := exec.Command("git", "diff", "--cached", "--name-only", "HEAD")
	cmd.Env = env
	cmd.Dir = repo
	out, err := cmd.Output()
	if err != nil || len(strings.TrimSpace(string(out))) == 0 {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(out)), "\n")
}

// fileDiff gets the diff for a single file and pipes it through the user's
// configured pager (e.g. delta, diff-so-fancy) if available.
func fileDiff(env []string, repo, file string, cols int) string {
	cmd := exec.Command("git", "diff", "--cached", "--color=always", "HEAD", "--", file)
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

// addedPrefixRE matches a delta-rendered line whose old-side line-number
// column (term-fgx88) is empty and whose new-side column (term-fgx28)
// has a number — i.e. an added line, even if its code body is blank.
//
// removedPrefixRE is the symmetric pattern for a removed line.
var (
	addedPrefixRE   = regexp.MustCompile(`class="term-fgx88">\s*</span>.*?class="term-fgx28">\s*\d`)
	removedPrefixRE = regexp.MustCompile(`class="term-fgx88">\s*\d.*?class="term-fgx28">\s*</span>`)
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
// With delta's line-numbers feature on, each diff line starts with a
// non-bg gutter prefix (`    ┊ 36 │` etc) and only the code portion gets
// bg-22/bg-52. We split at the first bg span so the gutter stays
// uncoloured (matching delta in a real terminal) while the code area
// extends to the end of the line. Blank added/removed lines have no bg
// span at all in the HTML — terminal-to-html drops the trailing
// `\x1b[K` cells — so we additionally classify them by the gutter
// pattern (term-fgx88 vs term-fgx28 column emptiness).
func wrapDiffLines(htmlIn string) string {
	htmlIn = strings.TrimRight(htmlIn, "\n")
	lines := strings.Split(htmlIn, "\n")
	classes := make([]string, len(lines))
	for i, line := range lines {
		switch {
		case strings.Contains(line, "term-bgx22"):
			classes[i] = "add"
		case strings.Contains(line, "term-bgx52"):
			classes[i] = "del"
		case addedPrefixRE.MatchString(line):
			classes[i] = "add"
		case removedPrefixRE.MatchString(line):
			classes[i] = "del"
		}
	}
	for i := 1; i < len(lines)-1; i++ {
		if classes[i] != "" || !isVisuallyBlank(lines[i]) {
			continue
		}
		if classes[i-1] != "" && classes[i-1] == classes[i+1] {
			classes[i] = classes[i-1]
		}
	}
	var b strings.Builder
	for i, line := range lines {
		if classes[i] == "" {
			fmt.Fprintf(&b, `<span class="line">%s</span>`, line)
			continue
		}
		bg := "term-bgx22"
		if classes[i] == "del" {
			bg = "term-bgx52"
		}
		prefix, tail := splitDiffLine(line, bg)
		fmt.Fprintf(&b,
			`<span class="line line-%s"><span class="line-prefix">%s</span><span class="line-tail">%s</span></span>`,
			classes[i], prefix, tail)
	}
	return b.String()
}

// splitDiffLine partitions a single rendered line into the gutter
// prefix (rendered without a diff bg) and the code tail (rendered with
// the diff bg, extending to end of line via flex).
//
// Three shapes:
//   - line has bg-22/bg-52 → split at the first such span; prefix is
//     the gutter, tail is everything from that span onwards
//   - line has the line-number gutter but no body (blank added/removed
//     line) → prefix is the whole line, tail is empty
//   - line has no gutter at all (truly blank line filled by
//     neighbour-inheritance) → prefix is empty, tail is the whole line
//     so the bg covers it end-to-end
func splitDiffLine(line, bg string) (prefix, tail string) {
	if idx := strings.Index(line, bg); idx >= 0 {
		if spanStart := strings.LastIndex(line[:idx], "<span"); spanStart >= 0 {
			return line[:spanStart], line[spanStart:]
		}
	}
	if strings.Contains(line, "│") {
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
// row for a single repo.
func summarizeRepo(repo string) repoSummary {
	env, cleanup := diffEnv(repo)
	defer cleanup()
	files, ins, del := diffStat(env, repo)
	return repoSummary{
		branch: currentBranch(repo),
		files:  files,
		ins:    ins,
		del:    del,
	}
}

// renderListing renders a directory of subdirectories. For git repos it shows
// the current branch and short diff stats; summaries are computed in parallel
// so total time scales with the slowest repo, not the sum.
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

	summaries := make([]repoSummary, len(rows))
	var wg sync.WaitGroup
	for i, r := range rows {
		if !r.isGit {
			continue
		}
		wg.Add(1)
		go func(i int, sub string) {
			defer wg.Done()
			summaries[i] = summarizeRepo(sub)
		}(i, filepath.Join(dir, r.name))
	}
	wg.Wait()

	var body strings.Builder
	body.WriteString(`<div class="header"><div class="header-left">`)
	fmt.Fprintf(&body, `<span class="branch">%s</span>`, template.HTMLEscapeString(filepath.Base(dir)))
	writeWorkdir(&body, dir, urlPath)
	body.WriteString(`</div></div>`)

	body.WriteString(`<div class="dir-list">`)
	for i, r := range rows {
		cls := "dir-row"
		if !r.isGit {
			cls += " dir-folder"
		}
		fmt.Fprintf(&body, `<a class="%s" href="%s"><span class="dir-name">%s</span>`,
			cls, template.HTMLEscapeString(urlPath+r.name+"/"),
			template.HTMLEscapeString(r.name))
		if r.isGit {
			s := summaries[i]
			if s.files > 0 {
				body.WriteString(`<span class="dir-stats">`)
				noun := "files"
				if s.files == 1 {
					noun = "file"
				}
				fmt.Fprintf(&body, `<span class="stats-files">%d %s</span>`, s.files, noun)
				if s.ins > 0 {
					fmt.Fprintf(&body, ` <span class="stats-ins">+%d</span>`, s.ins)
				}
				if s.del > 0 {
					fmt.Fprintf(&body, ` <span class="stats-del">-%d</span>`, s.del)
				}
				body.WriteString(`</span>`)
			}
			fmt.Fprintf(&body, `<span class="dir-branch">%s</span>`, template.HTMLEscapeString(s.branch))
		} else {
			body.WriteString(`<span class="dir-branch dir-folder-tag">folder</span>`)
		}
		body.WriteString(`</a>`)
	}
	if len(rows) == 0 {
		body.WriteString(`<p class="empty">No subdirectories here.</p>`)
	}
	body.WriteString(`</div>`)

	writePage(w, body.String())
}

// renderRepo renders the diff view for a single git repo.
func renderRepo(w http.ResponseWriter, r *http.Request, repo string) {
	env, cleanup := diffEnv(repo)
	defer cleanup()

	files := changedFiles(env, repo)

	cols := 120
	if c := r.URL.Query().Get("cols"); c != "" {
		fmt.Sscanf(c, "%d", &cols)
	}

	urlPath := r.URL.Path
	if !strings.HasSuffix(urlPath, "/") {
		urlPath += "/"
	}

	var body strings.Builder
	body.WriteString(`<div class="header"><div class="header-left">`)
	fmt.Fprintf(&body, `<span class="branch">%s</span>`, template.HTMLEscapeString(currentBranch(repo)))
	writeWorkdir(&body, repo, urlPath)
	body.WriteString(`</div><div class="header-right">`)

	if len(files) == 0 {
		body.WriteString(`</div></div>`)
		body.WriteString(`<p style="color:#6c7086;padding:16px">No uncommitted changes.</p>`)
		writePage(w, body.String())
		return
	}

	nfiles, ins, del := diffStat(env, repo)
	if nfiles > 0 {
		noun := "files"
		if nfiles == 1 {
			noun = "file"
		}
		body.WriteString(`<span class="stats">`)
		fmt.Fprintf(&body, `<span class="stats-files">%d %s</span>`, nfiles, noun)
		if ins > 0 {
			fmt.Fprintf(&body, `, <span class="stats-ins">+%d</span>`, ins)
		}
		if del > 0 {
			fmt.Fprintf(&body, `, <span class="stats-del">-%d</span>`, del)
		}
		body.WriteString(`</span>`)
	}
	body.WriteString(`<button class="toggle" type="button" onclick="toggleAll()">expand all</button>`)
	body.WriteString(`</div></div>`)

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
		raw := fileDiff(env, repo, file, cols)
		if raw == "" {
			continue
		}
		screen, _ := terminal.NewScreen(terminal.WithMaxSize(cols, 0))
		screen.Write([]byte(raw))
		heading, rest := extractHeading(screen.AsHTML())
		rendered := wrapDiffLines(rest)

		added, removed, binary := fileDiffStat(env, repo, file)

		slug := strings.ReplaceAll(file, "/", "-")
		slug = strings.ReplaceAll(slug, ".", "-")

		display := file
		if heading != "" {
			display = heading
		}
		fmt.Fprintf(&body, `<details id="file-%s" open><summary><span class="filename">%s</span><span class="filestats">`,
			template.HTMLEscapeString(slug), template.HTMLEscapeString(display))
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
	fmt.Fprintf(w, `<!DOCTYPE html><html><head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<style>%s</style>
<style>%s</style>
<style>%s</style>
</head><body>%s
<script>
(function(){
  var FILE_KEY='webdiff:fileState';
  var details=document.querySelectorAll('details[id]');
  var btn=document.querySelector('.toggle');
  if(details.length===0)return;

  function load(){try{return JSON.parse(localStorage.getItem(FILE_KEY)||'null')}catch(e){return null}}
  function save(s){try{localStorage.setItem(FILE_KEY,JSON.stringify(s))}catch(e){}}

  var state=load()||{};
  details.forEach(function(d){if(state[d.id]===false)d.open=false;});

  function anyOpen(){return Array.from(details).some(function(d){return d.open})}
  function refreshBtn(){if(btn)btn.textContent=anyOpen()?'collapse all':'expand all'}
  details.forEach(function(d){
    d.addEventListener('toggle',function(){
      state[d.id]=d.open;save(state);refreshBtn();
    });
  });
  refreshBtn();

  window.toggleAll=function(){
    var any=anyOpen();
    details.forEach(function(d){d.open=!any});
  };

  document.querySelectorAll('.pill').forEach(function(p){
    p.addEventListener('click',function(){
      var id=p.getAttribute('href').slice(1);
      var el=document.getElementById(id);
      if(el&&!el.open)el.open=true;
    });
  });

  var pills=document.querySelectorAll('.pill');
  function updateActive(){
    var scrollY=window.scrollY+60;var active=null;
    details.forEach(function(d,i){if(d.offsetTop<=scrollY)active=i});
    pills.forEach(function(p,i){p.classList.toggle('pill-active',i===active)});
    if(active!==null){
      var ap=pills[active];var bar=ap.parentElement;
      bar.scrollTo({left:ap.offsetLeft-bar.offsetLeft-8,behavior:'smooth'});
    }
  }
  window.addEventListener('scroll',updateActive,{passive:true});
  requestAnimationFrame(function(){requestAnimationFrame(updateActive)});
})();
</script>
</body></html>`, styleCSS, xterm256CSS(), paletteCSS, body)
}

// xterm256CSS emits per-index .term-fgxN / .term-bgxN rules for the
// 256-colour palette. Indices 0–15 reference --color-N custom properties
// from style.css (so the user's palette.css override flows through);
// 16–231 are the 6×6×6 RGB cube and 232–255 are the 24-step grey ramp,
// both computed once at startup.
func xterm256CSS() string {
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
	if len(os.Args) > 1 && os.Args[1] == "palette" {
		if err := runPalette(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	port := "9418"
	if len(os.Args) > 1 {
		port = os.Args[1]
	}

	paletteCSS = loadPalette()

	wd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot determine working directory:", err)
		os.Exit(1)
	}
	if isGitRepo(wd) {
		rootDir = wd
	} else if top, err := exec.Command("git", "-C", wd, "rev-parse", "--show-toplevel").Output(); err == nil {
		rootDir = strings.TrimSpace(string(top))
	} else {
		rootDir = wd
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

	mode := "repo"
	if !isGitRepo(rootDir) {
		mode = "multi-repo browser"
	}
	fmt.Printf("Serving %s on http://0.0.0.0:%s (root: %s, cache: %s)\n", mode, port, rootDir, cacheRoot)
	http.HandleFunc("/", handler)
	http.ListenAndServe("0.0.0.0:"+port, nil)
}
