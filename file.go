package main

import (
	"bytes"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/alecthomas/chroma/v2"
	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
)

// File-viewer limits. Generous enough that almost all source files in a
// repo render in full; defensive enough that a stray multi-MB JSON blob
// or a checked-in binary doesn't OOM the tokeniser.
const (
	fileMaxRenderBytes  = 2 << 20 // 2 MiB — drop to raw/placeholder beyond this
	fileBinarySniffSize = 8 << 10 // 8 KiB — enough to catch a UTF-8 / NUL miss
)

// fileFormatter is the chroma HTML formatter shared by every file-view
// request. WithClasses(true) means chroma emits semantic class names
// (`.k`, `.s`, `.c1`, …); the actual colours come from the rules in
// assets/style.css, which reference --color-N so a user's palette.css
// flows through. WithLinkableLineNumbers makes #L42 fragments work, so
// OSC 8 links that include a line number (or anyone sharing a link)
// land at the right row.
var fileFormatter = chromahtml.New(
	chromahtml.WithClasses(true),
	chromahtml.WithLineNumbers(true),
	chromahtml.WithLinkableLineNumbers(true, "L"),
)

// renderFileBrowser serves /file/<kind>/<name>/<subpath>. Directories
// render as a `.dir-list` (same chrome as the home page); regular
// files render via chroma; anything else 404s. ?raw=1 streams the bytes
// straight back with a sniffed Content-Type — used by the "raw" button
// and as the fallback for binary or oversized files.
func renderFileBrowser(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/file")
	trimmed := strings.Trim(rest, "/")
	parts := strings.SplitN(trimmed, "/", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		http.NotFound(w, r)
		return
	}
	ref, ok := resolveRepoRef("/" + parts[0] + "/" + parts[1] + "/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	var subpath string
	if len(parts) == 3 {
		decoded, err := url.PathUnescape(parts[2])
		if err != nil {
			http.NotFound(w, r)
			return
		}
		subpath = strings.TrimSuffix(decoded, "/")
	}

	abs := filepath.Join(ref.abs, subpath)
	if !isUnder(abs, ref.abs) {
		http.NotFound(w, r)
		return
	}
	// EvalSymlinks resolves every component, so a symlink anywhere in
	// the path that escapes the repo root is caught here even if the
	// final target sits inside it via traversal.
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if !isUnder(resolved, ref.abs) {
		http.NotFound(w, r)
		return
	}
	info, err := os.Stat(resolved)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	if r.URL.Query().Get("raw") == "1" {
		if info.IsDir() {
			http.Error(w, "raw not supported for directories", http.StatusBadRequest)
			return
		}
		serveRawFile(w, r, resolved, info)
		return
	}

	switch {
	case info.IsDir():
		renderFileDir(w, ref, subpath, resolved)
	case info.Mode().IsRegular():
		renderFileContent(w, ref, subpath, resolved, info.Size())
	default:
		http.NotFound(w, r)
	}
}

// isUnder reports whether p sits inside (or equals) base, after both
// have been cleaned. Used by the file browser as a traversal guard:
// every path the user can name has to resolve under ref.abs.
func isUnder(p, base string) bool {
	rel, err := filepath.Rel(base, p)
	if err != nil {
		return false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}

// fileBrowseURL builds the /file URL for a given ref + sub-path. Each
// path segment is individually percent-encoded so spaces, `#`, and `?`
// in filenames survive the round-trip.
func fileBrowseURL(ref repoRef, subpath string) string {
	u := "/file/" + ref.kind + "/" + ref.name
	if subpath == "" {
		return u + "/"
	}
	var encoded []string
	for seg := range strings.SplitSeq(subpath, "/") {
		if seg == "" {
			continue
		}
		encoded = append(encoded, url.PathEscape(seg))
	}
	return u + "/" + strings.Join(encoded, "/")
}

// renderFileDir lists the entries in a directory using the same
// .dir-list / .dir-row chrome the home page uses for repos/worktrees.
// Directories sort first, files second, both case-insensitive. We
// deliberately don't hide dotfiles — the agent links into `.claude/`,
// and the server's already gated by tailscale identity so this isn't
// a public directory listing.
func renderFileDir(w http.ResponseWriter, ref repoRef, subpath, abs string) {
	entries, err := os.ReadDir(abs)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	type row struct {
		name  string
		isDir bool
	}
	rows := make([]row, 0, len(entries))
	for _, e := range entries {
		// .git is enormous (objects, packs, refs) and not useful to
		// browse from a file viewer; the diff page is the right
		// surface for git state. Other dotfiles stay visible —
		// .claude/, .github/, etc., are routinely meaningful.
		if e.Name() == ".git" {
			continue
		}
		rows = append(rows, row{name: e.Name(), isDir: e.IsDir()})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].isDir != rows[j].isDir {
			return rows[i].isDir
		}
		return strings.ToLower(rows[i].name) < strings.ToLower(rows[j].name)
	})

	var body strings.Builder
	writeFileHeading(&body, ref, subpath, false)
	body.WriteString(`<div class="dir-list">`)
	if subpath != "" {
		parent := path.Dir(subpath)
		if parent == "." {
			parent = ""
		}
		fmt.Fprintf(&body, `<a class="dir-row dir-folder" href="%s"><span class="dir-name">..</span></a>`,
			template.HTMLEscapeString(fileBrowseURL(ref, parent)))
	}
	for _, r := range rows {
		childSub := r.name
		if subpath != "" {
			childSub = subpath + "/" + r.name
		}
		cls := "dir-row"
		if r.isDir {
			cls += " dir-folder"
		}
		fmt.Fprintf(&body, `<a class="%s" href="%s"><span class="dir-name">%s</span></a>`,
			cls,
			template.HTMLEscapeString(fileBrowseURL(ref, childSub)),
			template.HTMLEscapeString(r.name))
	}
	if len(rows) == 0 {
		body.WriteString(`<p class="empty">(empty directory)</p>`)
	}
	body.WriteString(`</div>`)
	writePage(w, body.String())
}

// renderFileContent renders a regular file via chroma. Oversized and
// non-text files short-circuit to a placeholder with a "raw" link so
// the user can still pull the bytes if they need to.
func renderFileContent(w http.ResponseWriter, ref repoRef, subpath, abs string, size int64) {
	if size > fileMaxRenderBytes {
		renderFilePlaceholder(w, ref, subpath, fmt.Sprintf("file is %.1f MiB — too large to view inline", float64(size)/(1024*1024)))
		return
	}
	src, err := os.ReadFile(abs)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sniff := src
	if len(sniff) > fileBinarySniffSize {
		sniff = sniff[:fileBinarySniffSize]
	}
	if bytes.IndexByte(sniff, 0) >= 0 || !utf8.Valid(sniff) {
		renderFilePlaceholder(w, ref, subpath, fmt.Sprintf("binary file (%d bytes)", size))
		return
	}

	lexer := lexers.Match(abs)
	if lexer == nil {
		lexer = lexers.Analyse(string(src))
	}
	if lexer == nil {
		lexer = lexers.Fallback
	}
	lexer = chroma.Coalesce(lexer)
	iterator, err := lexer.Tokenise(nil, string(src))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var body strings.Builder
	writeFileHeading(&body, ref, subpath, true)
	body.WriteString(`<div class="term-container"><div class="term-inner">`)
	// styles.Fallback is unused when Classes(true) is set — chroma picks
	// class names from the standard token taxonomy regardless of style
	// — but Format requires a non-nil *chroma.Style argument.
	if err := fileFormatter.Format(&body, styles.Fallback, iterator); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	body.WriteString(`</div></div>`)
	writePage(w, body.String())
}

// renderFilePlaceholder is the bail-out view for files we won't tokenise
// (oversized, binary). Still shows the page chrome (so navigation back
// up the tree works) and offers a `raw` link so the bytes are reachable
// for download.
func renderFilePlaceholder(w http.ResponseWriter, ref repoRef, subpath, msg string) {
	var body strings.Builder
	writeFileHeading(&body, ref, subpath, true)
	fmt.Fprintf(&body, `<p class="empty">%s</p>`, template.HTMLEscapeString(msg))
	writePage(w, body.String())
}

// serveRawFile streams a file as-is. http.DetectContentType handles the
// common cases; for known text suffixes we force charset=utf-8 so
// browsers don't second-guess the encoding. Content-Disposition: inline
// so the browser shows it in-tab rather than triggering a download.
func serveRawFile(w http.ResponseWriter, _ *http.Request, abs string, info os.FileInfo) {
	f, err := os.Open(abs)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()
	var head [512]byte
	n, _ := f.Read(head[:])
	ct := http.DetectContentType(head[:n])
	if strings.HasPrefix(ct, "text/") && !strings.Contains(ct, "charset") {
		ct = ct + "; charset=utf-8"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", info.Size()))
	w.Header().Set("Content-Disposition", "inline; filename=\""+filepath.Base(abs)+"\"")
	if _, err := f.Seek(0, 0); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	io.Copy(w, f)
}

// writeFileHeading renders the sticky title row + nav row for the file
// browser. Row 1 is a breadcrumb of clickable path segments. Row 2 has
// home / diff / (raw) buttons matching the diff and stream pages.
func writeFileHeading(b *strings.Builder, ref repoRef, subpath string, includeRaw bool) {
	b.WriteString(`<div class="page-heading"><div class="page-heading-row">`)
	b.WriteString(`<span class="branch">`)
	fmt.Fprintf(b, `<a class="crumb" href="%s">%s</a>`,
		template.HTMLEscapeString(fileBrowseURL(ref, "")),
		template.HTMLEscapeString(ref.name))
	cumulative := ""
	if subpath != "" {
		for seg := range strings.SplitSeq(subpath, "/") {
			if seg == "" {
				continue
			}
			cumulative = path.Join(cumulative, seg)
			b.WriteString(`<span class="crumb-sep"> / </span>`)
			fmt.Fprintf(b, `<a class="crumb" href="%s">%s</a>`,
				template.HTMLEscapeString(fileBrowseURL(ref, cumulative)),
				template.HTMLEscapeString(seg))
		}
	}
	b.WriteString(`</span></div>`)

	b.WriteString(`<div class="page-heading-row page-heading-row-scroll"><div class="header-buttons">`)
	b.WriteString(`<a class="toggle" href="/">home</a>`)
	fmt.Fprintf(b, `<a class="toggle" href="%s">diff</a>`, template.HTMLEscapeString(ref.url))
	if includeRaw {
		fmt.Fprintf(b, `<a class="toggle" href="%s?raw=1">raw</a>`,
			template.HTMLEscapeString(fileBrowseURL(ref, subpath)))
	}
	b.WriteString(`</div></div></div>`)
}
