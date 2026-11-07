package main

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

func TestNewPiSessionID(t *testing.T) {
	// Pi's assertValidSessionId accepts this pattern.
	piValid := regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9._-]*[A-Za-z0-9])?$`)
	seen := map[string]bool{}
	for range 100 {
		id := newPiSessionID()
		if !piSessionIDRE.MatchString(id) || !piValid.MatchString(id) {
			t.Fatalf("bad id %q", id)
		}
		if id[14] != '7' {
			t.Errorf("id %q is not UUIDv7", id)
		}
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
}

func TestPiCommand(t *testing.T) {
	agentDir := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", agentDir)
	id := newPiSessionID()

	// No file yet: Pi makes the session with our ID.
	if got, want := piCommand("pi", id), "pi --session-id "+id; got != want {
		t.Errorf("piCommand = %q, want %q", got, want)
	}

	// With a file, Pi opens it, wherever the worktree is now.
	dir := filepath.Join(agentDir, "sessions", "--home-me-it's-a-repo--")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "2026-11-06T23-00-00-000Z_"+id+".jsonl")
	if err := os.WriteFile(file, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	want := "pi --flag --session '" + agentDir + "/sessions/--home-me-it'\\''s-a-repo--/2026-11-06T23-00-00-000Z_" + id + ".jsonl'"
	if got := piCommand("pi --flag", id); got != want {
		t.Errorf("piCommand = %q, want %q", got, want)
	}

	// An ID that isn't ours never reaches a glob.
	if got := piSessionFile("*"); got != "" {
		t.Errorf("piSessionFile(*) = %q", got)
	}
}

func TestSetHistoryPiSession(t *testing.T) {
	newHistoryTestRepo(t, "foo-wd")
	id := historyID(1000, "foo-wd")
	pi := newPiSessionID()

	// The record comes before the first capture, so it makes the meta.
	setHistoryPiSession(id, "foo-wd", 1000, pi)
	m, ok := readHistoryMeta(id)
	if !ok || m.PiSession != pi || m.Kind != "repos" || m.Name != "foo" || !m.Live || m.Started != 1000 {
		t.Fatalf("meta after record = %+v, %v", m, ok)
	}
	// The entry has no scrollback yet, which reads as empty.
	if b, err := readHistoryANSI(id); err != nil || len(b) != 0 {
		t.Errorf("readHistoryANSI = %q, %v", b, err)
	}

	// Captures must keep the ID.
	writeHistoryMeta(id, "foo-wd", 1000)
	if m, _ := readHistoryMeta(id); m.PiSession != pi {
		t.Errorf("PiSession after capture = %q, want %q", m.PiSession, pi)
	}
}

func TestResumablePiSession(t *testing.T) {
	newHistoryTestRepo(t, "foo-wd")
	ref := repoRef{
		kind: "repos",
		name: "foo",
	}
	pi := newPiSessionID()
	withPi := historyID(1000, "foo-wd")
	setHistoryPiSession(withPi, "foo-wd", 1000, pi)
	noPi := historyID(2000, "foo-wd")
	writeHistoryMeta(noPi, "foo-wd", 2000)

	if got, status, msg := resumablePiSession(withPi, ref); got != pi || status != 0 {
		t.Errorf("resumablePiSession = %q, %d %q", got, status, msg)
	}
	for _, tc := range []struct {
		id     string
		ref    repoRef
		status int
	}{
		{"../x", ref, http.StatusBadRequest},
		{historyID(3000, "foo-wd"), ref, http.StatusNotFound},
		{noPi, ref, http.StatusConflict},
		{withPi, repoRef{kind: "repos", name: "bar"}, http.StatusBadRequest},
	} {
		if got, status, _ := resumablePiSession(tc.id, tc.ref); got != "" || status != tc.status {
			t.Errorf("resumablePiSession(%q, %+v) = %q, %d; want status %d", tc.id, tc.ref, got, status, tc.status)
		}
	}
}
