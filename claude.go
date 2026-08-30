package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
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

// wdSession* tracks live pane-change state for all *-wd tmux sessions,
// updated by the background watcher and consumed by the listing page.
var (
	wdSessionsMu sync.RWMutex
	wdSpinning   = map[string]bool{}
	wdLastRaw    = map[string]string{}
	wdLastChange = map[string]time.Time{}
	// wdSessionRepo maps a *-wd tmux session name to the repo it was
	// spawned for, populated by ensureRepoSession. Consumed by
	// pollWdSessions when broadcasting turn-ended events so subscribers
	// know which repo to sync without having to reverse-derive the path
	// from the session name (which can ambiguate when repos in rootDir
	// and worktreesRoot share a basename).
	wdSessionRepo = map[string]repoRef{}
)

const wdSpinTimeout = 3 * time.Second

// turnEvent fires when a *-wd session transitions from "spinning" to
// "idle" — our proxy for "the agent just finished its turn." The
// payload carries enough for a sync client to act without another
// round-trip to the server.
type turnEvent struct {
	Kind    string `json:"kind"`    // "repos" or "worktrees"
	Name    string `json:"name"`    // repo basename / URL slug
	Session string `json:"session"` // tmux session name
	TS      int64  `json:"ts"`      // unix milliseconds
}

var (
	turnSubsMu sync.Mutex
	turnSubs   = map[chan turnEvent]struct{}{}
)

func subscribeTurnEvents() (<-chan turnEvent, func()) {
	ch := make(chan turnEvent, 32)
	turnSubsMu.Lock()
	turnSubs[ch] = struct{}{}
	turnSubsMu.Unlock()
	return ch, func() {
		turnSubsMu.Lock()
		delete(turnSubs, ch)
		turnSubsMu.Unlock()
		close(ch)
	}
}

// broadcastTurnEvent is non-blocking: slow subscribers drop events
// rather than backing up the poller. Subscribers are expected to be
// idempotent (a missed event just means the next agent turn re-fires
// the sync).
func broadcastTurnEvent(ev turnEvent) {
	turnSubsMu.Lock()
	defer turnSubsMu.Unlock()
	for ch := range turnSubs {
		select {
		case ch <- ev:
		default:
		}
	}
}

// recordRepoSession is called by ensureRepoSession on every cache hit
// or miss to keep wdSessionRepo current with the live sessions. The
// caller is the only place a *-wd session is created or observed by
// name+repo together, so this is the right registration point.
func recordRepoSession(sessionName, repoPath string) {
	var kind string
	switch {
	// worktreesRoot is inside rootDir, so it has to be tested first or
	// every worktree would register as the repo "wt".
	case worktreesRoot != "" && strings.HasPrefix(repoPath, worktreesRoot+string(os.PathSeparator)):
		kind = "worktrees"
	case rootDir != "" && strings.HasPrefix(repoPath, rootDir+string(os.PathSeparator)):
		kind = "repos"
	default:
		return // not under either root — nothing to broadcast for
	}
	slug := worktreeSlug(repoPath)
	ref := repoRef{
		abs:  repoPath,
		kind: kind,
		name: slug,
		url:  "/" + kind + "/" + slug + "/",
	}
	wdSessionsMu.Lock()
	wdSessionRepo[sessionName] = ref
	wdSessionsMu.Unlock()
}

// startSessionWatcher polls all *-wd tmux sessions once per second and
// marks each as spinning when its pane content has changed within the
// last wdSpinTimeout — a reliable proxy for "the agent is iterating."
func startSessionWatcher(ctx context.Context) {
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				pollWdSessions(ctx)
			}
		}
	}()
}

func pollWdSessions(ctx context.Context) {
	out, err := tmuxOut(ctx, "list-sessions", "-F", "#{session_name}\t#{session_created}\t#{pane_current_path}")
	if err != nil {
		if isTmuxDown(err) {
			wdSessionsMu.Lock()
			wdSpinning = map[string]bool{}
			wdLastRaw = map[string]string{}
			wdLastChange = map[string]time.Time{}
			wdSessionsMu.Unlock()
		}
		return
	}
	type snap struct {
		name    string
		created int64
		path    string
		raw     string
	}
	var snaps []snap
	active := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		name, rest, _ := strings.Cut(line, "\t")
		if !strings.HasSuffix(name, "-wd") {
			continue
		}
		createdStr, path, _ := strings.Cut(rest, "\t")
		active[name] = true
		raw, err := tmuxOut(ctx, "capture-pane", "-p", "-t", name+":0.0")
		if err != nil {
			continue
		}
		created, _ := strconv.ParseInt(strings.TrimSpace(createdStr), 10, 64)
		snaps = append(snaps, snap{name: name, created: created, path: path, raw: raw})
	}

	// Re-register any session missing from wdSessionRepo. The map is
	// in-memory, so a webdiff restart leaves it empty while the tmux
	// sessions it described are still running — without this, turn events
	// never fire for them again (and the sync client's auto-pull stays
	// dead) until someone hits /agent for that repo. The pane's cwd
	// avoids reverse-deriving the repo from the session name — that scan
	// can't tell a repo from a worktree sharing its basename. Use
	// pane_current_path, not session_path: the latter is frozen at
	// creation and doesn't follow a directory rename.
	wdSessionsMu.RLock()
	var unregistered []snap
	for _, s := range snaps {
		if _, ok := wdSessionRepo[s.name]; !ok && s.path != "" {
			unregistered = append(unregistered, s)
		}
	}
	wdSessionsMu.RUnlock()
	for _, s := range unregistered {
		recordRepoSession(s.name, s.path)
	}

	now := time.Now()
	var transitions []turnEvent
	// changed / wentIdle drive the history capture below; collected under
	// the lock but acted on after it's released (captureHistory shells out
	// to tmux and writes files — not work to hold the watcher mutex for).
	changed := make(map[string]bool, len(snaps))
	wentIdle := make(map[string]bool, len(snaps))
	wdSessionsMu.Lock()
	for _, s := range snaps {
		prev, seen := wdLastRaw[s.name]
		wdLastRaw[s.name] = s.raw
		if seen && s.raw != prev {
			wdLastChange[s.name] = now
			changed[s.name] = true
		}
		lc := wdLastChange[s.name]
		nowSpinning := !lc.IsZero() && now.Sub(lc) < wdSpinTimeout
		// Spinning→idle is the turn-end signal. We require the
		// previous tick to have been spinning to avoid firing on
		// every startup-idle session.
		if wdSpinning[s.name] && !nowSpinning {
			wentIdle[s.name] = true
			if ref, ok := wdSessionRepo[s.name]; ok {
				transitions = append(transitions, turnEvent{
					Kind:    ref.kind,
					Name:    ref.name,
					Session: s.name,
					TS:      now.UnixMilli(),
				})
			}
		}
		wdSpinning[s.name] = nowSpinning
	}
	for name := range wdSpinning {
		if !active[name] {
			delete(wdSpinning, name)
			delete(wdLastRaw, name)
			delete(wdLastChange, name)
			delete(wdSessionRepo, name)
		}
	}
	wdSessionsMu.Unlock()
	// Archive scrollback for sessions that produced new output (throttled)
	// or just finished a turn (forced, so the resting state is always
	// captured complete).
	for _, s := range snaps {
		if changed[s.name] || wentIdle[s.name] {
			captureHistory(ctx, s.name, s.created, wentIdle[s.name])
		}
	}
	for _, ev := range transitions {
		broadcastTurnEvent(ev)
	}
}

// handleSessionsSpinning returns a JSON object of session-name → true
// for every *-wd session currently spinning, for the listing page's
// live spinner indicators.
func handleSessionsSpinning(w http.ResponseWriter, r *http.Request) {
	wdSessionsMu.RLock()
	result := make(map[string]bool)
	for name, spinning := range wdSpinning {
		if spinning {
			result[name] = true
		}
	}
	wdSessionsMu.RUnlock()
	writeJSON(w, http.StatusOK, result)
}

func registerAgentRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/comments/send", handleCommentsSend)
	mux.HandleFunc("/api/pane/stream", handlePaneStream)
	mux.HandleFunc("/api/pane/input", handlePaneInput)
	mux.HandleFunc("/api/pane/restart", handlePaneRestart)
	mux.HandleFunc("/api/pane/history-url", handlePaneHistoryURL)
	mux.HandleFunc("/api/pane/attach", handlePaneAttach)
	mux.HandleFunc("/api/sessions/spinning", handleSessionsSpinning)
	mux.HandleFunc("/api/sessions/turn-events", handleTurnEvents)
	mux.HandleFunc("/api/worktrees/create", handleWorktreeCreate)
	mux.HandleFunc("/api/worktrees/remove", handleWorktreeRemove)
	mux.HandleFunc("/api/history/forget", handleHistoryForget)
}

// maxAttachmentBytes is the upper bound on a single attachment after
// base64 decoding. 20 MB comfortably covers logs and small binaries
// without inviting OOM from a runaway upload; the base64'd JSON body
// itself lands ~27 MB which still fits well inside default http body
// limits.
const maxAttachmentBytes = 20 * 1024 * 1024

// attachmentNameRE strips a filename down to characters safe in a
// disk path (and a tmux/terminal copy-paste). Anything outside the
// allowed set collapses to a single underscore so two adjacent bad
// runs don't blow the name out.
var attachmentNameRE = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// sanitizeAttachmentName turns a user-supplied filename into one safe
// to write to disk. Drops directory components, normalises bad chars
// to underscores, caps the length, and substitutes a sensible default
// when the input strips down to nothing.
func sanitizeAttachmentName(name string) string {
	name = filepath.Base(strings.TrimSpace(name))
	if name == "." || name == "/" || name == "\\" {
		name = ""
	}
	name = attachmentNameRE.ReplaceAllString(name, "_")
	name = strings.Trim(name, "._")
	if name == "" {
		name = "attachment.txt"
	}
	if len(name) > 80 {
		name = name[:80]
	}
	return name
}

// handlePaneAttach writes a single attachment to disk and returns the
// absolute path so the client can paste it into the next message as
// an appendix. The pane has to be a webdiff-managed session — we
// derive the session name from the paneID so an attacker who got a
// hold of a foreign pane id can't drop files using our endpoint.
//
// Attachments live under attachmentsRoot/<session>/<timestamp>-<name>.
// One subdir per session means a session restart can clear just its
// own staging area without sweeping everyone else's pending uploads.
func handlePaneAttach(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Cap the body before we even read it: a 30 MB ceiling lets a
	// fully base64'd 20 MB binary land with room for envelope, and
	// stops a hostile client from streaming gigabytes into json.Decode.
	r.Body = http.MaxBytesReader(w, r.Body, int64(maxAttachmentBytes*4/3+4096))
	var req struct {
		PaneID        string `json:"paneID"`
		Name          string `json:"name"`
		ContentBase64 string `json:"contentBase64"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.PaneID == "" {
		http.Error(w, "paneID required", http.StatusBadRequest)
		return
	}
	if req.ContentBase64 == "" {
		http.Error(w, "contentBase64 required", http.StatusBadRequest)
		return
	}
	data, err := base64.StdEncoding.DecodeString(req.ContentBase64)
	if err != nil {
		http.Error(w, "contentBase64 not valid base64: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(data) > maxAttachmentBytes {
		http.Error(w, fmt.Sprintf("attachment too large (%d bytes, max %d)", len(data), maxAttachmentBytes), http.StatusRequestEntityTooLarge)
		return
	}
	ctx := r.Context()
	sessionOut, err := tmuxOut(ctx, "display-message", "-t", req.PaneID, "-p", "#{session_name}")
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "tmux display-message failed: "+err.Error(), nil)
		return
	}
	session := strings.TrimSpace(sessionOut)
	if !strings.HasSuffix(session, "-wd") {
		http.Error(w, "pane is not in a webdiff-managed session", http.StatusForbidden)
		return
	}
	if attachmentsRoot == "" {
		http.Error(w, "attachments root not configured", http.StatusInternalServerError)
		return
	}
	dir := filepath.Join(attachmentsRoot, session)
	if err := os.MkdirAll(dir, 0700); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "mkdir attachments dir: "+err.Error(), nil)
		return
	}
	name := sanitizeAttachmentName(req.Name)
	final := filepath.Join(dir, fmt.Sprintf("%d-%s", time.Now().UnixNano(), name))
	if err := os.WriteFile(final, data, 0600); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "write attachment: "+err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"path":  final,
		"name":  name,
		"bytes": len(data),
	})
}

// handleTurnEvents streams turn-ended events as Server-Sent Events.
// Each event is a JSON turnEvent. The connection stays open until the
// client disconnects; the subscriber channel is closed on cleanup so
// pollWdSessions stops blasting it.
//
// Subscribers connect from the desktop client to auto-pull a repo
// when its agent finishes a turn. Browsers could also subscribe to
// drive future UI; v1 only uses it from client_events.go.
func handleTurnEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	ch, cancel := subscribeTurnEvents()
	defer cancel()

	// Initial comment so any HTTP-2 / proxy buffer flushes the headers
	// and clients see a connected state right away.
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	ctx := r.Context()
	// Light keepalive so an idle subscriber's intermediary doesn't
	// time the connection out. Half of typical 60 s proxy idle.
	keepalive := time.NewTicker(30 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-keepalive.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case ev := <-ch:
			data, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// defaultRemote returns the name of the remote to fetch from. It prefers
// "origin" when present, else falls back to the sole remote when exactly
// one is configured (repos like tailscale name their only remote "ro").
// Returns ("", false) when there's no unambiguous choice.
func defaultRemote(repo string) (string, bool) {
	out, err := exec.Command("git", "-C", repo, "remote").Output()
	if err != nil {
		return "", false
	}
	remotes := strings.Fields(strings.TrimSpace(string(out)))
	for _, r := range remotes {
		if r == "origin" {
			return "origin", true
		}
	}
	if len(remotes) == 1 {
		return remotes[0], true
	}
	return "", false
}

// handleWorktreeCreate creates a managed worktree at
// `<worktreesRoot>/<repo>/<leaf>` for a named branch of a repo under
// rootDir, and reports it under the flattened `<repo>-<leaf>` slug (see
// worktreeSlug). If the branch already exists locally git just checks it
// out into the new worktree; otherwise it's created via `-b` from the
// fetched <remote>/main (when "from main" is set), the current HEAD, or
// <remote>/<branch> if the branch already exists on the remote. The
// remote is resolved via defaultRemote rather than hardcoding "origin".
func handleWorktreeCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Repo     string `json:"repo"`
		Branch   string `json:"branch"`
		FromMain bool   `json:"fromMain"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	req.Repo = strings.TrimSpace(req.Repo)
	req.Branch = strings.TrimSpace(req.Branch)
	if req.Repo == "" || req.Branch == "" {
		http.Error(w, "repo and branch required", http.StatusBadRequest)
		return
	}
	ref, ok := resolveRepoRef("/repos/" + req.Repo + "/")
	if !ok || ref.kind != "repos" || !isGitRepo(ref.abs) {
		http.Error(w, "unknown repo", http.StatusNotFound)
		return
	}
	leaf := worktreeLeaf(req.Branch)
	if leaf == "" {
		http.Error(w, "branch name has no usable characters", http.StatusBadRequest)
		return
	}
	name := ref.name + "-" + leaf
	wtPath := filepath.Join(worktreesRoot, ref.name, leaf)
	if _, err := os.Stat(wtPath); err == nil {
		writeJSON(w, http.StatusOK, map[string]string{"url": "/worktrees/" + name + "/", "path": wtPath})
		return
	}
	// Slugs flatten two path segments, so distinct paths can collide.
	// Refuse rather than shadow an existing worktree, which resolveRepoRef
	// would then resolve to whichever sorts first.
	for _, wt := range listManagedWorktrees() {
		if wt.Name == name {
			http.Error(w, "worktree slug "+name+" is already taken by "+wt.Path, http.StatusConflict)
			return
		}
	}
	if err := os.MkdirAll(filepath.Dir(wtPath), 0700); err != nil {
		http.Error(w, "cannot create worktree parent dir: "+err.Error(), http.StatusInternalServerError)
		return
	}

	remote, hasRemote := defaultRemote(ref.abs)
	if hasRemote {
		exec.Command("git", "-C", ref.abs, "fetch", remote, req.Branch).Run()
	}

	localExists := exec.Command("git", "-C", ref.abs, "rev-parse", "--verify", "refs/heads/"+req.Branch).Run() == nil
	remoteBranch := "refs/remotes/" + remote + "/" + req.Branch
	remoteExists := hasRemote && exec.Command("git", "-C", ref.abs, "rev-parse", "--verify", remoteBranch).Run() == nil

	var addCmd *exec.Cmd
	if localExists {
		addCmd = exec.Command("git", "-C", ref.abs, "worktree", "add", wtPath, req.Branch)
	} else if remoteExists {
		addCmd = exec.Command("git", "-C", ref.abs, "worktree", "add", "-b", req.Branch, wtPath, remoteBranch)
	} else if req.FromMain {
		base := "main"
		if hasRemote {
			exec.Command("git", "-C", ref.abs, "fetch", remote, "main").Run()
			if exec.Command("git", "-C", ref.abs, "rev-parse", "--verify", "refs/remotes/"+remote+"/main").Run() == nil {
				base = "refs/remotes/" + remote + "/main"
			}
		}
		addCmd = exec.Command("git", "-C", ref.abs, "worktree", "add", "-b", req.Branch, wtPath, base)
	} else {
		addCmd = exec.Command("git", "-C", ref.abs, "worktree", "add", "-b", req.Branch, wtPath)
	}
	if out, err := addCmd.CombinedOutput(); err != nil {
		http.Error(w, "git worktree add failed: "+strings.TrimSpace(string(out)), http.StatusBadGateway)
		return
	}
	if err := statsManager.RefreshDiscovery(r.Context()); err != nil {
		http.Error(w, "refresh worktree stats: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"url": "/worktrees/" + name + "/", "path": wtPath})
}

// handleWorktreeRemove tears down a managed worktree: kills its tmux
// session (if any), runs `git worktree remove --force` so uncommitted
// throwaway work doesn't block teardown, and wipes the warm diff
// cache for it. Falls back to RemoveAll for the dir if git left
// anything behind (e.g. a manually-corrupted worktree).
func handleWorktreeRemove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	ref, ok := resolveRepoRef("/worktrees/" + req.Name + "/")
	if !ok || ref.kind != "worktrees" {
		http.Error(w, "unknown worktree", http.StatusNotFound)
		return
	}
	ctx := r.Context()

	sessName := repoSessionName(ref.abs)
	if sessionExists(ctx, sessName) {
		// Removing the worktree kills the session and the scrollback with
		// it; archive it first so the history survives the teardown.
		snapshotSessionHistory(ctx, sessName)
		if _, err := tmuxOut(ctx, "kill-session", "-t", sessName); err != nil {
			http.Error(w, "tmux kill-session failed: "+err.Error(), http.StatusBadGateway)
			return
		}
	}
	// Outside the branch above: the usual order is that the session died on
	// its own (leaving its archive flagged live) and only later did the
	// user remove the worktree. Removing the worktree is unambiguously
	// deliberate either way, and once the directory is gone there's nothing
	// left to resume into.
	clearSessionLive(sessName)

	// `git worktree remove` needs to run from inside the parent repo, not
	// the worktree itself (the worktree's git common dir tells us where).
	parent, ok := worktreeParentGitDir(ref.abs)
	if ok {
		removeCmd := exec.Command("git", "-C", parent, "worktree", "remove", "--force", ref.abs)
		if out, err := removeCmd.CombinedOutput(); err != nil {
			// Fall through to filesystem removal — the parent repo may
			// already be gone, in which case the orphan worktree dir is
			// all that's left.
			_ = out
		}
	}
	// Belt-and-braces: ensure both the worktree dir and its diff cache
	// are gone regardless of how `git worktree remove` fared.
	_ = os.RemoveAll(ref.abs)
	_ = indexCache.Remove("worktrees-" + ref.name)
	// Don't leave an empty `wt/<repo>` behind. Remove, not RemoveAll, so
	// it no-ops while sibling worktrees are still there.
	if grouping := filepath.Dir(ref.abs); grouping != worktreesRoot {
		_ = os.Remove(grouping)
	}
	// If the worktree was already broken before we touched it, git's
	// remove command was skipped above and any registered worktree
	// metadata in some parent repo's .git/worktrees/ is now stale.
	// Prune every known repo so future `git worktree list` calls stay
	// clean. Cheap — one tiny git call per repo.
	pruneAllRepos()
	if err := statsManager.RefreshDiscovery(ctx); err != nil {
		http.Error(w, "refresh repo stats: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// pruneAllRepos runs `git worktree prune` against every git repo
// directly under rootDir. Called after a worktree removal to clear
// stale `.git/worktrees/<name>` entries that the worktree's parent
// repo may still be holding — typically for broken worktrees where
// our preferred `git -C <parent> worktree remove` path didn't run.
func pruneAllRepos() {
	entries, err := os.ReadDir(rootDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		sub := filepath.Join(rootDir, e.Name())
		if !isGitRepo(sub) {
			continue
		}
		_ = exec.Command("git", "-C", sub, "worktree", "prune").Run()
	}
}

// worktreeParentGitDir returns the parent repo's working tree path
// for a managed worktree, derived from `git rev-parse
// --git-common-dir`. The result is what we pass to `git -C` for
// `worktree remove`. ok=false when git can't introspect the dir
// (orphaned worktree) — the caller falls back to plain filesystem
// removal.
func worktreeParentGitDir(wtPath string) (string, bool) {
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
	return filepath.Dir(common), true
}

// ownerBranchPrefix is stripped from a branch when naming its worktree
// dir: every branch here is prefixed with it, so keeping it would just
// pad every path with the same seven characters.
const ownerBranchPrefix = "tomhjp/"

// worktreeLeaf reduces a branch name to the leaf directory its worktree
// lives in, e.g. "tomhjp/fix-x" → "fix-x" and "gabriel/queue-metrics" →
// "gabriel-queue-metrics". Any remaining path separator folds to a dash
// rather than nesting deeper, since worktreeSlug expects exactly one
// level below the repo. Empty input or input that reduces to nothing
// returns "" — the caller rejects that case so we never create a
// worktree dir with an empty name.
func worktreeLeaf(branch string) string {
	s := strings.ToLower(branch)
	s = strings.TrimPrefix(s, ownerBranchPrefix)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '.':
			b.WriteRune(r)
		case r == '/':
			b.WriteRune('-')
		}
	}
	// A leading dot would make the dir invisible to managedWorktreePaths.
	return strings.TrimLeft(b.String(), ".")
}

// allowedPaneKeys is the whitelist for /api/pane/input's `key` field.
// Anything not on this list is rejected so the endpoint can't be
// abused to fire off arbitrary tmux key sequences (Ctrl-C, M-x, etc.)
// from a compromised browser session — `text` is the documented way
// to send freeform input. The set is small on purpose: extend it only
// when there's a concrete UI need.
var allowedPaneKeys = map[string]bool{
	"Up":     true,
	"Down":   true,
	"Escape": true,
	"Enter":  true,
}

type paneInputRequest struct {
	PaneID string `json:"paneID"`
	Key    string `json:"key,omitempty"`
	Text   string `json:"text,omitempty"`
}

// handlePaneInput sends a single tmux key (`key`) or a freeform message
// followed by Enter (`text`) to a pane. It's the back-end for the
// stream page's footer toolbar so the reviewer can drive the agent's
// TUI session — answer follow-up prompts, scroll history, kick off a
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

// webdiffSessionForPane resolves the tmux session that owns paneID and
// verifies it's webdiff-managed. The `-wd` suffix is webdiff's marker;
// arbitrary user sessions must not be reachable from a browser request.
// A non-zero status is an HTTP error code with a matching message.
func webdiffSessionForPane(ctx context.Context, paneID string) (name string, status int, msg string) {
	out, err := tmuxOut(ctx, "display-message", "-t", paneID, "-p", "#{session_name}")
	if err != nil {
		return "", http.StatusBadGateway, "tmux display-message failed: " + err.Error()
	}
	name = strings.TrimSpace(out)
	if name == "" {
		return "", http.StatusBadGateway, "no session for pane"
	}
	if !strings.HasSuffix(name, "-wd") {
		return "", http.StatusForbidden, "session is not webdiff-managed"
	}
	return name, 0, ""
}

// handlePaneHistoryURL archives the pane's current scrollback and returns
// the /history/<id>.txt URL without killing the session. The restart popup
// fetches this so it can show a copyable history link for the next session
// *before* the user confirms the kill.
func handlePaneHistoryURL(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		PaneID string `json:"paneID"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.PaneID == "" {
		http.Error(w, "paneID required", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	name, status, msg := webdiffSessionForPane(ctx, req.PaneID)
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	resp := struct {
		HistoryURL string `json:"historyURL,omitempty"`
	}{}
	if histID := snapshotSessionHistory(ctx, name); histID != "" {
		resp.HistoryURL = "/history/" + histID + ".txt"
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// handlePaneRestart kills the webdiff-managed tmux session that owns
// paneID. The client navigates to /agent<refURL> after this returns,
// which respawns a fresh session with a clean scrollback. Used by the
// stream page's "restart session" button when accumulated history has
// made the live tail sluggish to render.
func handlePaneRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		PaneID string `json:"paneID"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.PaneID == "" {
		http.Error(w, "paneID required", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	name, status, msg := webdiffSessionForPane(ctx, req.PaneID)
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	// Archive the scrollback before we throw it away — the whole point of
	// "restart" is a clean slate, so this is the last chance to keep it.
	histID := snapshotSessionHistory(ctx, name)
	if _, err := tmuxOut(ctx, "kill-session", "-t", name); err != nil {
		writeJSONError(w, http.StatusBadGateway, "tmux kill-session failed: "+err.Error(), nil)
		return
	}
	// Only after the kill succeeded: a failed kill leaves the session
	// running, and clearing early would drop it off the home page while
	// it's still alive.
	clearSessionLive(name)
	resp := struct {
		HistoryURL string `json:"historyURL,omitempty"`
	}{}
	if histID != "" {
		resp.HistoryURL = "/history/" + histID + ".txt"
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
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
	// "/repos/webdiff" or "/worktrees/webdiff-feat-x"). Translate it to
	// the absolute filesystem path so the prompt names a location the
	// agent can actually `cd` into. Fall back to the raw value if
	// resolution fails — better a confusing prompt than no prompt.
	repoPath := req.Repo
	if ref, ok := resolveRepoRef(req.Repo); ok {
		repoPath = ref.abs
	}
	prompt := formatPrompt(repoPath, req.Comments)

	// No agent id: sending comments only spawns when the session has
	// died, and there's no reviewer choice attached to a send. That
	// respawn gets the default agent.
	pane, err := ensureRepoSession(ctx, repoPath, "")
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, err.Error(), nil)
		return
	}

	if err := sendToPane(ctx, pane.ID, prompt); err != nil {
		writeJSONError(w, http.StatusBadGateway, "tmux send failed: "+err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sent": len(req.Comments)})
}

// handlePaneStream serves a Server-Sent Events stream of `tmux
// capture-pane` snapshots for paneID. The browser opens it after a
// successful send so the reviewer can watch the agent's output without
// switching to tmux. There's no idle detection — the browser closes the
// stream when the user clicks "stop" (or navigates away, which cancels
// the request context). An idle pane emits nothing, so we send a
// periodic SSE comment as a keepalive; without it a peer that vanished
// without a FIN (suspended mobile browser, dropped VPN path) leaves the
// connection established forever, since a stream with no writes never
// notices the other end is gone.
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
	//   • shifted history + new tail      → {drop: k, keep: n, append: tail}
	//   • pure append                     → {drop: 0, keep: oldLen, append: tail}
	//   • in-place tail redraw            → {drop: 0, keep: prefix, append: tail}
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
	// (the agent's spinners / typing) emits fire at a steady ~750 ms
	// cadence so mobile clients don't pay for every cursor blink.
	//
	// `-S` starts the capture above the visible area, so the reviewer
	// sees the scrollback the agent has produced too — without it,
	// anything that has scrolled off-screen in the tmux pane is
	// invisible here. But `-S -` (the whole history) makes each poll
	// cost O(history): with the default 100k-line history-limit the
	// tmux server ends up pinned serializing hundreds of KB per tick,
	// so cap the capture at the most recent streamCaptureLines lines.
	const (
		streamPollInterval    = 250 * time.Millisecond
		streamEmitMinInterval = 750 * time.Millisecond
		streamCaptureLines    = 2000
	)
	var lastRaw string
	var pending []string
	var lastSent []string
	var lastEmit time.Time
	snapshot := func() bool {
		out, err := tmuxOut(ctx, "capture-pane", "-p", "-e", "-S", strconv.Itoa(-streamCaptureLines), "-t", paneID)
		if err != nil {
			send(map[string]string{"error": err.Error()})
			return false
		}
		if out != lastRaw {
			lastRaw = out
			screen, _ := terminal.NewScreen()
			screen.Write([]byte(out))
			rendered := strings.TrimRight(screen.AsHTML(), "\n")
			// Claude Code (and tools like ripgrep with --hyperlink-format)
			// emit OSC 8 hyperlinks for file paths, which terminal-to-html
			// renders as `<a href="file:///…">`. The browser is on a
			// different host to the sandbox so those URLs are dead; rewrite
			// any whose target lives under a known repo/worktree to /file/…
			// instead. Out-of-scope paths are left alone — no worse than
			// today, and the user can still copy the absolute path out.
			rendered = rewriteFileHrefs(rendered)
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
			drop, keep, appendLines := streamDelta(lastSent, pending)
			if drop == 0 && keep == len(lastSent) && len(appendLines) == 0 {
				lastSent = pending
				pending = nil
				lastEmit = time.Now()
				return true
			}
			ok = send(map[string]any{"drop": drop, "keep": keep, "append": appendLines})
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
	// 30s matches handleTurnEvents. The write itself only errors once
	// TCP gives up on retransmits, but each keepalive restarts that
	// clock, so a dead peer is reaped within minutes rather than never.
	keepalive := time.NewTicker(30 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-keepalive.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-ticker.C:
			if !snapshot() {
				return
			}
		}
	}
}

// streamDelta finds a drop+keep+append patch that turns the client's
// last-known buffer (`old`) into `new`:
//
//	new = old[drop : drop+keep] + appendLines
//
// Besides ordinary terminal output, this handles full-screen agents such as
// Pi. Pi keeps a live footer at the bottom and inserts completed output just
// above it, so the old buffer is not a prefix of the new one even though most
// of its rows are unchanged. Keeping the longest old suffix that matches the
// new prefix lets the browser retain those DOM nodes and its scroll anchor;
// only the live tail is replaced.
//
// The scan is O(N²) worst case, but normal candidates fail on their first row
// and the winning candidate then consumes almost the entire capture.
func streamDelta(old, new []string) (drop, keep int, appendLines []string) {
	bestDrop, bestKeep := len(old), 0
	for candidate := 0; candidate < len(old); candidate++ {
		n := min(len(old)-candidate, len(new))
		matched := 0
		for matched < n && old[candidate+matched] == new[matched] {
			matched++
		}
		if matched > bestKeep {
			bestDrop, bestKeep = candidate, matched
		}
		// No later suffix can beat this one.
		if bestKeep >= len(old)-candidate-1 {
			break
		}
	}
	// A lone matching row in an otherwise redrawn screen is more likely a
	// coincidental blank/separator than a useful anchor. Preserve the previous
	// all-replace behaviour in that case.
	if bestKeep < 2 && bestKeep < len(old) {
		bestDrop, bestKeep = len(old), 0
	}
	return bestDrop, bestKeep, new[bestKeep:]
}

// ensureRepoSession returns the agent pane in the tmux session
// dedicated to repoPath, creating both session and agent process on
// first call. The session is named `<basename>-wd` so it's easy to
// spot in `tmux list-sessions` output and won't collide with the
// user's own sessions. When the session already exists we just look
// up its initial pane (window 0, pane 0) — webdiff always puts the
// agent there, and ignoring later splits the user might have made
// keeps the behaviour predictable.
//
// agentID selects which agent to spawn (see resolveAgent); it only
// matters on the creating call, since an existing session is already
// running whatever it was started with.
func ensureRepoSession(ctx context.Context, repoPath, agentID string) (Pane, error) {
	name := repoSessionName(repoPath)
	recordRepoSession(name, repoPath)
	if sessionExists(ctx, name) {
		out, err := tmuxOut(ctx, "display", "-t", name+":0.0", "-p", "#{pane_id}")
		if err != nil {
			return Pane{}, fmt.Errorf("tmux display: %w", err)
		}
		return Pane{ID: strings.TrimSpace(out), Label: name}, nil
	}
	wantName, wantCmd := resolveAgent(agentID)

	// `bash -lc <cmd>` (rather than execing the agent directly) so
	// the new pane picks up the user's login PATH — the agent binary
	// typically lives in ~/.local/bin which isn't on the systemd unit's
	// inherited PATH. The command is passed as a single string so any
	// flags configured by the operator are honoured. `-c repoPath` so
	// the agent starts already cd'd to the repo.
	//
	// history-limit is fixed per-window at window creation time, so we
	// spawn a throwaway bash window 0 just to anchor the session, bump
	// the session's history-limit, then open the real agent window
	// (which inherits the new limit). kill-window + move-window
	// relocates the agent back to :0.0 so everything downstream can
	// keep assuming that target.
	if _, err := tmuxOut(ctx, "new-session", "-d", "-s", name, "-c", repoPath, "bash"); err != nil {
		return Pane{}, fmt.Errorf("tmux new-session: %w", err)
	}
	if _, err := tmuxOut(ctx, "set-option", "-t", name, "history-limit", "100000"); err != nil {
		return Pane{}, fmt.Errorf("tmux set-option history-limit: %w", err)
	}
	out, err := tmuxOut(ctx, "new-window", "-t", name, "-c", repoPath, "-P", "-F", "#{pane_id}", "bash", "-lc", wantCmd)
	if err != nil {
		return Pane{}, fmt.Errorf("tmux new-window: %w", err)
	}
	paneID := strings.TrimSpace(out)
	if paneID == "" {
		return Pane{}, errors.New("tmux didn't return a pane id")
	}
	if _, err := tmuxOut(ctx, "kill-window", "-t", name+":0"); err != nil {
		return Pane{}, fmt.Errorf("tmux kill-window: %w", err)
	}
	if _, err := tmuxOut(ctx, "move-window", "-s", name+":1", "-t", name+":0"); err != nil {
		return Pane{}, fmt.Errorf("tmux move-window: %w", err)
	}

	// Wait until pane_current_command matches the agent's binary name
	// (the first token of its command) AND the pane content has been
	// visually stable for stableFor. The TUI plays an animated banner
	// while initialising; the static input prompt that follows is the
	// reliable signal that it's ready to receive bracketed paste.
	// Polling at 50 ms gives us ~6 checks per stable window without
	// hammering tmux.
	const (
		pollInterval = 50 * time.Millisecond
		stableFor    = 300 * time.Millisecond
	)
	deadline := time.Now().Add(15 * time.Second)
	var lastPane string
	var stableAt time.Time
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return Pane{}, ctx.Err()
		default:
		}
		cmd, err := tmuxOut(ctx, "display", "-t", paneID, "-p", "#{pane_current_command}")
		if err != nil || strings.TrimSpace(cmd) != wantName {
			time.Sleep(pollInterval)
			continue
		}
		content, err := tmuxOut(ctx, "capture-pane", "-p", "-t", paneID)
		if err != nil {
			time.Sleep(pollInterval)
			continue
		}
		if content != lastPane {
			lastPane = content
			stableAt = time.Now()
		} else if !stableAt.IsZero() && time.Since(stableAt) >= stableFor {
			return Pane{ID: paneID, Label: name + " (just spawned)"}, nil
		}
		time.Sleep(pollInterval)
	}
	return Pane{}, fmt.Errorf("%s didn't start within 15 s", wantName)
}

// handleStartSession ensures the ref's webdiff-<base> tmux session
// exists, then 303-redirects to /stream<refUrl>. Mounted at /agent/
// as a prefix handler so the URL suffix is the ref's diff URL path
// (/repos/<name>/ or /worktrees/<name>/), mirroring /stream/. The
// diff page's agent header button uses this so a single button works
// whether the session is already running (fast lookup) or needs to
// be spawned (~5 s for the bash → agent exec to settle).
//
// `?agent=<id>` picks which agent to spawn, and is echoed onto the
// redirect so a reload of the stream page still shows which one is
// running.
func handleStartSession(w http.ResponseWriter, r *http.Request) {
	repoURL := strings.TrimPrefix(r.URL.Path, "/agent")
	ref, ok := resolveRepoRef(repoURL)
	if !ok {
		http.NotFound(w, r)
		return
	}
	agentID := r.URL.Query().Get("agent")
	if _, err := ensureRepoSession(r.Context(), ref.abs, agentID); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	http.Redirect(w, r, "/stream"+ref.url+agentQuery(agentID), http.StatusSeeOther)
}

// repoStreamPane returns the pane id, session label and running agent
// id for the repo's webdiff tmux session if it already exists. Returns
// zero values when the session hasn't been spawned yet (the diff page
// hides the stream link in that case — readers don't need a "stream"
// link before there's anything to stream). Failures past existence (the
// display lookup erroring out) are also reported as not-running so a
// transient tmux hiccup just removes the link rather than breaking page
// render.
func repoStreamPane(ctx context.Context, repoPath string) (paneID, label, agentID string) {
	name := repoSessionName(repoPath)
	if !sessionExists(ctx, name) {
		return "", "", ""
	}
	out, err := tmuxOut(ctx, "display", "-t", name+":0.0", "-p", "#{pane_id}\t#{pane_start_command}")
	if err != nil {
		return "", "", ""
	}
	id, start, _ := strings.Cut(strings.TrimSpace(out), "\t")
	return id, name, agentFromStartCommand(start)
}

// agentFromStartCommand extracts the agent id from a pane's
// pane_start_command, which for a webdiff-spawned pane is the
// `bash -lc <cmd>` wrapper ensureRepoSession used. Returns "" when the
// command names something we don't recognise — a user's own pane, or a
// session left over from a differently configured -agent flag — so
// callers fall back to the default rather than showing a bogus label.
func agentFromStartCommand(start string) string {
	cmd := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(start), "bash -lc "))
	if cmd == "" {
		return ""
	}
	// Compare against the whole command so an -agent value carrying
	// flags still matches, then fall back to its first token, which is
	// what a knownAgents id is.
	if cmd == agentCmd {
		return agentName
	}
	name, _, _ := strings.Cut(cmd, " ")
	if _, ok := knownAgents[name]; ok {
		return name
	}
	return ""
}

// repoSessionName derives the tmux session name for a repo path. The
// slug gives something readable in `tmux list-sessions` and the `-wd`
// suffix scopes it to this tool without burying the repo name at the
// end of a long prefix. Characters tmux disallows in session names
// (`.`, `:`) are squashed to dashes.
func repoSessionName(repoPath string) string {
	base := worktreeSlug(repoPath)
	if base == "" || base == "." || base == "/" {
		base = "default"
	}
	base = strings.NewReplacer(".", "-", ":", "-", " ", "-").Replace(base)
	return base + "-wd"
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
	Session  string // session name, e.g. "claude"
	Index    string // window.pane index within the session, e.g. "0.1"
	PaneID   string // tmux pane id, e.g. "%23"
	Command  string // pane_current_command, e.g. "claude" or "vim"
	Title    string // pane_title; only meaningful when distinct from Command
	LastUsed int64  // window_activity unix seconds; 0 if unknown
}

// listAllPanes returns every tmux pane on the host, sorted by session
// then window.pane index. Used by the root listing's "tmux sessions"
// section to surface the full tmux state, independent of any
// webdiff-managed session.
//
// LastUsed comes from `window_activity` rather than `pane_last_used`:
// the latter would be per-pane but only landed in tmux 3.5, and we want
// to keep working on the 3.x line that ships with current LTS distros.
// window_activity updates on any pane activity in the window, which is
// close enough for the at-a-glance "when was something happening here".
func listAllPanes(ctx context.Context) ([]SessionPane, error) {
	out, err := tmuxOut(ctx, "list-panes", "-a", "-F",
		"#{session_name}\t#{window_index}.#{pane_index}\t#{pane_id}\t#{pane_current_command}\t#{window_activity}\t#{pane_title}")
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
		fields := strings.SplitN(line, "\t", 6)
		if len(fields) < 5 {
			continue
		}
		sp := SessionPane{
			Session: fields[0],
			Index:   fields[1],
			PaneID:  fields[2],
			Command: fields[3],
		}
		if ts, err := strconv.ParseInt(fields[4], 10, 64); err == nil {
			sp.LastUsed = ts
		}
		if len(fields) == 6 {
			sp.Title = fields[5]
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
// Routes through a tmux paste buffer rather than send-keys -l: tmux
// reads the prompt from stdin (no argv limit) and `paste-buffer -p`
// emits the start/end markers atomically. The earlier three-call
// send-keys path could leave the pane locked in paste mode if the
// prompt argv exceeded the kernel's MAX_ARG_STRLEN (~128 KB) — exec
// would fail between the start marker and the end marker and the
// agent's TUI would silently swallow every subsequent keystroke into
// the half-open paste buffer.
//
// We pause between the paste and the submit Enter: agent TUIs
// re-render after a paste, and an Enter that lands during the redraw
// is sometimes consumed as "insert newline" rather than "submit". The
// 100 ms wait is well below human-perceptible latency and avoids the
// race in practice.
func sendToPane(ctx context.Context, paneID, prompt string) error {
	bufName := "webdiff-paste-" + strings.TrimPrefix(paneID, "%")
	cmd := exec.CommandContext(ctx, "tmux", "load-buffer", "-b", bufName, "-")
	cmd.Stdin = strings.NewReader(prompt)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return fmt.Errorf("tmux load-buffer: %w", err)
		}
		return fmt.Errorf("tmux load-buffer: %s: %w", msg, err)
	}
	defer tmuxOut(context.Background(), "delete-buffer", "-b", bufName)
	if _, err := tmuxOut(ctx, "paste-buffer", "-p", "-b", bufName, "-t", paneID); err != nil {
		return err
	}
	time.Sleep(100 * time.Millisecond)
	if _, err := tmuxOut(ctx, "send-keys", "-t", paneID, "Enter"); err != nil {
		return err
	}
	return nil
}

// formatPrompt turns the per-comment tuples into a single multiline
// prompt the agent TUI will see as one paste.
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

// fileHrefRE matches the `href="file://…"` attributes that
// terminal-to-html emits when the agent prints an OSC 8 hyperlink. The
// path is captured raw and HTML-decoded inside the rewriter — file
// paths rarely contain HTML-special characters, but `&` in a filename
// would arrive as `&amp;` and round-tripping cleanly is cheap.
var fileHrefRE = regexp.MustCompile(`href="file://([^"]*)"`)

// rewriteFileHrefs walks every `href="file://…"` in `s` and, for those
// whose target lives under a webdiff-managed repo or worktree, rewrites
// the href to point at /file/<kind>/<name>/<rest>. The browser sits on
// a different machine to the sandbox, so the original file:// URLs are
// broken; pointing them at the file viewer turns each into a working
// click-through into the source. Anything outside rootDir / worktreesRoot
// is left as-is so the user can still see and copy the path.
func rewriteFileHrefs(s string) string {
	if !strings.Contains(s, `href="file://`) {
		return s
	}
	return fileHrefRE.ReplaceAllStringFunc(s, func(match string) string {
		sub := fileHrefRE.FindStringSubmatch(match)
		if len(sub) != 2 {
			return match
		}
		// Reverse the HTML escape buildkite applied to the URL, then
		// parse so url.Path is properly percent-decoded and any
		// trailing #fragment is split off.
		raw := html.UnescapeString(sub[1])
		u, err := url.Parse("file://" + raw)
		if err != nil || u.Path == "" {
			return match
		}
		mapped := mapFilePathToWebdiff(u.Path, u.Fragment)
		if mapped == "" {
			return match
		}
		return `href="` + html.EscapeString(mapped) + `"`
	})
}

// mapFilePathToWebdiff turns an absolute filesystem path into the /file
// URL that serves it, or returns "" if the path doesn't live under any
// repo or worktree webdiff manages. Both rootDir and worktreesRoot are
// resolved fresh each call rather than cached — they're set once at
// startup and the cost is a couple of stat-free Clean calls per OSC 8
// link, which is negligible at the SSE emit cadence.
func mapFilePathToWebdiff(absPath, fragment string) string {
	sep := string(filepath.Separator)
	// worktreesRoot sits inside rootDir, so it has to be tried first —
	// otherwise every worktree path matches the repos scope as the repo
	// "wt". A worktree also spends two path segments (<repo>/<leaf>) on
	// its identity where a repo spends one.
	for _, scope := range []struct {
		kind    string
		root    string
		nameLen int
	}{
		{"worktrees", worktreesRoot, 2},
		{"repos", rootDir, 1},
	} {
		if scope.root == "" {
			continue
		}
		rootAbs, err := filepath.Abs(scope.root)
		if err != nil {
			continue
		}
		rootAbs = filepath.Clean(rootAbs)
		prefix := rootAbs + sep
		if !strings.HasPrefix(absPath, prefix) {
			continue
		}
		rest := strings.TrimPrefix(absPath, prefix)
		parts := strings.SplitN(rest, sep, scope.nameLen+1)
		if len(parts) < scope.nameLen {
			continue
		}
		var bad bool
		for _, p := range parts[:scope.nameLen] {
			if p == "" || strings.HasPrefix(p, ".") {
				bad = true
			}
		}
		if bad {
			continue
		}
		name := strings.Join(parts[:scope.nameLen], "-")
		if scope.kind == "repos" && filepath.Join(rootAbs, name) == worktreesRoot {
			continue
		}
		out := "/file/" + scope.kind + "/" + url.PathEscape(name) + "/"
		if len(parts) > scope.nameLen && parts[scope.nameLen] != "" {
			var encoded []string
			for seg := range strings.SplitSeq(parts[scope.nameLen], sep) {
				if seg == "" {
					continue
				}
				encoded = append(encoded, url.PathEscape(seg))
			}
			out += strings.Join(encoded, "/")
		}
		if fragment != "" {
			out += "#" + fragment
		}
		return out
	}
	return ""
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
