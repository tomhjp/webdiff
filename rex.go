package main

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
)

// This file is the client for the Rex terminal server. webdiff starts new
// agent sessions in Rex and keeps the tmux code only for sessions that
// started before the change.
//
// The server documents its protocol at GET /llms.txt on its socket. The
// control connection is JSON over a WebSocket: a hello, then requests
// and responses matched by id, and events with a gapless seq. A data
// connection streams one terminal's raw output. webdiff uses that stream
// only to know when a terminal changed.
//
// Rex state that webdiff needs (which session holds which agent, and
// what the agent says it is doing) comes from events, not from polling:
//
//   - Agents that send OSC 7501 (Pi) report working/done/idle/blocked.
//   - Claude Code sends no 7501, but it animates its terminal title while
//     it works and shows "✳" when it waits for input.

// rexAPIRevision is the control API revision this client speaks.
const rexAPIRevision = 10

const (
	rexTerminalCreator     = "com.superlogical.terminal"
	rexTerminalFlavor      = "com.superlogical.terminal.shell"
	rexTerminalSubprotocol = "rex.data.terminal.v1"
	// rexMaxBlobBytes is the largest part that format and blob_read
	// return in one reply.
	rexMaxBlobBytes = 983040
)

// rexc is the process-wide Rex client. It is nil when Rex isn't
// installed, and then every Rex lookup reports "not found".
var rexc *rexClient

// rexSpawn reports whether new agent sessions start in Rex. It is false
// when the -terminal flag selects tmux or Rex isn't installed.
var rexSpawn bool

// rexError is an error response from the server.
type rexError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Status  int    `json:"status"`
}

func (e *rexError) Error() string {
	return fmt.Sprintf("rex: %s (%s)", e.Message, e.Code)
}

// errRexDisconnected is returned by calls made while there is no control
// connection.
var errRexDisconnected = errors.New("rex: not connected")

type rexResponse struct {
	payload json.RawMessage
	err     error
}

// rexConn is one control connection.
type rexConn struct {
	ws      *websocket.Conn
	nextID  atomic.Uint64
	mu      sync.Mutex
	pending map[string]chan rexResponse
	closed  bool
	done    chan struct{}
}

func (c *rexConn) call(ctx context.Context, method string, payload, out any) error {
	id := fmt.Sprint(c.nextID.Add(1))
	ch := make(chan rexResponse, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return errRexDisconnected
	}
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()
	if payload == nil {
		payload = struct{}{}
	}
	msg, err := json.Marshal(struct {
		Type    string `json:"type"`
		ID      string `json:"id"`
		Method  string `json:"method"`
		Payload any    `json:"payload"`
	}{"request", id, method, payload})
	if err != nil {
		return err
	}
	if err := c.ws.Write(ctx, websocket.MessageText, msg); err != nil {
		return fmt.Errorf("rex %s: %w", method, err)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return errRexDisconnected
	case r := <-ch:
		if r.err != nil {
			return fmt.Errorf("%s: %w", method, r.err)
		}
		if out != nil {
			return json.Unmarshal(r.payload, out)
		}
		return nil
	}
}

// readLoop delivers responses to their callers and events to onEvent.
// onEvent runs on this goroutine, so it must not make calls on c.
func (c *rexConn) readLoop(ctx context.Context, onEvent func(json.RawMessage)) {
	defer func() {
		c.mu.Lock()
		c.closed = true
		c.mu.Unlock()
		close(c.done)
	}()
	for {
		_, data, err := c.ws.Read(ctx)
		if err != nil {
			return
		}
		var m struct {
			Type    string          `json:"type"`
			ID      string          `json:"id"`
			Payload json.RawMessage `json:"payload"`
			Error   *rexError       `json:"error"`
		}
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		switch m.Type {
		case "response":
			c.mu.Lock()
			ch := c.pending[m.ID]
			c.mu.Unlock()
			if ch == nil {
				continue
			}
			if m.Error != nil {
				ch <- rexResponse{err: m.Error}
			} else {
				ch <- rexResponse{payload: m.Payload}
			}
		case "event":
			onEvent(m.Payload)
		}
	}
}

// rexSession is what the client knows about one Rex session. Only the
// agent block is tracked: webdiff creates each session with one block.
type rexSession struct {
	ID      string
	Label   string
	Created int64 // unix seconds, from the UUIDv7 session id

	Agent      string // block id, "" until known
	AgentLabel string // block label; webdiff stores the agent id there

	// status is the state of the agent's root OSC 7501 record, "" when
	// there is none. hasStatus stays set once the agent has reported,
	// so that its title is no longer used as the signal.
	status    string
	hasStatus bool
	title     string
	spinning  bool
	// lastActive is when the agent last changed state, for the listing.
	lastActive time.Time
}

func (s *rexSession) working() bool {
	if s.hasStatus {
		return s.status == "working"
	}
	return titleSaysWorking(s.title)
}

// ready reports whether the agent can take a paste.
func (s *rexSession) ready() bool {
	return s.hasStatus || titleSaysIdle(s.title)
}

// titleSaysWorking reports whether a terminal title shows Claude Code's
// working spinner: half-circle glyphs in current versions, braille dots
// in older ones.
func titleSaysWorking(title string) bool {
	r, _ := utf8.DecodeRuneInString(title)
	switch {
	case r >= 0x25D0 && r <= 0x25D3: // ◐ ◑ ◒ ◓
		return true
	case r > 0x2800 && r <= 0x28FF: // braille patterns, not blank
		return true
	}
	return false
}

// titleSaysIdle reports whether a terminal title shows Claude Code's
// idle marker. Claude sets it once its prompt can take input.
func titleSaysIdle(title string) bool {
	return strings.HasPrefix(title, "✳")
}

// rexClient keeps one control connection open, reconnects when it drops,
// and keeps an index of the server's sessions up to date from events.
type rexClient struct {
	socket string
	http   *http.Client

	mu       sync.Mutex
	conn     *rexConn
	sessions map[string]*rexSession // by session id
	// changed is closed and replaced on every state change, so waiters
	// can block on it.
	changed  chan struct{}
	watchers map[string]*rexWatcher // by block id

	// onTurnEnd is called, on its own goroutine, when a session's agent
	// stops working.
	onTurnEnd func(s rexSession)
}

func newRexClient(socket string) *rexClient {
	return &rexClient{
		socket: socket,
		http: &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socket)
			},
		}},
		sessions: map[string]*rexSession{},
		changed:  make(chan struct{}),
		watchers: map[string]*rexWatcher{},
	}
}

// defaultRexSocket returns the socket of the personal Rex server.
func defaultRexSocket() string {
	if s := os.Getenv("REX_SERVER"); strings.HasPrefix(s, "unix://") {
		return strings.TrimPrefix(s, "unix://")
	}
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = fmt.Sprintf("/run/user/%d", os.Getuid())
	}
	return filepath.Join(dir, "rex", "server.sock")
}

// notifyLocked wakes every waiter. Callers hold c.mu.
func (c *rexClient) notifyLocked() {
	close(c.changed)
	c.changed = make(chan struct{})
}

func (c *rexClient) current() *rexConn {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn
}

func (c *rexClient) call(ctx context.Context, method string, payload, out any) error {
	conn := c.current()
	if conn == nil {
		return errRexDisconnected
	}
	return conn.call(ctx, method, payload, out)
}

// blockCall calls a terminal block method.
func (c *rexClient) blockCall(ctx context.Context, sessionID, blockID, method string, args, out any) error {
	if args == nil {
		args = struct{}{}
	}
	return c.call(ctx, rexTerminalCreator+"."+method, map[string]any{
		"session_id": sessionID,
		"block_id":   blockID,
		"args":       args,
	}, out)
}

// run keeps the control connection open until ctx ends.
func (c *rexClient) run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		conn, err := c.dial(ctx)
		if err != nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		backoff = time.Second
		go conn.readLoop(ctx, c.handleEvent)
		if err := c.start(ctx, conn); err != nil {
			conn.ws.Close(websocket.StatusNormalClosure, "")
		}
		<-conn.done
		c.mu.Lock()
		if c.conn == conn {
			c.conn = nil
			// The index is only good while events arrive. A dropped
			// connection may mean the server restarted, which ends every
			// session anyway.
			c.sessions = map[string]*rexSession{}
			c.notifyLocked()
		}
		c.mu.Unlock()
	}
}

func (c *rexClient) dial(ctx context.Context) (*rexConn, error) {
	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(dctx, "ws://rex/api/control", &websocket.DialOptions{
		HTTPClient:   c.http,
		Subprotocols: []string{"rex.control.v1"},
	})
	if err != nil {
		return nil, err
	}
	ws.SetReadLimit(4 << 20)
	hello, _ := json.Marshal(map[string]any{
		"type":         "hello",
		"api_revision": rexAPIRevision,
		"client": map[string]string{
			"kind": "other",
			"name": "webdiff",
		},
	})
	if err := ws.Write(dctx, websocket.MessageText, hello); err != nil {
		ws.CloseNow()
		return nil, err
	}
	_, data, err := ws.Read(dctx)
	if err != nil {
		ws.CloseNow()
		return nil, err
	}
	var welcome struct {
		Type        string `json:"type"`
		Protocol    string `json:"protocol"`
		APIRevision int    `json:"api_revision"`
	}
	if json.Unmarshal(data, &welcome) != nil || welcome.Type != "welcome" || welcome.Protocol != "rex.control.v1" {
		ws.CloseNow()
		return nil, fmt.Errorf("rex: unexpected welcome %q", data)
	}
	if welcome.APIRevision != rexAPIRevision {
		ws.CloseNow()
		return nil, fmt.Errorf("rex: server uses API revision %d, want %d", welcome.APIRevision, rexAPIRevision)
	}
	return &rexConn{
		ws:      ws,
		pending: map[string]chan rexResponse{},
		done:    make(chan struct{}),
	}, nil
}

// start subscribes to every event on the server, then builds the session
// index. Events that arrive during the sync are applied too. Each update
// is a whole value (a state, a title), so applying one twice does no harm.
func (c *rexClient) start(ctx context.Context, conn *rexConn) error {
	var res struct {
		Sessions []struct {
			SessionID string `json:"session_id"`
			Label     string `json:"label"`
		} `json:"sessions"`
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := conn.call(cctx, "session.attach_all", map[string]string{"event_scope": "all"}, &res); err != nil {
		return err
	}
	c.mu.Lock()
	c.conn = conn
	c.sessions = map[string]*rexSession{}
	for _, s := range res.Sessions {
		c.sessions[s.SessionID] = &rexSession{
			ID:      s.SessionID,
			Label:   s.Label,
			Created: rexIDTime(s.SessionID),
		}
	}
	c.notifyLocked()
	c.mu.Unlock()
	for _, s := range res.Sessions {
		c.syncSession(cctx, s.SessionID)
	}
	return nil
}

// syncSession reads a session's agent block and its status and title.
func (c *rexClient) syncSession(ctx context.Context, sessionID string) {
	var blocks struct {
		Blocks []struct {
			BlockID     string `json:"block_id"`
			Label       string `json:"label"`
			CreatorName string `json:"creator_name"`
			Placement   string `json:"placement"`
		} `json:"blocks"`
	}
	if err := c.call(ctx, "session.list_blocks", map[string]string{"session_id": sessionID}, &blocks); err != nil {
		return
	}
	var agent, agentLabel string
	for _, b := range blocks.Blocks {
		if b.CreatorName == rexTerminalCreator && b.Placement == "placed" {
			agent, agentLabel = b.BlockID, b.Label
			break
		}
	}
	if agent == "" {
		c.mu.Lock()
		s := c.sessions[sessionID]
		if s != nil {
			s.Agent, s.AgentLabel = "", ""
			c.notifyLocked()
		}
		c.mu.Unlock()
		// A webdiff session whose agent exited is left with no blocks.
		// tmux ends such a session, so do the same.
		if s != nil && len(blocks.Blocks) == 0 && strings.HasSuffix(s.Label, "-wd") {
			_ = c.call(ctx, "session.destroy", map[string]string{"session_id": sessionID}, nil)
		}
		return
	}
	var ps struct {
		Records []struct {
			ID    string `json:"id"`
			State string `json:"state"`
		} `json:"records"`
	}
	var title struct {
		Title string `json:"title"`
	}
	psErr := c.blockCall(ctx, sessionID, agent, "program_status", nil, &ps)
	_ = c.blockCall(ctx, sessionID, agent, "title", nil, &title)

	c.mu.Lock()
	s := c.sessions[sessionID]
	if s == nil {
		c.mu.Unlock()
		return
	}
	if s.Agent != agent {
		s.status, s.hasStatus, s.spinning = "", false, false
	}
	s.Agent, s.AgentLabel = agent, agentLabel
	if psErr == nil {
		for _, r := range ps.Records {
			if r.ID == "" {
				s.status, s.hasStatus = r.State, true
			}
		}
	}
	s.title = title.Title
	c.updateLocked(s)
	c.mu.Unlock()
}

// updateLocked recomputes s.spinning after a change and reports a turn
// end. Callers hold c.mu.
func (c *rexClient) updateLocked(s *rexSession) {
	now := s.working()
	if now != s.spinning {
		s.lastActive = time.Now()
		if s.spinning && !now && c.onTurnEnd != nil {
			go c.onTurnEnd(*s)
		}
	}
	s.spinning = now
	c.notifyLocked()
}

// handleEvent applies one event to the index. It runs on the read loop,
// so anything that calls the server runs on a new goroutine.
func (c *rexClient) handleEvent(raw json.RawMessage) {
	var ev struct {
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}
	if json.Unmarshal(raw, &ev) != nil {
		return
	}
	switch ev.Type {
	case "session_created":
		var p struct {
			Session struct {
				SessionID string `json:"session_id"`
				Label     string `json:"label"`
			} `json:"session"`
		}
		if json.Unmarshal(ev.Payload, &p) != nil {
			return
		}
		c.mu.Lock()
		if c.sessions[p.Session.SessionID] == nil {
			c.sessions[p.Session.SessionID] = &rexSession{
				ID:      p.Session.SessionID,
				Label:   p.Session.Label,
				Created: rexIDTime(p.Session.SessionID),
			}
			c.notifyLocked()
		}
		c.mu.Unlock()
		go c.syncSession(context.Background(), p.Session.SessionID)
	case "session_destroyed":
		var p struct {
			SessionID string `json:"session_id"`
		}
		if json.Unmarshal(ev.Payload, &p) != nil {
			return
		}
		c.mu.Lock()
		delete(c.sessions, p.SessionID)
		c.notifyLocked()
		c.mu.Unlock()
	case "session_event":
		var p struct {
			SessionID string `json:"session_id"`
			Event     struct {
				Type    string          `json:"type"`
				Payload json.RawMessage `json:"payload"`
			} `json:"event"`
		}
		if json.Unmarshal(ev.Payload, &p) != nil {
			return
		}
		c.handleSessionEvent(p.SessionID, p.Event.Type, p.Event.Payload)
	}
}

func (c *rexClient) handleSessionEvent(sessionID, typ string, payload json.RawMessage) {
	switch typ {
	case "session_label_changed":
		var p struct {
			Label string `json:"label"`
		}
		if json.Unmarshal(payload, &p) != nil {
			return
		}
		c.mu.Lock()
		if s := c.sessions[sessionID]; s != nil {
			s.Label = p.Label
			c.notifyLocked()
		}
		c.mu.Unlock()
	case "block_created", "block_closed":
		go c.syncSession(context.Background(), sessionID)
	case "block_event":
		var p struct {
			BlockID     string          `json:"block_id"`
			CreatorName string          `json:"creator_name"`
			Name        string          `json:"name"`
			Payload     json.RawMessage `json:"payload"`
		}
		if json.Unmarshal(payload, &p) != nil || p.CreatorName != rexTerminalCreator {
			return
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		s := c.sessions[sessionID]
		if s == nil || s.Agent != p.BlockID {
			return
		}
		applyTerminalEvent(s, p.Name, p.Payload)
		c.updateLocked(s)
	}
}

// applyTerminalEvent applies one terminal event to the agent's state.
func applyTerminalEvent(s *rexSession, name string, payload json.RawMessage) {
	switch name {
	case "program_status_changed":
		var p struct {
			ID     string `json:"id"`
			Record struct {
				State string `json:"state"`
			} `json:"record"`
		}
		if json.Unmarshal(payload, &p) == nil && p.ID == "" {
			s.status, s.hasStatus = p.Record.State, true
		}
	case "program_status_removed":
		var p struct {
			IDs    []string `json:"ids"`
			Reason string   `json:"reason"`
		}
		if json.Unmarshal(payload, &p) != nil {
			return
		}
		// Pi starts each rendered message with OSC 133;A. The status
		// protocol says a terminal drops "working" at a prompt mark, so
		// Rex removes the record a few ms after Pi reports it. The turn
		// isn't over: keep the last reported state until the next report.
		if p.Reason == "prompt" {
			return
		}
		for _, id := range p.IDs {
			if id == "" {
				s.status = ""
			}
		}
	case "title_changed":
		var p struct {
			Title string `json:"title"`
		}
		if json.Unmarshal(payload, &p) == nil {
			s.title = p.Title
		}
	case "child_exited":
		s.status = ""
		s.title = ""
	}
}

// rexIDTime returns the creation time, in unix seconds, encoded in a
// UUIDv7 id such as "session:01a1aa95-47f1-77a0-…". It returns 0 for an
// id that isn't a UUIDv7.
func rexIDTime(id string) int64 {
	_, u, ok := strings.Cut(id, ":")
	if !ok {
		return 0
	}
	h := strings.ReplaceAll(u, "-", "")
	if len(h) != 32 || h[12] != '7' {
		return 0
	}
	b, err := hex.DecodeString(h[:12])
	if err != nil {
		return 0
	}
	ms := int64(binary.BigEndian.Uint64(append([]byte{0, 0}, b...)))
	return ms / 1000
}

// rexPaneID joins a session and block id into the opaque pane id the
// browser passes back. Rex block calls need both, and the "session:"
// prefix keeps it apart from tmux's "%N" pane ids.
func rexPaneID(sessionID, blockID string) string {
	return sessionID + "/" + blockID
}

// parseRexPaneID splits a pane id made by rexPaneID.
func parseRexPaneID(id string) (sessionID, blockID string, ok bool) {
	sessionID, blockID, ok = strings.Cut(id, "/")
	if !ok || !strings.HasPrefix(sessionID, "session:") || !strings.HasPrefix(blockID, "block:") {
		return "", "", false
	}
	return sessionID, blockID, true
}

// isRexPane reports whether a pane id names a Rex block.
func isRexPane(id string) bool {
	_, _, ok := parseRexPaneID(id)
	return ok
}

// sessionByLabel returns a copy of the session with the given label
// whose agent block is known.
func (c *rexClient) sessionByLabel(label string) (rexSession, bool) {
	if c == nil {
		return rexSession{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range c.sessions {
		if s.Label == label && s.Agent != "" {
			return *s, true
		}
	}
	return rexSession{}, false
}

// sessionByID returns a copy of the session with the given id.
func (c *rexClient) sessionByID(id string) (rexSession, bool) {
	if c == nil {
		return rexSession{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.sessions[id]
	if !ok {
		return rexSession{}, false
	}
	return *s, true
}

// snapshot returns a copy of every known session.
func (c *rexClient) snapshot() []rexSession {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]rexSession, 0, len(c.sessions))
	for _, s := range c.sessions {
		out = append(out, *s)
	}
	return out
}

// waitFor blocks until cond, run under c.mu, holds or ctx ends.
func (c *rexClient) waitFor(ctx context.Context, cond func() bool) error {
	for {
		c.mu.Lock()
		ok := cond()
		ch := c.changed
		c.mu.Unlock()
		if ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ch:
		}
	}
}

// ensureConnected starts the personal server if it isn't running, and
// waits for the control connection.
func (c *rexClient) ensureConnected(ctx context.Context) error {
	if c.current() != nil {
		return nil
	}
	if _, err := os.Stat(c.socket); err != nil {
		// Without the rex.socket unit nothing listens until a server
		// starts. `rex server start` is idempotent.
		if out, err := exec.CommandContext(ctx, "rex", "server", "start").CombinedOutput(); err != nil {
			return fmt.Errorf("rex server start: %v: %s", err, strings.TrimSpace(string(out)))
		}
	}
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := c.waitFor(wctx, func() bool { return c.conn != nil }); err != nil {
		return fmt.Errorf("rex: no connection to %s", c.socket)
	}
	return nil
}

// createAgentSession starts cmd in a new session named label, in dir.
// The agent id goes in the block label, so that a later webdiff can tell
// which agent runs there. It waits until the agent can take a paste.
func (c *rexClient) createAgentSession(ctx context.Context, label, dir, agentID, agentName, cmd string) (rexSession, error) {
	if err := c.ensureConnected(ctx); err != nil {
		return rexSession{}, err
	}
	var res struct {
		SessionID      string `json:"session_id"`
		InitialWindows []struct {
			BlockIDs []string `json:"block_ids"`
		} `json:"initial_windows"`
	}
	// `bash -lc <cmd>`, as for tmux: the agent binary is usually in
	// ~/.local/bin, which the login profile puts on PATH, and an -agent
	// value can carry flags. Close the terminal on every exit, including
	// a quick failure, so the session ends with its agent as in tmux.
	err := c.call(ctx, "session.create", map[string]any{
		"label": label,
		"initial_windows": []any{map[string]any{
			"window_label": "agent",
			"layout": map[string]any{"block": map[string]any{
				"flavor": rexTerminalFlavor,
				"label":  agentID,
				"options": map[string]any{
					"shell":   "none",
					"command": []string{"bash", "-lc", cmd},
					"cwd":     dir,
					"exit": map[string]any{
						"on_completion":           true,
						"quick_exit_threshold_ms": 0,
					},
				},
			}},
		}},
	}, &res)
	if err != nil {
		return rexSession{}, err
	}
	if len(res.InitialWindows) == 0 || len(res.InitialWindows[0].BlockIDs) == 0 {
		return rexSession{}, errors.New("rex: session.create returned no block")
	}
	block := res.InitialWindows[0].BlockIDs[0]
	c.mu.Lock()
	s := c.sessions[res.SessionID]
	if s == nil {
		s = &rexSession{
			ID:      res.SessionID,
			Label:   label,
			Created: rexIDTime(res.SessionID),
		}
		c.sessions[res.SessionID] = s
	}
	s.Agent, s.AgentLabel = block, agentID
	c.notifyLocked()
	c.mu.Unlock()

	if err := c.waitReady(ctx, res.SessionID, block, agentName); err != nil {
		return rexSession{}, err
	}
	got, _ := c.sessionByID(res.SessionID)
	return got, nil
}

// waitReady waits until the agent in block can take a paste. Pi reports
// "idle" with OSC 7501 and Claude Code sets its "✳" title, both within a
// second. An agent that does neither gets the old tmux rule after 3 s:
// its process runs and its screen hasn't changed for 300 ms.
func (c *rexClient) waitReady(ctx context.Context, sessionID, block, agentName string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	signalled := make(chan error, 1)
	go func() {
		signalled <- c.waitFor(ctx, func() bool {
			s := c.sessions[sessionID]
			return s == nil || s.ready()
		})
	}()
	select {
	case err := <-signalled:
		if err != nil {
			return fmt.Errorf("%s didn't start within 15 s", agentName)
		}
		if _, ok := c.sessionByID(sessionID); !ok {
			return fmt.Errorf("%s exited at start", agentName)
		}
		return nil
	case <-time.After(3 * time.Second):
	}
	var last string
	var stableAt time.Time
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s didn't start within 15 s", agentName)
		case err := <-signalled:
			if err == nil {
				return nil
			}
		case <-time.After(100 * time.Millisecond):
		}
		var proc struct {
			Foreground *struct {
				Name  string `json:"name"`
				Argv0 string `json:"argv0"`
			} `json:"foreground"`
		}
		if c.blockCall(ctx, sessionID, block, "process", nil, &proc) != nil || proc.Foreground == nil {
			continue
		}
		if filepath.Base(proc.Foreground.Argv0) != agentName && proc.Foreground.Name != agentName {
			continue
		}
		screen, err := c.formatScreen(ctx, sessionID, block, "text")
		if err != nil {
			continue
		}
		if screen != last {
			last, stableAt = screen, time.Now()
		} else if time.Since(stableAt) >= 300*time.Millisecond {
			return nil
		}
	}
}

// paste sends text as one bracketed paste, then presses Enter. Both go
// on the one control connection, and the server delivers block.event
// calls in the order they arrive, so the Enter can't overtake the paste.
func (c *rexClient) paste(ctx context.Context, sessionID, blockID, text string, enter bool) error {
	if err := c.call(ctx, "block.event", map[string]any{
		"session_id": sessionID,
		"block_id":   blockID,
		"event": map[string]any{
			"type":    "paste",
			"payload": map[string]string{"text": text},
		},
	}, nil); err != nil {
		return err
	}
	if !enter {
		return nil
	}
	return c.key(ctx, sessionID, blockID, "enter")
}

// rexKeys maps the tmux key names the stream toolbar sends to Rex's
// physical key names.
var rexKeys = map[string]string{
	"Left":   "arrow_left",
	"Up":     "arrow_up",
	"Down":   "arrow_down",
	"Right":  "arrow_right",
	"Escape": "escape",
	"Enter":  "enter",
}

// key presses one key with no modifiers. Rex encodes it for the
// keyboard mode the program has turned on.
func (c *rexClient) key(ctx context.Context, sessionID, blockID, key string) error {
	return c.call(ctx, "block.event", map[string]any{
		"session_id": sessionID,
		"block_id":   blockID,
		"event": map[string]any{
			"type": "key",
			"payload": map[string]any{
				"key":                 key,
				"mods":                "",
				"consumed_mods":       "",
				"text":                "",
				"unshifted_codepoint": 0,
				"composing":           false,
				"action":              "press",
			},
		},
	}, nil)
}

// rexTermSize is a terminal size in cells. CellWidthPx and CellHeightPx
// are the size of one cell in device pixels, or 0 if they are not known.
type rexTermSize struct {
	Columns      int `json:"columns"`
	Rows         int `json:"rows"`
	CellWidthPx  int `json:"cell_width_px,omitempty"`
	CellHeightPx int `json:"cell_height_px,omitempty"`
}

// resize makes webdiff the owner of the block and sets the terminal to
// size. A terminal follows the size of its owner, so the claim must come
// first. Another client, such as Rex.app, can take the block back. This
// is the same thing that the Rex iOS app does when it shows a pane.
func (c *rexClient) resize(ctx context.Context, sessionID, blockID string, size rexTermSize) error {
	// session.own_block needs a connection attached to the session.
	if err := c.call(ctx, "session.attach", map[string]string{"session_id": sessionID}, nil); err != nil {
		return err
	}
	if err := c.call(ctx, "session.own_block", map[string]any{
		"session_id": sessionID,
		"block_ids":  []string{blockID},
		"mode":       "take",
	}, nil); err != nil {
		return err
	}
	args := map[string]int{
		"columns": size.Columns,
		"rows":    size.Rows,
	}
	// Rex wants both cell dimensions or neither.
	if size.CellWidthPx > 0 && size.CellHeightPx > 0 {
		args["cell_width_px"] = size.CellWidthPx
		args["cell_height_px"] = size.CellHeightPx
	}
	return c.blockCall(ctx, sessionID, blockID, "resize", args, nil)
}

// termSize returns the current size of the block's terminal.
func (c *rexClient) termSize(ctx context.Context, sessionID, blockID string) (rexTermSize, error) {
	var out rexTermSize
	err := c.blockCall(ctx, sessionID, blockID, "size", nil, &out)
	return out, err
}

// destroy ends a session and the agent in it.
func (c *rexClient) destroy(ctx context.Context, sessionID string) error {
	if err := c.call(ctx, "session.destroy", map[string]string{"session_id": sessionID}, nil); err != nil {
		return err
	}
	c.mu.Lock()
	delete(c.sessions, sessionID)
	c.notifyLocked()
	c.mu.Unlock()
	return nil
}

// rename changes a session's label.
func (c *rexClient) rename(ctx context.Context, sessionID, label string) error {
	if err := c.call(ctx, "session.set_label", map[string]string{
		"session_id": sessionID,
		"label":      label,
	}, nil); err != nil {
		return err
	}
	c.mu.Lock()
	if s := c.sessions[sessionID]; s != nil {
		s.Label = label
		c.notifyLocked()
	}
	c.mu.Unlock()
	return nil
}

type rexFormatResult struct {
	Content string `json:"content"`
	ID      *int64 `json:"id"`
	Total   int64  `json:"total"`
}

type rexBlobResult struct {
	Data  string `json:"data"`
	Total int64  `json:"total"`
	EOF   bool   `json:"eof"`
}

// formatScreen returns the whole screen and scrollback, read in parts if
// it is larger than one reply.
func (c *rexClient) formatScreen(ctx context.Context, sessionID, blockID, format string) (string, error) {
	return c.formatTail(ctx, sessionID, blockID, format, 0)
}

// formatTail returns the last tailBytes of the terminal's screen and
// scrollback, or all of it when tailBytes is 0. The format method always
// renders the whole scrollback, but only the requested part is sent.
// With a tail, the first line is dropped, because it can be cut.
func (c *rexClient) formatTail(ctx context.Context, sessionID, blockID, format string, tailBytes int64) (string, error) {
	var res rexFormatResult
	if err := c.blockCall(ctx, sessionID, blockID, "format", map[string]any{
		"format":    format,
		"trim":      true,
		"max_bytes": rexMaxBlobBytes,
	}, &res); err != nil {
		return "", err
	}
	if res.ID == nil {
		if tailBytes > 0 && int64(len(res.Content)) > tailBytes {
			return dropFirstLine(res.Content[len(res.Content)-int(tailBytes):]), nil
		}
		return res.Content, nil
	}
	var b strings.Builder
	offset := int64(len(res.Content))
	cut := false
	if tailBytes > 0 && res.Total-tailBytes > offset {
		offset = res.Total - tailBytes
		cut = true
	} else {
		b.WriteString(res.Content)
	}
	for {
		var part rexBlobResult
		if err := c.blockCall(ctx, sessionID, blockID, "blob_read", map[string]any{
			"id":        *res.ID,
			"offset":    offset,
			"max_bytes": rexMaxBlobBytes,
		}, &part); err != nil {
			return "", err
		}
		b.WriteString(part.Data)
		offset += int64(len(part.Data))
		if part.EOF || part.Data == "" {
			break
		}
	}
	if cut {
		return dropFirstLine(b.String()), nil
	}
	return b.String(), nil
}

func dropFirstLine(s string) string {
	if _, rest, ok := strings.Cut(s, "\n"); ok {
		return rest
	}
	return s
}

// rexBlockInfo describes one block for the home page's session list.
type rexBlockInfo struct {
	Session  rexSession
	BlockID  string
	Index    string // "<window>.<block>", zero-based, as tmux shows it
	Command  string
	Title    string
	LastUsed int64
}

// listBlocks lists every terminal block in every session.
func (c *rexClient) listBlocks(ctx context.Context) []rexBlockInfo {
	if c == nil || c.current() == nil {
		return nil
	}
	sessions := c.snapshot()
	results := make([][]rexBlockInfo, len(sessions))
	var wg sync.WaitGroup
	for i, s := range sessions {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = c.listSessionBlocks(ctx, s)
		}()
	}
	wg.Wait()
	var out []rexBlockInfo
	for _, r := range results {
		out = append(out, r...)
	}
	return out
}

func (c *rexClient) listSessionBlocks(ctx context.Context, s rexSession) []rexBlockInfo {
	var blocks struct {
		Blocks []struct {
			BlockID     string `json:"block_id"`
			CreatorName string `json:"creator_name"`
			WindowID    string `json:"window_id"`
		} `json:"blocks"`
	}
	if c.call(ctx, "session.list_blocks", map[string]string{"session_id": s.ID}, &blocks) != nil {
		return nil
	}
	var out []rexBlockInfo
	windows := map[string]int{}
	perWindow := map[string]int{}
	for _, b := range blocks.Blocks {
		if b.CreatorName != rexTerminalCreator || b.WindowID == "" {
			continue
		}
		w, ok := windows[b.WindowID]
		if !ok {
			w = len(windows)
			windows[b.WindowID] = w
		}
		info := rexBlockInfo{
			Session:  s,
			BlockID:  b.BlockID,
			Index:    fmt.Sprintf("%d.%d", w, perWindow[b.WindowID]),
			LastUsed: s.Created,
		}
		perWindow[b.WindowID]++
		if b.BlockID == s.Agent && !s.lastActive.IsZero() {
			info.LastUsed = s.lastActive.Unix()
		}
		var proc struct {
			Foreground *struct {
				Name  string `json:"name"`
				Argv0 string `json:"argv0"`
			} `json:"foreground"`
		}
		if c.blockCall(ctx, s.ID, b.BlockID, "process", nil, &proc) == nil && proc.Foreground != nil {
			info.Command = proc.Foreground.Name
			if a := filepath.Base(strings.TrimPrefix(proc.Foreground.Argv0, "-")); a != "" && a != "." {
				info.Command = a
			}
		}
		var title struct {
			Title string `json:"title"`
		}
		if c.blockCall(ctx, s.ID, b.BlockID, "title", nil, &title) == nil {
			info.Title = title.Title
		}
		out = append(out, info)
	}
	return out
}

// rexWatcher holds one data connection to a block and tells every
// subscriber when the terminal produces output.
type rexWatcher struct {
	subs   map[chan struct{}]struct{}
	cancel context.CancelFunc
}

// watchBlock returns a channel that receives a value soon after the
// block produces output, and a function to stop watching. Values are
// coalesced: a slow reader sees one value for many changes. All viewers
// of a block share one data connection, because the server allows one
// per client.
func (c *rexClient) watchBlock(sessionID, blockID string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	c.mu.Lock()
	w := c.watchers[blockID]
	if w == nil {
		ctx, cancel := context.WithCancel(context.Background())
		w = &rexWatcher{
			subs:   map[chan struct{}]struct{}{},
			cancel: cancel,
		}
		c.watchers[blockID] = w
		go c.runWatcher(ctx, w, sessionID, blockID)
	}
	w.subs[ch] = struct{}{}
	c.mu.Unlock()
	return ch, func() {
		c.mu.Lock()
		delete(w.subs, ch)
		if len(w.subs) == 0 {
			w.cancel()
			if c.watchers[blockID] == w {
				delete(c.watchers, blockID)
			}
		}
		c.mu.Unlock()
	}
}

func (c *rexClient) notifyWatcher(w *rexWatcher) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for ch := range w.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// runWatcher keeps a data connection open until ctx ends. When the
// connection fails it opens a new one, and tells subscribers to read the
// screen again, because output may have arrived in between.
func (c *rexClient) runWatcher(ctx context.Context, w *rexWatcher, sessionID, blockID string) {
	for ctx.Err() == nil {
		err := c.streamBlock(ctx, sessionID, blockID, func() { c.notifyWatcher(w) })
		if ctx.Err() != nil {
			return
		}
		c.notifyWatcher(w)
		var rerr *rexError
		if errors.As(err, &rerr) && (rerr.Code == "not_found" || rerr.Code == "session_not_found") {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

// streamBlock opens one data connection to a block's terminal and calls
// onData for each message until the connection or ctx ends.
func (c *rexClient) streamBlock(ctx context.Context, sessionID, blockID string, onData func()) error {
	// data.connect needs a connection attached to the session.
	if err := c.call(ctx, "session.attach", map[string]string{"session_id": sessionID}, nil); err != nil {
		return err
	}
	var tk struct {
		Ticket string `json:"ticket"`
	}
	if err := c.call(ctx, "data.connect", map[string]string{
		"session_id":  sessionID,
		"block_id":    blockID,
		"data_ref_id": "terminal",
		"subprotocol": rexTerminalSubprotocol,
	}, &tk); err != nil {
		return err
	}
	ticket, err := base64.RawURLEncoding.DecodeString(tk.Ticket)
	if err != nil || len(ticket) != 16 {
		return fmt.Errorf("rex: bad data ticket %q", tk.Ticket)
	}
	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(dctx, "ws://rex/api/data", &websocket.DialOptions{
		HTTPClient:   c.http,
		Subprotocols: []string{rexTerminalSubprotocol},
	})
	if err != nil {
		return err
	}
	defer ws.CloseNow()
	ws.SetReadLimit(4 << 20)
	if err := ws.Write(dctx, websocket.MessageBinary, append([]byte{0x01}, ticket...)); err != nil {
		return err
	}
	_, welcome, err := ws.Read(dctx)
	if err != nil {
		return err
	}
	if len(welcome) < 1 || welcome[0] != 0x81 {
		return fmt.Errorf("rex: data connection rejected: % x", welcome)
	}
	for {
		_, _, err := ws.Read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				// Status 1000 ends the stream, so the server doesn't keep
				// it waiting for a resume.
				ws.Close(websocket.StatusNormalClosure, "")
			}
			return err
		}
		onData()
	}
}
