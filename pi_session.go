package main

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Pi saves each conversation as a JSONL session file. webdiff gives each
// Pi session that it starts a session ID that it chooses, and records
// that ID in the history metadata. The ID identifies the conversation
// itself, not the terminal that showed it. Thus "resume" can continue the
// conversation, and doesn't paste scrollback that was drawn at the width
// of the last viewer.
//
// Limit: if the user starts a different conversation in the same Pi
// process (/new, /resume, /fork), the recorded ID still names the first
// one.

// piAgentName is the agent name that resolveAgent returns for Pi.
const piAgentName = "pi"

// piSessionIDRE matches the session IDs that webdiff makes. They go into
// a `bash -lc` command line, so nothing else is accepted.
var piSessionIDRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// newPiSessionID returns a new UUIDv7, the same kind of ID that Pi makes
// for itself.
func newPiSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	ms := uint64(time.Now().UnixMilli())
	var ts [8]byte
	binary.BigEndian.PutUint64(ts[:], ms)
	copy(b[:6], ts[2:])
	b[6] = b[6]&0x0f | 0x70 // version 7
	b[8] = b[8]&0x3f | 0x80 // RFC 9562 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// piSessionsDir returns the directory where Pi keeps its session files,
// one subdirectory for each working directory. It is "" if it isn't known.
func piSessionsDir() string {
	if dir := os.Getenv("PI_CODING_AGENT_DIR"); dir != "" {
		if rest, ok := strings.CutPrefix(dir, "~/"); ok {
			home, err := os.UserHomeDir()
			if err != nil {
				return ""
			}
			dir = filepath.Join(home, rest)
		}
		return filepath.Join(dir, "sessions")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".pi", "agent", "sessions")
}

// piSessionFile returns the session file of the Pi session id, or "" if
// Pi hasn't written one. Pi writes the file when the conversation has its
// first reply, and names it "<time>_<id>.jsonl".
func piSessionFile(id string) string {
	dir := piSessionsDir()
	if dir == "" || !piSessionIDRE.MatchString(id) {
		return ""
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "*", "*_"+id+".jsonl"))
	if len(matches) == 0 {
		return ""
	}
	return matches[0]
}

// piCommand returns cmd with the flag that starts Pi on session id. If
// Pi has a file for the session, Pi opens that file, even when the
// worktree has moved since the session started. Otherwise Pi makes a new
// session with that ID, or opens the session with that ID for the
// working directory.
func piCommand(cmd, id string) string {
	if path := piSessionFile(id); path != "" {
		return cmd + " --session " + shellQuote(path)
	}
	return cmd + " --session-id " + id
}

// shellQuote quotes s as one word for a POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
