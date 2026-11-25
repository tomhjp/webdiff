package main

import (
	"context"
	"fmt"
	"html/template"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// A checkout can have more than one agent session. Each session has a
// number n, from 1 to maxCheckoutSessions:
//
//   - Session 1 is "<slug>-wd", the name that webdiff used before it
//     could start more than one session for a checkout.
//   - Session n > 1 is "<slug>-wd<n>", for example "corp-fix-x-wd2".
//
// The name of session 1 always ends in "-wd", and the other names end
// in a digit. Thus two checkouts cannot get the same session name, and
// the name of session n gives back its checkout and n without doubt.
// The names use only characters that tmux and the history ids accept.
//
// Pages and requests select a session with the "n" query parameter. If
// there is no "n", they use session 1.

// webdiffSessionSuffix is the suffix of session 1 of a checkout. The
// names of the other sessions add their number after it.
const webdiffSessionSuffix = "-wd"

// maxCheckoutSessions is the largest number of agent sessions that one
// checkout can have.
const maxCheckoutSessions = 20

// checkoutSessionName returns the name of session n of the checkout at
// repoPath. It returns the name of session 1 if n is less than 2.
func checkoutSessionName(repoPath string, n int) string {
	name := repoSessionName(repoPath)
	if n < 2 {
		return name
	}
	return name + strconv.Itoa(n)
}

// parseSessionName splits the name of a webdiff session into the name
// of session 1 of its checkout and the session number. ok is false if
// name is not the name of a webdiff session.
func parseSessionName(name string) (primary string, n int, ok bool) {
	if strings.HasSuffix(name, webdiffSessionSuffix) {
		return name, 1, true
	}
	digits := len(name)
	for digits > 0 && name[digits-1] >= '0' && name[digits-1] <= '9' {
		digits--
	}
	num := name[digits:]
	primary = name[:digits]
	if num == "" || num[0] == '0' || !strings.HasSuffix(primary, webdiffSessionSuffix) {
		return "", 0, false
	}
	n, err := strconv.Atoi(num)
	if err != nil || n < 2 || n > maxCheckoutSessions {
		return "", 0, false
	}
	return primary, n, true
}

// isWebdiffSession reports whether name is the name of a session that
// webdiff manages. Requests from a browser can only change these
// sessions.
func isWebdiffSession(name string) bool {
	_, _, ok := parseSessionName(name)
	return ok
}

// sessionNumber returns the session number in a webdiff session name,
// or 1 if name is not the name of a webdiff session.
func sessionNumber(name string) int {
	if _, n, ok := parseSessionName(name); ok {
		return n
	}
	return 1
}

// sessionGroupKey returns the name of session 1 of the checkout of a
// webdiff session, and name itself for all other sessions. The home
// page uses it to show all the sessions of a checkout together.
func sessionGroupKey(name string) string {
	if primary, _, ok := parseSessionName(name); ok {
		return primary
	}
	return name
}

// sessionNumberParam reads the "n" query parameter. It returns 1 if
// there is no parameter, and ok=false if the value is not a session
// number.
func sessionNumberParam(q url.Values) (n int, ok bool) {
	v := q.Get("n")
	if v == "" {
		return 1, true
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > maxCheckoutSessions {
		return 0, false
	}
	return n, true
}

// sessionQuery returns the query string that selects session n and the
// agent id agentID, for example "?n=2&agent=pi". Session 1 and the
// default agent need no parameter. It returns "" if no parameter is
// necessary.
func sessionQuery(n int, agentID string) string {
	v := url.Values{}
	if n > 1 {
		v.Set("n", strconv.Itoa(n))
	}
	if agentID != "" && agentID != agentName {
		if _, ok := knownAgents[agentID]; ok {
			v.Set("agent", agentID)
		}
	}
	if len(v) == 0 {
		return ""
	}
	return "?" + v.Encode()
}

// newAgentURL returns the /agent URL that starts a new agent session in
// the checkout at refURL, with the lowest free session number. agentID
// selects the agent; "" selects the default agent.
func newAgentURL(refURL, agentID string) string {
	v := url.Values{}
	v.Set("new", "1")
	if agentID != "" && agentID != agentName {
		if _, ok := knownAgents[agentID]; ok {
			v.Set("agent", agentID)
		}
	}
	return "/agent" + refURL + "?" + v.Encode()
}

// sessionTag returns the text that tells session n apart from the other
// sessions of its checkout, for example "#2". It returns "" for
// session 1.
func sessionTag(n int) string {
	if n < 2 {
		return ""
	}
	return fmt.Sprintf("#%d", n)
}

// liveSessionNames returns the names of all running sessions, in Rex and
// in tmux.
func liveSessionNames(ctx context.Context) []string {
	var names []string
	for _, s := range rexc.snapshot() {
		if s.Agent != "" {
			names = append(names, s.Label)
		}
	}
	if out, err := tmuxOut(ctx, "list-sessions", "-F", "#{session_name}"); err == nil {
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			if line != "" {
				names = append(names, line)
			}
		}
	}
	return names
}

// checkoutSessionNames returns the names of the running sessions of the
// checkout at repoPath, ordered by session number.
func checkoutSessionNames(ctx context.Context, repoPath string) []string {
	primary := repoSessionName(repoPath)
	var out []string
	for _, name := range liveSessionNames(ctx) {
		if p, _, ok := parseSessionName(name); ok && p == primary && !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	slices.SortFunc(out, func(a, b string) int {
		return sessionNumber(a) - sessionNumber(b)
	})
	return out
}

// checkoutSession is a running agent session of a checkout.
type checkoutSession struct {
	n     int
	agent string // agent id, "" if not known
}

// liveCheckoutSessions returns the running agent sessions of the
// checkout at repoPath, ordered by session number.
func liveCheckoutSessions(ctx context.Context, repoPath string) []checkoutSession {
	var out []checkoutSession
	for _, name := range checkoutSessionNames(ctx, repoPath) {
		n := sessionNumber(name)
		// The session can stop after checkoutSessionNames.
		paneID, _, agent := repoStreamPane(ctx, repoPath, n)
		if paneID == "" {
			continue
		}
		out = append(out, checkoutSession{
			n:     n,
			agent: agent,
		})
	}
	return out
}

// writeOtherSessionLinks emits a link to the stream page of each
// session in sessions, other than session current. The "more" menus of
// the diff page and the stream page show these links. refURL is the URL
// of the checkout. The label of a link is the same as the label of the
// agent button on the diff page, for example "pi #2".
func writeOtherSessionLinks(b *strings.Builder, refURL string, current int, sessions []checkoutSession) {
	for _, s := range sessions {
		if s.n == current {
			continue
		}
		agent, _ := resolveAgent(s.agent)
		label := agent
		if tag := sessionTag(s.n); tag != "" {
			label += " " + tag
		}
		fmt.Fprintf(b, `<a class="toggle other-session" href="%s" title="open agent session %d of this checkout">%s</a>`,
			template.HTMLEscapeString("/stream"+refURL+sessionQuery(s.n, "")), s.n, template.HTMLEscapeString(label))
	}
}

// sessionsStarting holds the names of the sessions that a request has
// selected for a new agent but that can still be absent from Rex and
// tmux. Without it, two requests at the same time could select the same
// free number.
var (
	sessionsStartingMu sync.Mutex
	sessionsStarting   = map[string]bool{}
)

// reserveFreeSession selects the lowest session number of the checkout
// at repoPath that has no running session, and reserves it until the
// caller calls release.
func reserveFreeSession(ctx context.Context, repoPath string) (n int, release func(), err error) {
	live := checkoutSessionNames(ctx, repoPath)
	sessionsStartingMu.Lock()
	defer sessionsStartingMu.Unlock()
	for n := 1; n <= maxCheckoutSessions; n++ {
		name := checkoutSessionName(repoPath, n)
		if sessionsStarting[name] || slices.Contains(live, name) {
			continue
		}
		sessionsStarting[name] = true
		return n, func() {
			sessionsStartingMu.Lock()
			delete(sessionsStarting, name)
			sessionsStartingMu.Unlock()
		}, nil
	}
	return 0, nil, fmt.Errorf("this checkout already has %d agent sessions", maxCheckoutSessions)
}
