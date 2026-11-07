package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestTitleSaysWorking(t *testing.T) {
	for title, want := range map[string]bool{
		"◐ Claude Code": true,
		"◑ Fix the bug": true,
		"⠂ Claude Code": true,
		"⠐ Claude Code": true,
		"✳ Claude Code": false,
		"π - webdiff":   false,
		"":              false,
		"\u2800 blank":  false,
	} {
		if got := titleSaysWorking(title); got != want {
			t.Errorf("titleSaysWorking(%q) = %v, want %v", title, got, want)
		}
	}
	if !titleSaysIdle("✳ Claude Code") || titleSaysIdle("◐ Claude Code") {
		t.Error("titleSaysIdle is wrong")
	}
}

func TestRexIDTime(t *testing.T) {
	got := rexIDTime("session:01a1aa95-47f1-77a0-a4db-6267348a419b")
	want := time.Date(2026, 11, 5, 7, 21, 12, 0, time.UTC).Unix()
	if got != want {
		t.Errorf("rexIDTime = %d, want %d", got, want)
	}
	for _, id := range []string{"", "session:", "session:nothex", "session:01a1aa95-47f1-47a0-a4db-6267348a419b"} {
		if got := rexIDTime(id); got != 0 {
			t.Errorf("rexIDTime(%q) = %d, want 0", id, got)
		}
	}
}

func TestParseRexPaneID(t *testing.T) {
	id := rexPaneID("session:a", "block:b")
	s, b, ok := parseRexPaneID(id)
	if !ok || s != "session:a" || b != "block:b" {
		t.Errorf("parseRexPaneID(%q) = %q, %q, %v", id, s, b, ok)
	}
	for _, bad := range []string{"%12", "session:a", "block:b/session:a", "session:a/%1", ""} {
		if isRexPane(bad) {
			t.Errorf("isRexPane(%q) = true", bad)
		}
	}
}

// rexEvent builds a block_event for the agent block of session.
func rexEvent(session, block, name string, payload any) json.RawMessage {
	p, _ := json.Marshal(payload)
	ev, _ := json.Marshal(map[string]any{
		"type": "session_event",
		"payload": map[string]any{
			"session_id": session,
			"event": map[string]any{
				"type": "block_event",
				"payload": map[string]any{
					"block_id":     block,
					"creator_name": rexTerminalCreator,
					"name":         name,
					"payload":      json.RawMessage(p),
				},
			},
		},
	})
	return ev
}

// A Pi turn: working, then Rex drops the record at Pi's own OSC 133
// prompt mark, then done. The turn must last until done.
func TestRexTurnFromProgramStatus(t *testing.T) {
	c := newRexClient("")
	c.sessions["session:s"] = &rexSession{
		ID:    "session:s",
		Label: "x-wd",
		Agent: "block:b",
	}
	ended := make(chan rexSession, 4)
	c.onTurnEnd = func(s rexSession) { ended <- s }
	spinning := func() bool {
		s, _ := c.sessionByID("session:s")
		return s.spinning
	}
	status := func(state string) json.RawMessage {
		return rexEvent("session:s", "block:b", "program_status_changed", map[string]any{
			"id":     "",
			"record": map[string]string{"state": state},
		})
	}

	c.handleEvent(status("idle"))
	if spinning() {
		t.Fatal("idle agent is spinning")
	}
	if s, _ := c.sessionByID("session:s"); !s.ready() {
		t.Fatal("agent that reported idle isn't ready")
	}
	c.handleEvent(status("working"))
	if !spinning() {
		t.Fatal("working agent isn't spinning")
	}
	c.handleEvent(rexEvent("session:s", "block:b", "program_status_removed", map[string]any{
		"ids":    []string{""},
		"reason": "prompt",
	}))
	if !spinning() {
		t.Fatal("a prompt mark ended the turn")
	}
	// A title that isn't Claude's doesn't matter once 7501 is in use.
	c.handleEvent(rexEvent("session:s", "block:b", "title_changed", map[string]string{"title": "✳ x"}))
	if !spinning() {
		t.Fatal("title changed the state of a 7501 agent")
	}
	// Events for another block in the session are ignored.
	c.handleEvent(rexEvent("session:s", "block:other", "program_status_changed", map[string]any{
		"id":     "",
		"record": map[string]string{"state": "done"},
	}))
	if !spinning() {
		t.Fatal("another block's status ended the turn")
	}
	c.handleEvent(status("done"))
	if spinning() {
		t.Fatal("done agent is spinning")
	}
	select {
	case s := <-ended:
		if s.Label != "x-wd" {
			t.Errorf("turn end for %q", s.Label)
		}
	case <-time.After(time.Second):
		t.Fatal("no turn end")
	}
	select {
	case <-ended:
		t.Fatal("second turn end")
	case <-time.After(50 * time.Millisecond):
	}
}

// Claude Code: no 7501, a spinner in the title while it works.
func TestRexTurnFromTitle(t *testing.T) {
	c := newRexClient("")
	c.sessions["session:s"] = &rexSession{
		ID:    "session:s",
		Label: "x-wd",
		Agent: "block:b",
	}
	ended := make(chan rexSession, 4)
	c.onTurnEnd = func(s rexSession) { ended <- s }
	for _, title := range []string{"◐ Claude Code", "◑ Claude Code", "◐ Claude Code"} {
		c.handleEvent(rexEvent("session:s", "block:b", "title_changed", map[string]string{"title": title}))
		if s, _ := c.sessionByID("session:s"); !s.spinning || s.ready() {
			t.Fatalf("after %q: spinning=%v ready=%v", title, s.spinning, s.ready())
		}
	}
	c.handleEvent(rexEvent("session:s", "block:b", "title_changed", map[string]string{"title": "✳ Fix it"}))
	if s, _ := c.sessionByID("session:s"); s.spinning || !s.ready() {
		t.Fatalf("idle title: spinning=%v ready=%v", s.spinning, s.ready())
	}
	select {
	case <-ended:
	case <-time.After(time.Second):
		t.Fatal("no turn end")
	}
}

func TestRexSessionIndexEvents(t *testing.T) {
	c := newRexClient("")
	ev := func(typ string, payload any) json.RawMessage {
		b, _ := json.Marshal(map[string]any{"type": typ, "payload": payload})
		return b
	}
	c.sessions["session:01a1aa95-47f1-77a0-a4db-6267348a419b"] = &rexSession{
		ID:    "session:01a1aa95-47f1-77a0-a4db-6267348a419b",
		Label: "a-wd",
		Agent: "block:b",
	}
	if _, ok := c.sessionByLabel("a-wd"); !ok {
		t.Fatal("session not found by label")
	}
	c.handleEvent(ev("session_event", map[string]any{
		"session_id": "session:01a1aa95-47f1-77a0-a4db-6267348a419b",
		"event": map[string]any{
			"type":    "session_label_changed",
			"payload": map[string]string{"label": "b-wd"},
		},
	}))
	if _, ok := c.sessionByLabel("b-wd"); !ok {
		t.Fatal("rename not applied")
	}
	c.handleEvent(ev("session_destroyed", map[string]string{"session_id": "session:01a1aa95-47f1-77a0-a4db-6267348a419b"}))
	if _, ok := c.sessionByLabel("b-wd"); ok {
		t.Fatal("destroyed session still listed")
	}
}

// TestRexLive runs against the real Rex server on this host. Set
// WEBDIFF_REX_TEST=1 to run it.
func TestRexLive(t *testing.T) {
	if os.Getenv("WEBDIFF_REX_TEST") == "" {
		t.Skip("set WEBDIFF_REX_TEST=1 to test against the Rex server")
	}
	if _, err := exec.LookPath("rex"); err != nil {
		t.Skip("rex is not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c := newRexClient(defaultRexSocket())
	go c.run(ctx)
	if err := c.ensureConnected(ctx); err != nil {
		t.Fatal(err)
	}
	ended := make(chan rexSession, 4)
	c.onTurnEnd = func(s rexSession) { ended <- s }

	// A fake agent: it reports idle, reads one pasted line, reports
	// working, prints, and reports done. Then it waits for input.
	script := `
st() { printf '\033]7501;state=%s:app=fake\033\\' "$1"; }
st idle
IFS= read -r line
st working
i=0; while [ $i -lt 300 ]; do echo "row $i $line"; i=$((i+1)); done
sleep 0.5
st done
read -r rest
`
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/fake", []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	label := fmt.Sprintf("webdiff-test-%d-wd", time.Now().UnixNano())
	s, err := c.createAgentSession(ctx, label, dir, "fake", "fake", "sh ./fake")
	if err != nil {
		t.Fatal(err)
	}
	defer c.destroy(context.Background(), s.ID)
	if got, ok := c.sessionByLabel(label); !ok || got.Agent != s.Agent || got.AgentLabel != "fake" || got.Created == 0 {
		t.Fatalf("sessionByLabel = %+v, %v", got, ok)
	}

	changes, stop := c.watchBlock(s.ID, s.Agent)
	defer stop()
	if err := c.paste(ctx, s.ID, s.Agent, "hello", true); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-ended:
		if got.Label != label {
			t.Errorf("turn end for %q", got.Label)
		}
	case <-ctx.Done():
		t.Fatal("no turn end")
	}
	select {
	case <-changes:
	case <-time.After(5 * time.Second):
		t.Fatal("no change signal from the data stream")
	}

	tail, err := c.formatTail(ctx, s.ID, s.Agent, "vt", 200)
	if err != nil {
		t.Fatal(err)
	}
	tail = normalizeVT(tail)
	if !strings.Contains(tail, "row 299 hello") || strings.Contains(tail, "row 0 hello") || len(tail) > 200 {
		t.Errorf("tail = %q", tail)
	}
	all, err := c.formatScreen(ctx, s.ID, s.Agent, "text")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(all, "row 0 hello") || !strings.Contains(all, "row 299 hello") {
		t.Errorf("whole capture is missing rows: %q", all[:min(len(all), 200)])
	}

	if err := c.rename(ctx, s.ID, "renamed-"+label); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.sessionByLabel("renamed-" + label); !ok {
		t.Fatal("renamed session not found")
	}

	// The agent exits after one more line. Its session must go with it.
	if err := c.paste(ctx, s.ID, s.Agent, "bye", true); err != nil {
		t.Fatal(err)
	}
	wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
	defer wcancel()
	if err := c.waitFor(wctx, func() bool { return c.sessions[s.ID] == nil }); err != nil {
		t.Fatal("session didn't end with its agent")
	}
}
