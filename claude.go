package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	terminal "github.com/buildkite/terminal-to-html/v3"
)

// Pane is one tmux pane that's a candidate for receiving review
// comments. Label is what the picker shows to the user.
type Pane struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// Comment is one review note entered in webdiff. The tuple of
// (file, oldLine, newLine) is rendered into a single-line bullet in
// the prompt; the frontend leaves the unused side empty per the diff
// state of the line that was clicked.
type Comment struct {
	File    string `json:"file"`
	OldLine string `json:"oldLine"`
	NewLine string `json:"newLine"`
	Text    string `json:"text"`
}

// sendRequest is the body of POST /api/comments/send.
//
// Repo is the URL path the browser was viewing (e.g. "/webdiff"); the
// server translates it to an absolute filesystem path and uses the
// path basename to derive a `webdiff-<base>` tmux session that's
// dedicated to this repo. No pane-picker fields any more — the
// "session per repo" model auto-creates the session on first send.
type sendRequest struct {
	Repo     string    `json:"repo"`
	Comments []Comment `json:"comments"`
}

func registerClaudeRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/comments/send", handleCommentsSend)
	mux.HandleFunc("/api/pane/stream", handlePaneStream)
	mux.HandleFunc("/api/pane/input", handlePaneInput)
}

// allowedPaneKeys is the whitelist for /api/pane/input's `key` field.
// Anything not on this list is rejected so the endpoint can't be
// abused to fire off arbitrary tmux key sequences (Ctrl-C, M-x, etc.)
// from a compromised browser session — `text` is the documented way
// to send freeform input. The set is small on purpose: extend it only
// when there's a concrete UI need.
var allowedPaneKeys = map[string]bool{
	"Up":    true,
	"Down":  true,
	"Enter": true,
}

type paneInputRequest struct {
	PaneID string `json:"paneID"`
	Key    string `json:"key,omitempty"`
	Text   string `json:"text,omitempty"`
}

// handlePaneInput sends a single tmux key (`key`) or a freeform message
// followed by Enter (`text`) to a pane. It's the back-end for the
// stream page's footer toolbar so the reviewer can drive a Claude TUI
// session — answer follow-up prompts, scroll history, kick off a
// planning session — without leaving the browser.
func handlePaneInput(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req paneInputRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.PaneID == "" {
		http.Error(w, "paneID required", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	switch {
	case req.Key != "":
		if !allowedPaneKeys[req.Key] {
			http.Error(w, "unsupported key", http.StatusBadRequest)
			return
		}
		if _, err := tmuxOut(ctx, "send-keys", "-t", req.PaneID, req.Key); err != nil {
			writeJSONError(w, http.StatusBadGateway, "tmux send-keys failed: "+err.Error(), nil)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"sent": req.Key})
	case req.Text != "":
		if err := sendToPane(ctx, req.PaneID, req.Text); err != nil {
			writeJSONError(w, http.StatusBadGateway, "tmux paste failed: "+err.Error(), nil)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"sent": "text"})
	default:
		http.Error(w, "key or text required", http.StatusBadRequest)
	}
}

func handleCommentsSend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req sendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(req.Comments) == 0 {
		http.Error(w, "no comments to send", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	// req.Repo is the URL pathname the browser was viewing (e.g.
	// "/webdiff"). Translate it to the absolute filesystem path so the
	// prompt names a location claude can actually `cd` into. Fall back
	// to the raw value if resolution fails — better a confusing prompt
	// than no prompt.
	repoPath := req.Repo
	if abs, ok := resolvePath(req.Repo); ok {
		repoPath = abs
	}
	prompt := formatPrompt(repoPath, req.Comments)

	pane, err := ensureRepoSession(ctx, repoPath)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, err.Error(), nil)
		return
	}

	if err := sendToPane(ctx, pane.ID, prompt); err != nil {
		writeJSONError(w, http.StatusBadGateway, "tmux send failed: "+err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"sent":   len(req.Comments),
		"paneID": pane.ID,
		"label":  pane.Label,
		"prompt": prompt,
	})
}

// handlePaneStream serves a Server-Sent Events stream of `tmux
// capture-pane` snapshots for paneID. The browser opens it after a
// successful send so the reviewer can watch claude's output without
// switching to tmux. There's no idle detection — the browser closes the
// stream when the user clicks "stop" (or navigates away, which cancels
// the request context).
//
// Each event's data is a JSON object: {"html": "..."} with the rendered
// pane content, or {"error": "..."} if a capture failed (after which we
// give up so EventSource doesn't auto-reconnect into the same error).
func handlePaneStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	paneID := r.URL.Query().Get("paneID")
	if paneID == "" {
		http.Error(w, "paneID required", http.StatusBadRequest)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	// Some reverse proxies (and safeweb's middleware) buffer responses;
	// this hint tells nginx-style proxies to pass bytes through.
	h.Set("X-Accel-Buffering", "no")

	ctx := r.Context()

	send := func(payload any) bool {
		data, err := json.Marshal(payload)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	// We track three layers of state between ticks:
	//
	//   lastRaw     — the raw tmux capture bytes; used as a cheap
	//                 equality short-circuit so an idle pane skips the
	//                 terminal-to-html render entirely.
	//   pending     — rendered per-line HTML waiting to be emitted;
	//                 nil when there's nothing new since the last send.
	//   lastSent    — the per-line HTML the client already has, used
	//                 to compute incremental deltas against `pending`.
	//
	// On each change we render the full pane, split into lines, wrap
	// each in <span class="line"> (the same shape wrapDiffLines uses
	// for the diff body so the existing .term-container .line CSS
	// lays the lines out vertically), then send the smallest delta
	// that brings the client up to date:
	//
	//   • first tick / unalignable redraw → {reset: true, append: all}
	//   • shifted history + new tail      → {drop: k, append: tail}
	//   • pure append                     → {drop: 0, append: tail}
	//
	// The drop/append framing handles the common tmux case (history
	// fills up, oldest lines fall off the top while new ones append
	// at the bottom) without ever having to retransmit the whole
	// scrollback, which on a 2000-line history-limit pane would be
	// painful over mobile.
	//
	// Cadence is leading-edge throttled: poll capture-pane every
	// `streamPollInterval` (small, so a change after idle is detected
	// quickly) but only emit when at least `streamEmitMinInterval` has
	// elapsed since the previous emit. After an idle stretch the first
	// new content goes out almost immediately; under sustained activity
	// (claude's spinners / typing) emits fire at a steady ~750 ms
	// cadence so mobile clients don't pay for every cursor blink.
	//
	// `-S -` starts the capture at the top of the pane history rather
	// than the top of the visible area, so the reviewer sees the
	// scrollback claude has produced too — without it, anything that
	// has scrolled off-screen in the tmux pane is invisible here.
	const (
		streamPollInterval    = 100 * time.Millisecond
		streamEmitMinInterval = 750 * time.Millisecond
	)
	var lastRaw string
	var pending []string
	var lastSent []string
	var lastEmit time.Time
	snapshot := func() bool {
		out, err := tmuxOut(ctx, "capture-pane", "-p", "-e", "-S", "-", "-t", paneID)
		if err != nil {
			send(map[string]string{"error": err.Error()})
			return false
		}
		if out != lastRaw {
			lastRaw = out
			screen, _ := terminal.NewScreen()
			screen.Write([]byte(out))
			rendered := strings.TrimRight(screen.AsHTML(), "\n")
			var wrapped []string
			if rendered != "" {
				for _, line := range strings.Split(rendered, "\n") {
					wrapped = append(wrapped, `<span class="line">`+line+`</span>`)
				}
			}
			pending = wrapped
		}
		if pending == nil {
			return true
		}
		if !lastEmit.IsZero() && time.Since(lastEmit) < streamEmitMinInterval {
			return true
		}
		var ok bool
		if lastSent == nil {
			ok = send(map[string]any{"reset": true, "append": pending})
		} else {
			drop, appendLines := streamDelta(lastSent, pending)
			if drop == 0 && len(appendLines) == 0 {
				lastSent = pending
				pending = nil
				lastEmit = time.Now()
				return true
			}
			ok = send(map[string]any{"drop": drop, "append": appendLines})
		}
		if !ok {
			return false
		}
		lastSent = pending
		pending = nil
		lastEmit = time.Now()
		return true
	}

	if !snapshot() {
		return
	}
	ticker := time.NewTicker(streamPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !snapshot() {
				return
			}
		}
	}
}

// streamDelta finds the smallest drop+append patch that turns the
// client's last-known buffer (`old`) into the new buffer (`new`). It
// scans for the smallest k such that old[k:] is a prefix of new — the
// idea being that tmux history shifts up by k lines as oldest lines
// fall off the top, and any remaining new content lands at the
// bottom. If no such k exists the buffer was redrawn (banner repaint,
// pager exit, screen clear), and we fall back to "drop everything,
// append everything" — correct, just not minimal.
//
// The scan is O(N²) worst case but in practice exits at the first or
// second iteration: typical k is 0 (append-only) or a single-digit
// shift.
func streamDelta(old, new []string) (drop int, appendLines []string) {
	for drop = 0; drop <= len(old); drop++ {
		remaining := old[drop:]
		if len(remaining) > len(new) {
			continue
		}
		match := true
		for i := range remaining {
			if remaining[i] != new[i] {
				match = false
				break
			}
		}
		if match {
			return drop, new[len(remaining):]
		}
	}
	return len(old), new
}

// ensureRepoSession returns the claude pane in the tmux session
// dedicated to repoPath, creating both session and claude process on
// first call. The session is named `webdiff-<basename>` so it's easy
// to spot in `tmux list-sessions` output and won't collide with the
// user's own sessions. When the session already exists we just look
// up its initial pane (window 0, pane 0) — webdiff always puts claude
// there, and ignoring later splits the user might have made keeps the
// behaviour predictable.
func ensureRepoSession(ctx context.Context, repoPath string) (Pane, error) {
	name := repoSessionName(repoPath)
	if sessionExists(ctx, name) {
		out, err := tmuxOut(ctx, "display", "-t", name+":0.0", "-p", "#{pane_id}")
		if err != nil {
			return Pane{}, fmt.Errorf("tmux display: %w", err)
		}
		return Pane{ID: strings.TrimSpace(out), Label: name}, nil
	}

	// `bash -lc claude` (rather than execing claude directly) so the
	// new pane picks up the user's login PATH — claude typically lives
	// in ~/.local/bin which isn't on the systemd unit's inherited PATH.
	// `-c repoPath` so claude starts already cd'd to the repo.
	out, err := tmuxOut(ctx, "new-session", "-d", "-s", name, "-c", repoPath, "-P", "-F", "#{pane_id}", "bash", "-lc", "claude")
	if err != nil {
		return Pane{}, fmt.Errorf("tmux new-session: %w", err)
	}
	paneID := strings.TrimSpace(out)
	if paneID == "" {
		return Pane{}, errors.New("tmux didn't return a pane id")
	}

	// Wait for the bash → claude exec so paste arrives at the TUI's
	// prompt, not at bash's command line. Same 5 s budget the old
	// spawnClaudePane used.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return Pane{}, ctx.Err()
		default:
		}
		cmd, err := tmuxOut(ctx, "display", "-t", paneID, "-p", "#{pane_current_command}")
		if err == nil && strings.TrimSpace(cmd) == "claude" {
			// Claude prints its banner before showing the prompt; a
			// short pause avoids racing with the splash output and
			// having our paste land mid-redraw.
			time.Sleep(300 * time.Millisecond)
			return Pane{ID: paneID, Label: name + " (just spawned)"}, nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return Pane{}, errors.New("claude didn't start within 5 s")
}

// handleStartSession ensures the repo's webdiff-<base> tmux session
// exists, then 303-redirects to /_/stream pointed at the resulting
// pane. The diff page's "claude" header button uses this so a single
// button works whether the session is already running (fast lookup) or
// needs to be spawned (~5 s for the bash → claude exec to settle).
func handleStartSession(w http.ResponseWriter, r *http.Request) {
	repo := r.URL.Query().Get("repo")
	if repo == "" {
		http.Error(w, "repo required", http.StatusBadRequest)
		return
	}
	abs, ok := resolvePath(repo)
	if !ok {
		http.NotFound(w, r)
		return
	}
	pane, err := ensureRepoSession(r.Context(), abs)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	qs := "paneID=" + url.QueryEscape(pane.ID) + "&label=" + url.QueryEscape(pane.Label)
	if repo := r.URL.Query().Get("repo"); repo != "" {
		qs += "&repo=" + url.QueryEscape(repo)
	}
	http.Redirect(w, r, "/_/stream?"+qs, http.StatusSeeOther)
}

// repoStreamPane returns the pane id and session label for the repo's
// webdiff tmux session if it already exists. Returns ("", "") when the
// session hasn't been spawned yet (the diff page hides the stream link
// in that case — readers don't need a "stream" link before there's
// anything to stream). Failures past existence (the display lookup
// erroring out) are also reported as not-running so a transient tmux
// hiccup just removes the link rather than breaking page render.
func repoStreamPane(ctx context.Context, repoPath string) (paneID, label string) {
	name := repoSessionName(repoPath)
	if !sessionExists(ctx, name) {
		return "", ""
	}
	out, err := tmuxOut(ctx, "display", "-t", name+":0.0", "-p", "#{pane_id}")
	if err != nil {
		return "", ""
	}
	return strings.TrimSpace(out), name
}

// repoSessionName derives the tmux session name for a repo path. The
// basename gives us something readable in `tmux list-sessions`; the
// `webdiff-` prefix scopes it to this tool. Characters tmux disallows
// in session names (`.`, `:`) are squashed to dashes.
func repoSessionName(repoPath string) string {
	base := filepath.Base(filepath.Clean(repoPath))
	if base == "" || base == "." || base == "/" {
		base = "default"
	}
	base = strings.NewReplacer(".", "-", ":", "-", " ", "-").Replace(base)
	return "webdiff-" + base
}

// sessionExists reports whether the named tmux session is currently
// running. `has-session` exits non-zero for both "session missing" and
// "tmux server not running"; we treat both as "doesn't exist" because
// the caller's next move is `new-session`, which auto-starts the
// server if needed and surfaces any real failure with a clear error.
func sessionExists(ctx context.Context, name string) bool {
	_, err := tmuxOut(ctx, "has-session", "-t", "="+name)
	return err == nil
}

// writeJSONError writes a uniform JSON error body with optional extra
// payload.
func writeJSONError(w http.ResponseWriter, status int, msg string, extra map[string]any) {
	body := map[string]any{"error": msg}
	for k, v := range extra {
		body[k] = v
	}
	writeJSON(w, status, body)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// SessionPane is one tmux pane enriched with the session it belongs
// to and the command/title it's currently running. Used by the root
// listing to surface every tmux pane as a clickable jump-into-stream
// row, independent of the review-comment send flow.
type SessionPane struct {
	Session string // session name, e.g. "claude"
	Index   string // window.pane index within the session, e.g. "0.1"
	PaneID  string // tmux pane id, e.g. "%23"
	Command string // pane_current_command, e.g. "claude" or "vim"
	Title   string // pane_title; only meaningful when distinct from Command
}

// listAllPanes returns every tmux pane on the host, sorted by session
// then window.pane index. Used by the root listing's "tmux sessions"
// section to surface the full tmux state, independent of any
// webdiff-managed session.
func listAllPanes(ctx context.Context) ([]SessionPane, error) {
	out, err := tmuxOut(ctx, "list-panes", "-a", "-F",
		"#{session_name}\t#{window_index}.#{pane_index}\t#{pane_id}\t#{pane_current_command}\t#{pane_title}")
	if err != nil {
		if isTmuxDown(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("tmux list-panes: %w", err)
	}
	var panes []SessionPane
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, "\t", 5)
		if len(fields) < 4 {
			continue
		}
		sp := SessionPane{
			Session: fields[0],
			Index:   fields[1],
			PaneID:  fields[2],
			Command: fields[3],
		}
		if len(fields) == 5 {
			sp.Title = fields[4]
		}
		panes = append(panes, sp)
	}
	return panes, nil
}

// isTmuxDown reports whether an error from tmuxOut means the daemon
// isn't running. tmux uses "no server running on …" or "error
// connecting to …" depending on version; both end up as the same
// "treat as zero panes/sessions" case for us.
func isTmuxDown(err error) bool {
	s := err.Error()
	return strings.Contains(s, "no server running") || strings.Contains(s, "error connecting")
}

// sendToPane delivers `prompt` as a single bracketed-paste payload
// followed by Enter. Bracketed paste signals the receiving TUI to
// treat newlines as content rather than submits, so multiline reviews
// land as one user turn.
//
// We pause between the paste-close and the submit Enter: Claude's TUI
// re-renders after a paste, and an Enter that lands during the redraw
// is sometimes consumed as "insert newline" rather than "submit". The
// 100 ms wait is well below human-perceptible latency and avoids the
// race in practice.
func sendToPane(ctx context.Context, paneID, prompt string) error {
	pasteSteps := [][]string{
		{"send-keys", "-t", paneID, "-l", "--", "\x1b[200~"},
		{"send-keys", "-t", paneID, "-l", "--", prompt},
		{"send-keys", "-t", paneID, "-l", "--", "\x1b[201~"},
	}
	for _, args := range pasteSteps {
		if _, err := tmuxOut(ctx, args...); err != nil {
			return err
		}
	}
	time.Sleep(100 * time.Millisecond)
	if _, err := tmuxOut(ctx, "send-keys", "-t", paneID, "Enter"); err != nil {
		return err
	}
	return nil
}

// formatPrompt turns the per-comment tuples into a single multiline
// prompt the Claude TUI will see as one paste.
func formatPrompt(repo string, comments []Comment) string {
	var b strings.Builder
	if repo != "" {
		fmt.Fprintf(&b, "Review comments from webdiff for %s.\n\n", repo)
	} else {
		b.WriteString("Review comments from webdiff.\n\n")
	}
	for _, c := range comments {
		// Three scopes encoded by which fields are populated:
		//   - File=="" → review-level (the whole change)
		//   - File!="" but no line numbers → file-level
		//   - File!="" with NewLine/OldLine → line-level (existing
		//     behaviour)
		var loc string
		switch {
		case c.File == "":
			loc = "(overall review)"
		case c.NewLine != "":
			loc = fmt.Sprintf("%s:%s", c.File, c.NewLine)
		case c.OldLine != "":
			loc = fmt.Sprintf("%s (removed line %s)", c.File, c.OldLine)
		default:
			loc = fmt.Sprintf("%s (whole file)", c.File)
		}
		text := strings.TrimSpace(c.Text)
		if text == "" {
			continue
		}
		// Indent multi-line comment bodies under the bullet so the
		// list shape stays readable at the receiving end.
		text = strings.ReplaceAll(text, "\n", "\n  ")
		fmt.Fprintf(&b, "- %s — %s\n", loc, text)
	}
	return strings.TrimRight(b.String(), "\n")
}

// tmuxOut runs `tmux <args...>` and returns combined stdout. Errors
// include stderr text so callers can pattern-match on tmux's English
// messages ("no server running", "can't find pane", etc).
func tmuxOut(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "tmux", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return "", err
		}
		return "", fmt.Errorf("%s: %w", msg, err)
	}
	return stdout.String(), nil
}
