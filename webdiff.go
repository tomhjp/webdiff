package main

import (
	"fmt"
	"html/template"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	terminal "github.com/buildkite/terminal-to-html/v3"
)

// rootDir is the directory the server was started in. All requests resolve
// repo paths relative to this.
var rootDir string

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

// diffEnv returns an environment with a per-call temporary index file holding
// the result of `git add -A` against repo's HEAD.
func diffEnv(repo string) (env []string, cleanup func()) {
	f, err := os.CreateTemp("", "gitdiff-idx-*")
	if err != nil {
		return os.Environ(), func() {}
	}
	tmp := f.Name()
	f.Close()
	os.Remove(tmp)

	env = append(os.Environ(), "GIT_INDEX_FILE="+tmp)

	cmd := exec.Command("git", "read-tree", "HEAD")
	cmd.Env = env
	cmd.Dir = repo
	cmd.Run()

	cmd = exec.Command("git", "add", "-A")
	cmd.Env = env
	cmd.Dir = repo
	cmd.Run()

	return env, func() { os.Remove(tmp) }
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

// renderListing renders a directory of subdirectories. No stats — just names
// and a hint about whether each is a git repo. Clicking drills down.
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

	var body strings.Builder
	body.WriteString(`<div class="header"><div class="header-left">`)
	fmt.Fprintf(&body, `<span class="branch">%s</span>`, template.HTMLEscapeString(filepath.Base(dir)))
	writeWorkdir(&body, dir, urlPath)
	body.WriteString(`</div></div>`)

	body.WriteString(`<div class="dir-list">`)
	any := false
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		any = true
		sub := filepath.Join(dir, e.Name())
		cls, tagCls, tag := "dir-row dir-folder", "dir-branch dir-folder-tag", "folder"
		if isGitRepo(sub) {
			cls, tagCls, tag = "dir-row", "dir-branch", "git"
		}
		fmt.Fprintf(&body, `<a class="%s" href="%s"><span class="dir-name">%s</span><span class="%s">%s</span></a>`,
			cls, template.HTMLEscapeString(urlPath+e.Name()+"/"),
			template.HTMLEscapeString(e.Name()), tagCls, tag)
	}
	if !any {
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
		rendered := screen.AsHTML()

		added, removed, binary := fileDiffStat(env, repo, file)

		slug := strings.ReplaceAll(file, "/", "-")
		slug = strings.ReplaceAll(slug, ".", "-")

		fmt.Fprintf(&body, `<details id="file-%s"><summary><span class="filename">%s</span><span class="filestats">`,
			template.HTMLEscapeString(slug), template.HTMLEscapeString(file))
		switch {
		case binary:
			body.WriteString(`<span class="stats-files">binary</span>`)
		case added != "" || removed != "":
			fmt.Fprintf(&body, `<span class="stats-ins">+%s</span> <span class="stats-del">-%s</span>`,
				template.HTMLEscapeString(added), template.HTMLEscapeString(removed))
		}
		body.WriteString(`</span></summary><div class="term-container">`)
		body.WriteString(rendered)
		body.WriteString(`</div></details>`)
	}

	writePage(w, body.String())
}

func writePage(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!DOCTYPE html><html><head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width">
<style>
.term-container {
  font: 13px/1.4 "JetBrains Mono", "Fira Code", Menlo, Consolas, monospace;
  background: #0d1117; color: #e6edf3;
  padding: 8px; margin: 0;
  white-space: pre; overflow-x: auto;
}
.term-fg1 { font-weight: bold }
.term-fg2 { opacity: 0.7 }
.term-fg3 { font-style: italic }
.term-fg4 { text-decoration: underline }
.term-fg9 { text-decoration: line-through }
.term-fg30 { color: #000 }     .term-fg31 { color: #c23621 }
.term-fg32 { color: #25bc24 }  .term-fg33 { color: #adad27 }
.term-fg34 { color: #492ee1 }  .term-fg35 { color: #d338d3 }
.term-fg36 { color: #33bbc8 }  .term-fg37 { color: #cbcccd }
.term-fgi90,.term-fgi30  { color: #818383 }
.term-fgi91,.term-fgi31  { color: #fc391f }
.term-fgi92,.term-fgi32  { color: #31e722 }
.term-fgi93,.term-fgi33  { color: #eaec23 }
.term-fgi94,.term-fgi34  { color: #5833ff }
.term-fgi95,.term-fgi35  { color: #f935f8 }
.term-fgi96,.term-fgi36  { color: #14f0f0 }
.term-fgi97,.term-fgi37  { color: #e9ebeb }
.term-bg40  { background: #000 }     .term-bg41  { background: #c23621 }
.term-bg42  { background: #25bc24 }  .term-bg43  { background: #adad27 }
.term-bg44  { background: #492ee1 }  .term-bg45  { background: #d338d3 }
.term-bg46  { background: #33bbc8 }  .term-bg47  { background: #cbcccd }
body { margin: 0; background: #0d1117 }
.header { padding: 12px 8px; background: #161b22; border-bottom: 1px solid #30363d; display: flex; justify-content: space-between; align-items: flex-start; flex-wrap: wrap; gap: 4px }
.header-left { display: flex; flex-direction: column; gap: 2px }
.header-right { display: flex; align-items: center; gap: 8px; flex-wrap: wrap }
.branch { font: bold 15px sans-serif; color: #a6e3a1 }
.workdir { font: 12px sans-serif; color: #6c7086; text-decoration: none }
.workdir-back::before { content: "← "; color: #89b4fa }
.workdir-back { color: #89b4fa; cursor: pointer }
.workdir-back:hover { text-decoration: underline }
.stats { font: 13px sans-serif; color: #6c7086 }
.stats-files { color: #e6edf3 }
.stats-ins { color: #3fb950 }
.stats-del { color: #f85149 }
.dir-list { display: flex; flex-direction: column }
.dir-row { display: flex; align-items: baseline; gap: 12px; padding: 12px 14px; border-bottom: 1px solid #21262d; color: #e6edf3; text-decoration: none; font: 14px sans-serif }
.dir-row:hover { background: #161b22 }
.dir-name { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap }
.dir-folder .dir-name::before { content: "📁 "; opacity: 0.6 }
.dir-branch { font: 12px sans-serif; color: #a6e3a1; padding: 2px 8px; background: #161b22; border-radius: 10px }
.dir-folder-tag { color: #6c7086 }
.empty { color: #6c7086; padding: 16px; font: 14px sans-serif }
.toggle { font: 12px sans-serif; background: #30363d; color: #e6edf3; border: none; border-radius: 4px; padding: 4px 8px; cursor: pointer }
.toggle:hover { background: #484f58 }
.pillbar { display: flex; gap: 6px; padding: 8px; background: #0d1117; overflow-x: auto; white-space: nowrap; position: sticky; top: 0; z-index: 20; border-bottom: 1px solid #30363d; -webkit-overflow-scrolling: touch; transform: translateZ(0); will-change: transform }
.pill { font: 12px sans-serif; color: #e6edf3; background: #30363d; padding: 4px 10px; border-radius: 12px; text-decoration: none; flex-shrink: 0 }
.pill:hover { background: #484f58 }
.pill-active { background: #89b4fa; color: #0d1117 }
summary { font: bold 15px sans-serif; padding: 10px 8px; cursor: pointer; color: #89b4fa; background: #161b22; border-bottom: 1px solid #30363d; position: sticky; top: 37px; z-index: 10; display: flex; justify-content: space-between; align-items: center }
.filename { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis }
.filestats { font: 12px monospace; margin-left: 8px; white-space: nowrap }
details { margin-bottom: 2px }
details .term-container { margin: 0 }
</style>
<style>
%s
</style>
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
  details.forEach(function(d){d.open=state[d.id]===true});

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
</body></html>`, xterm256CSS(), body)
}

func xterm256CSS() string {
	base := []string{
		"#000", "#c23621", "#25bc24", "#adad27", "#492ee1", "#d338d3", "#33bbc8", "#cbcccd",
		"#818383", "#fc391f", "#31e722", "#eaec23", "#5833ff", "#f935f8", "#14f0f0", "#e9ebeb",
	}
	colors := make([]string, 256)
	for i := 0; i < 16; i++ {
		colors[i] = base[i]
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
	port := "9418"
	if len(os.Args) > 1 {
		port = os.Args[1]
	}

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

	mode := "repo"
	if !isGitRepo(rootDir) {
		mode = "multi-repo browser"
	}
	fmt.Printf("Serving %s on http://0.0.0.0:%s (root: %s)\n", mode, port, rootDir)
	http.HandleFunc("/", handler)
	http.ListenAndServe("0.0.0.0:"+port, nil)
}
