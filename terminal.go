package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// The functions in this file choose between Rex and tmux. New sessions
// start in Rex (see rexSpawn). Sessions that started in tmux keep running
// there until they end, so each lookup tries Rex first and then tmux.
// A pane id tells the two apart: Rex pane ids start with "session:"
// (rexPaneID), tmux pane ids with "%".

// errNoSession is returned when neither Rex nor tmux has the session.
var errNoSession = errors.New("no such session")

// sessionExists reports whether the named session runs in Rex or tmux.
// tmux's `has-session` exits non-zero for both "session missing" and
// "tmux server not running"; both mean "doesn't exist" here.
func sessionExists(ctx context.Context, name string) bool {
	if _, ok := rexc.sessionByLabel(name); ok {
		return true
	}
	_, err := tmuxOut(ctx, "has-session", "-t", "="+name)
	return err == nil
}

// paneSession returns the session name that owns paneID and the agent id
// that runs in it ("" when it isn't one we recognise).
func paneSession(ctx context.Context, paneID string) (name, agentID string, err error) {
	if sessionID, blockID, ok := parseRexPaneID(paneID); ok {
		s, ok := rexc.sessionByID(sessionID)
		if !ok {
			return "", "", errNoSession
		}
		if blockID == s.Agent {
			agentID = agentFromLabel(s.AgentLabel)
		}
		return s.Label, agentID, nil
	}
	out, err := tmuxOut(ctx, "display-message", "-t", paneID, "-p", "#{session_name}\t#{pane_start_command}")
	if err != nil {
		return "", "", err
	}
	name, start, _ := strings.Cut(strings.TrimSpace(out), "\t")
	if name == "" {
		return "", "", errNoSession
	}
	return name, agentFromStartCommand(start), nil
}

// agentFromLabel returns the agent id stored in a Rex block label by
// createAgentSession, or "" when the label names no agent we know.
func agentFromLabel(label string) string {
	if label == agentName {
		return label
	}
	if _, ok := knownAgents[label]; ok {
		return label
	}
	return ""
}

// killSession ends the named session and its agent.
func killSession(ctx context.Context, name string) error {
	if s, ok := rexc.sessionByLabel(name); ok {
		return rexc.destroy(ctx, s.ID)
	}
	if _, err := tmuxOut(ctx, "kill-session", "-t", name); err != nil {
		return fmt.Errorf("tmux kill-session: %w", err)
	}
	return nil
}

// renameSession gives a running session a new name.
func renameSession(ctx context.Context, oldName, newName string) error {
	if s, ok := rexc.sessionByLabel(oldName); ok {
		return rexc.rename(ctx, s.ID, newName)
	}
	_, err := tmuxOut(ctx, "rename-session", "-t", "="+oldName, newName)
	return err
}

// sessionCreated returns when the named session started, in unix
// seconds. History ids use it.
func sessionCreated(ctx context.Context, name string) (int64, bool) {
	if s, ok := rexc.sessionByLabel(name); ok {
		return s.Created, s.Created != 0
	}
	out, err := tmuxOut(ctx, "display-message", "-t", name, "-p", "#{session_created}")
	if err != nil {
		return 0, false
	}
	created, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	return created, err == nil
}

// captureScrollback returns the whole scrollback of the session's agent
// pane, with ANSI styles.
func captureScrollback(ctx context.Context, name string) (string, error) {
	if s, ok := rexc.sessionByLabel(name); ok {
		out, err := rexc.formatScreen(ctx, s.ID, s.Agent, "vt")
		return normalizeVT(out), err
	}
	return tmuxOut(ctx, "capture-pane", "-p", "-e", "-S", "-", "-t", name+":0.0")
}

// capturePaneTail returns about the last lines rows of a pane, with ANSI
// styles.
func capturePaneTail(ctx context.Context, paneID string, lines int) (string, error) {
	if sessionID, blockID, ok := parseRexPaneID(paneID); ok {
		// Rex can only cut its output by bytes. Pi's styled rows run to
		// a few hundred bytes, so this covers about `lines` rows; the
		// result is then cut to exactly that.
		out, err := rexc.formatTail(ctx, sessionID, blockID, "vt", int64(lines)*256)
		if err != nil {
			return "", err
		}
		out = normalizeVT(out)
		if rows := strings.Split(out, "\n"); len(rows) > lines {
			out = strings.Join(rows[len(rows)-lines:], "\n")
		}
		return out, nil
	}
	return tmuxOut(ctx, "capture-pane", "-p", "-e", "-S", strconv.Itoa(-lines), "-t", paneID)
}

// normalizeVT turns Rex's vt rows, which end in CRLF, into the
// LF-separated text that tmux capture-pane gives.
func normalizeVT(s string) string {
	return strings.ReplaceAll(s, "\r\n", "\n")
}

// sendToPane delivers prompt as one bracketed paste followed by Enter.
// Bracketed paste tells the agent's TUI to treat newlines as text, so a
// multiline review arrives as one user turn.
func sendToPane(ctx context.Context, paneID, prompt string) error {
	if sessionID, blockID, ok := parseRexPaneID(paneID); ok {
		return rexc.paste(ctx, sessionID, blockID, prompt, true)
	}
	return sendToTmuxPane(ctx, paneID, prompt)
}

// resizePane sets the terminal of a Rex pane to size and makes webdiff
// its owner. It reports false for a tmux pane, which it doesn't change:
// a tmux window has one size for all clients, so a phone would also
// shrink the pane for an attached laptop.
func resizePane(ctx context.Context, paneID string, size rexTermSize) (bool, error) {
	sessionID, blockID, ok := parseRexPaneID(paneID)
	if !ok {
		return false, nil
	}
	return true, rexc.resize(ctx, sessionID, blockID, size)
}

// sendPaneKey presses one key, named as tmux names it (see
// allowedPaneKeys).
func sendPaneKey(ctx context.Context, paneID, key string) error {
	if sessionID, blockID, ok := parseRexPaneID(paneID); ok {
		k, ok := rexKeys[key]
		if !ok {
			return fmt.Errorf("unsupported key %q", key)
		}
		return rexc.key(ctx, sessionID, blockID, k)
	}
	_, err := tmuxOut(ctx, "send-keys", "-t", paneID, key)
	return err
}
