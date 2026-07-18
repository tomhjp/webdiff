package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	terminal "github.com/buildkite/terminal-to-html/v3"
)

// historyRoot is the directory that holds archived agent-session
// scrollback (e.g. ~/.cache/webdiff/history). Set in main alongside the
// other cache roots.
var historyRoot string

// Session history persists the full scrollback of every *-wd tmux
// session so it survives a restart, a worktree removal, or the agent
// process simply exiting — all of which otherwise lose the pane's
// scrollback for good. Each archived *instance* is the pair of a
// session name and its tmux session_created time, so a restart (which
// gives the new session a fresh creation time) starts a new entry
// rather than overwriting the old one. An instance is two flat files
// keyed by a deterministic id `<created>-<session>`:
//
//   <id>.json — small metadata (repo, branch, timestamps)
//   <id>.ansi — the raw `capture-pane -e` scrollback
//
// Making the id deterministic means a session that outlives a webdiff
// restart maps back to the same files (update, not duplicate), and the
// listing can read just the tiny .json files.

// historyMeta is the per-instance metadata written as <id>.json.
type historyMeta struct {
	ID      string `json:"id"`
	Session string `json:"session"`
	Kind    string `json:"kind"`   // "repos" or "worktrees" ("" if unresolved)
	Name    string `json:"name"`   // repo / worktree basename
	Branch  string `json:"branch"` // git branch at first capture ("" if unknown)
	Started int64  `json:"started"`// tmux session_created, unix seconds
	Updated int64  `json:"updated"`// unix seconds of the last capture
}

const historySnapInterval = 2 * time.Second

// historyLastSnap throttles per-id captures so a session producing a
// steady stream of output doesn't rewrite its (potentially large)
// scrollback file on every watcher tick. Losing this map on restart is
// harmless — it just means the next capture isn't throttled.
var (
	historyMu       sync.Mutex
	historyLastSnap = map[string]time.Time{}
)

var historySessionRE = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// historyID builds the deterministic instance id from a session name
// and its tmux creation time. Session names come from repoSessionName
// (already restricted to safe chars) but we sanitise defensively so the
// id is always a single safe path component.
func historyID(created int64, session string) string {
	s := historySessionRE.ReplaceAllString(session, "-")
	return fmt.Sprintf("%d-%s", created, s)
}

// historyIDRE gates the id taken from the URL: a numeric created prefix
// then the sanitised session. No "/" means it can never escape
// historyRoot when joined into a filename.
var historyIDRE = regexp.MustCompile(`^[0-9]+-[A-Za-z0-9._-]+$`)

// captureHistory snapshots the full scrollback of a live *-wd session
// to its instance files. Throttled per-id unless force is set (used at
// turn-end and at the explicit kill points so the final state is always
// archived). A no-op when the capture is empty or tmux can't be reached.
func captureHistory(ctx context.Context, session string, created int64, force bool) {
	if historyRoot == "" || session == "" || created == 0 {
		return
	}
	id := historyID(created, session)

	historyMu.Lock()
	last, seen := historyLastSnap[id]
	if !force && seen && time.Since(last) < historySnapInterval {
		historyMu.Unlock()
		return
	}
	historyMu.Unlock()

	// Same flags handlePaneStream uses: -e keeps ANSI colour, -S - starts
	// at the top of the scrollback so we archive everything the pane has
	// produced, not just the visible area.
	out, err := tmuxOut(ctx, "capture-pane", "-p", "-e", "-S", "-", "-t", session+":0.0")
	if err != nil {
		return
	}
	scrollback := strings.TrimRight(out, "\n")
	if scrollback == "" {
		return
	}
	if err := os.WriteFile(filepath.Join(historyRoot, id+".ansi"), []byte(scrollback), 0600); err != nil {
		return
	}
	writeHistoryMeta(id, session, created)

	historyMu.Lock()
	historyLastSnap[id] = time.Now()
	historyMu.Unlock()
}

// writeHistoryMeta creates the <id>.json on first capture (resolving the
// repo/branch once) and otherwise just bumps the Updated timestamp,
// leaving Started and the resolved repo identity untouched.
func writeHistoryMeta(id, session string, created int64) {
	metaPath := filepath.Join(historyRoot, id+".json")
	now := time.Now().Unix()
	if b, err := os.ReadFile(metaPath); err == nil {
		var m historyMeta
		if json.Unmarshal(b, &m) == nil {
			m.Updated = now
			if data, err := json.Marshal(m); err == nil {
				_ = os.WriteFile(metaPath, data, 0600)
			}
			return
		}
	}
	m := historyMeta{
		ID:      id,
		Session: session,
		Started: created,
		Updated: now,
		Name:    strings.TrimSuffix(session, "-wd"),
	}
	if ref, ok := sessionRepoRef(session); ok {
		m.Kind = ref.kind
		m.Name = ref.name
		m.Branch = currentBranch(ref.abs)
	}
	if data, err := json.Marshal(m); err == nil {
		_ = os.WriteFile(metaPath, data, 0600)
	}
}

// sessionRepoRef resolves a *-wd session name back to the repo or
// worktree it was spawned for, by the same rootDir / worktreesRoot scan
// paneRepoURL uses. ok=false when nothing currently managed matches
// (e.g. the worktree was removed) — the caller then falls back to a
// name derived from the session.
func sessionRepoRef(session string) (repoRef, bool) {
	if !strings.HasSuffix(session, "-wd") {
		return repoRef{}, false
	}
	for _, scope := range []struct{ kind, root string }{
		{"repos", rootDir},
		{"worktrees", worktreesRoot},
	} {
		if scope.root == "" {
			continue
		}
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
				if ref, ok := resolveRepoRef("/" + scope.kind + "/" + e.Name() + "/"); ok {
					return ref, true
				}
			}
		}
	}
	return repoRef{}, false
}

// snapshotSessionHistory force-archives a session's current scrollback.
// Called from the restart and worktree-remove handlers just before the
// session is killed, so output produced since the last throttled
// watcher snapshot isn't lost. Returns the archived instance id (for
// building a /history/<id> reference) or "" when the session isn't
// webdiff-managed or tmux can't be reached.
func snapshotSessionHistory(ctx context.Context, session string) string {
	if !strings.HasSuffix(session, "-wd") {
		return ""
	}
	out, err := tmuxOut(ctx, "display-message", "-t", session, "-p", "#{session_created}")
	if err != nil {
		return ""
	}
	created, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return ""
	}
	captureHistory(ctx, session, created, true)
	return historyID(created, session)
}

// listHistory reads every <id>.json under historyRoot and returns the
// metadata sorted newest-first.
func listHistory() []historyMeta {
	if historyRoot == "" {
		return nil
	}
	entries, err := os.ReadDir(historyRoot)
	if err != nil {
		return nil
	}
	var metas []historyMeta
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(historyRoot, e.Name()))
		if err != nil {
			continue
		}
		var m historyMeta
		if json.Unmarshal(b, &m) != nil {
			continue
		}
		metas = append(metas, m)
	}
	sort.Slice(metas, func(i, j int) bool {
		if metas[i].Started != metas[j].Started {
			return metas[i].Started > metas[j].Started
		}
		return metas[i].ID > metas[j].ID
	})
	return metas
}

// handleHistory serves the history list at /history/ and a single
// archived session at /history/<id>. Mounted as a prefix handler.
func handleHistory(w http.ResponseWriter, r *http.Request) {
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/history"), "/")
	if id == "" {
		renderHistoryList(w)
		return
	}
	// A ".txt" suffix selects the agent-friendly plain-text rendering
	// (ANSI stripped) instead of the styled HTML view.
	plain := strings.HasSuffix(id, ".txt")
	if plain {
		id = strings.TrimSuffix(id, ".txt")
	}
	if !historyIDRE.MatchString(id) {
		http.NotFound(w, r)
		return
	}
	if plain {
		renderHistoryText(w, id)
		return
	}
	renderHistoryEntry(w, id)
}

// renderHistoryText serves one archived session as plain text with the
// terminal escape sequences stripped — the format an agent is handed in
// a later session. It runs the same terminal-to-html screen the HTML
// view uses, then reduces each line to its visible text via htmlText,
// so every escape sequence is interpreted (not regex-stripped) and the
// output matches the rendered view exactly.
func renderHistoryText(w http.ResponseWriter, id string) {
	b, err := os.ReadFile(filepath.Join(historyRoot, id+".json"))
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	var m historyMeta
	if err := json.Unmarshal(b, &m); err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	ansi, err := os.ReadFile(filepath.Join(historyRoot, id+".ansi"))
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	screen, _ := terminal.NewScreen()
	screen.Write(ansi)
	rendered := strings.TrimRight(screen.AsHTML(), "\n")

	var out strings.Builder
	fmt.Fprintf(&out, "# session %s", m.Name)
	if m.Branch != "" {
		fmt.Fprintf(&out, " (%s)", m.Branch)
	}
	fmt.Fprintf(&out, " — started %s\n", time.Unix(m.Started, 0).Format("2006-01-02 15:04"))
	out.WriteString("# tmux scrollback, ANSI stripped\n\n")
	for _, line := range strings.Split(rendered, "\n") {
		out.WriteString(htmlText(line))
		out.WriteByte('\n')
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(out.String()))
}

// renderHistoryList renders the newest-first listing. The chronological
// anchor (start time) is the row's main label; the repo and branch the
// session was on sit at the end of the row.
func renderHistoryList(w http.ResponseWriter) {
	metas := listHistory()

	var body strings.Builder
	body.WriteString(`<div class="page-heading"><div class="page-heading-row">`)
	body.WriteString(`<span class="branch">session history</span>`)
	body.WriteString(`</div><div class="page-heading-row page-heading-row-scroll"><div class="header-buttons">`)
	body.WriteString(`<a class="toggle" href="/">home</a>`)
	body.WriteString(`</div></div></div>`)

	body.WriteString(`<div class="dir-list">`)
	now := time.Now()
	for _, m := range metas {
		href := "/history/" + m.ID
		when := time.Unix(m.Started, 0).Format("2006-01-02 15:04")
		fmt.Fprintf(&body, `<a class="dir-row" href="%s"><span class="dir-name">%s</span>`,
			template.HTMLEscapeString(href), template.HTMLEscapeString(when))
		fmt.Fprintf(&body, `<span class="dir-stats">%s</span>`,
			template.HTMLEscapeString(formatLastUsed(m.Updated, now)))
		fmt.Fprintf(&body, `<span class="dir-parent">%s</span>`, template.HTMLEscapeString(m.Name))
		fmt.Fprintf(&body, `<span class="dir-branch">%s</span>`, template.HTMLEscapeString(m.Branch))
		body.WriteString(`</a>`)
	}
	body.WriteString(`</div>`)
	if len(metas) == 0 {
		body.WriteString(`<p class="empty">No session history yet.</p>`)
	}
	writePage(w, body.String())
}

// renderHistoryEntry renders one archived session's scrollback, using
// the same terminal-to-html → per-line span path the live stream view
// uses so the existing .term-container CSS lays it out.
func renderHistoryEntry(w http.ResponseWriter, id string) {
	b, err := os.ReadFile(filepath.Join(historyRoot, id+".json"))
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	var m historyMeta
	if err := json.Unmarshal(b, &m); err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	ansi, err := os.ReadFile(filepath.Join(historyRoot, id+".ansi"))
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	var body strings.Builder
	body.WriteString(`<div class="page-heading"><div class="page-heading-row">`)
	fmt.Fprintf(&body, `<span class="branch">%s</span>`, template.HTMLEscapeString(m.Name))
	body.WriteString(`<span class="head-meta">`)
	if m.Branch != "" {
		fmt.Fprintf(&body, `<span class="head-branch">%s</span>`, template.HTMLEscapeString(m.Branch))
	}
	fmt.Fprintf(&body, `<span class="stats">%s</span>`,
		template.HTMLEscapeString(time.Unix(m.Started, 0).Format("2006-01-02 15:04")))
	body.WriteString(`</span></div>`)
	body.WriteString(`<div class="page-heading-row page-heading-row-scroll"><div class="header-buttons">`)
	body.WriteString(`<a class="toggle" href="/">home</a>`)
	body.WriteString(`<a class="toggle" href="/history/">history</a>`)
	body.WriteString(`</div></div></div>`)

	screen, _ := terminal.NewScreen()
	screen.Write(ansi)
	rendered := rewriteFileHrefs(strings.TrimRight(screen.AsHTML(), "\n"))
	body.WriteString(`<div class="term-container"><div class="term-inner">`)
	if rendered != "" {
		for _, line := range strings.Split(rendered, "\n") {
			body.WriteString(`<span class="line">`)
			body.WriteString(line)
			body.WriteString(`</span>`)
		}
	}
	body.WriteString(`</div></div>`)
	writePage(w, body.String())
}
