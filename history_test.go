package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// newHistoryTestRepo points the history roots at temp dirs and creates a
// repo under rootDir whose repoSessionName is session, so
// writeHistoryMeta's sessionRepoRef lookup resolves (which is what gates
// the Live flag). Returns the repo's basename.
func newHistoryTestRepo(t *testing.T, session string) string {
	t.Helper()
	historyRoot = t.TempDir()
	rootDir = t.TempDir()
	worktreesRoot = filepath.Join(rootDir, "wt")

	base := session[:len(session)-len("-wd")]
	repo := filepath.Join(rootDir, base)
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	if got := repoSessionName(repo); got != session {
		t.Fatalf("repoSessionName(%q) = %q, want %q", repo, got, session)
	}
	return base
}

// writeLegacyMeta writes a meta the way pre-Live webdiff did: no "live"
// key at all, so unmarshalling yields false.
func writeLegacyMeta(t *testing.T, id, session string, started int64) {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"id":      id,
		"session": session,
		"kind":    "repos",
		"name":    session,
		"started": started,
		"updated": started,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(historyRoot, id+".json"), b, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestHistoryMetaLive(t *testing.T) {
	newHistoryTestRepo(t, "foo-wd")

	id := historyID(1000, "foo-wd")
	writeHistoryMeta(id, "foo-wd", 1000)
	m, ok := readHistoryMeta(id)
	if !ok {
		t.Fatal("readHistoryMeta failed")
	}
	if !m.Live {
		t.Error("Live not set on a resolvable session's first capture")
	}
	if m.Kind != "repos" {
		t.Errorf("Kind = %q, want repos", m.Kind)
	}

	// A capture on an already-existing meta keeps it live.
	writeHistoryMeta(id, "foo-wd", 1000)
	if m, _ := readHistoryMeta(id); !m.Live {
		t.Error("Live lost on update")
	}

	if err := setHistoryLive(id, false); err != nil {
		t.Fatalf("setHistoryLive: %v", err)
	}
	m, _ = readHistoryMeta(id)
	if m.Live {
		t.Error("Live still set after clear")
	}
	// Clearing must not disturb the rest of the metadata.
	if m.Started != 1000 || m.Kind != "repos" {
		t.Errorf("setHistoryLive clobbered meta: %+v", m)
	}
}

// An unresolvable session (someone's own `tmux new -s x-wd`) has no ref to
// resume into, so it must never become sticky.
func TestHistoryMetaLiveUnresolved(t *testing.T) {
	historyRoot = t.TempDir()
	rootDir = t.TempDir()
	worktreesRoot = filepath.Join(rootDir, "wt")

	id := historyID(1000, "scratch-wd")
	writeHistoryMeta(id, "scratch-wd", 1000)
	m, ok := readHistoryMeta(id)
	if !ok {
		t.Fatal("readHistoryMeta failed")
	}
	if m.Live {
		t.Error("Live set for a session that resolves to no repo")
	}
	if len(stoppedSessions(nil)) != 0 {
		t.Error("unresolved session showed up as stopped")
	}
}

func TestSetHistoryLiveRejectsBadID(t *testing.T) {
	historyRoot = t.TempDir()

	// A traversal id must be refused outright: the id arrives in a request
	// body and is joined into a path we then write.
	for _, bad := range []string{"../etc/passwd", "12/34", "nope", ""} {
		if err := setHistoryLive(bad, false); err == nil {
			t.Errorf("setHistoryLive(%q) accepted a bad id", bad)
		}
	}
	if entries, _ := os.ReadDir(historyRoot); len(entries) != 0 {
		t.Errorf("bad ids wrote %d files", len(entries))
	}

	// A well-formed id with no archive behind it must not create one:
	// otherwise it'd appear in every listing and 404 on click.
	if err := setHistoryLive("1234-ghost-wd", false); err == nil {
		t.Error("setHistoryLive accepted a missing archive")
	}
	if entries, _ := os.ReadDir(historyRoot); len(entries) != 0 {
		t.Errorf("missing-archive id wrote %d files", len(entries))
	}
}

func TestStoppedSessions(t *testing.T) {
	newHistoryTestRepo(t, "foo-wd")

	writeHistoryMeta(historyID(1000, "foo-wd"), "foo-wd", 1000)
	stopped := stoppedSessions(nil)
	if len(stopped) != 1 || stopped[0].Session != "foo-wd" {
		t.Fatalf("stoppedSessions = %+v, want one foo-wd", stopped)
	}

	// A session that's live in tmux belongs in the live rows, not here.
	if got := stoppedSessions(map[string]bool{"foo-wd": true}); len(got) != 0 {
		t.Errorf("live session listed as stopped: %+v", got)
	}

	// Legacy archives predate the flag and must stay out of the listing.
	writeLegacyMeta(t, historyID(500, "old-wd"), "old-wd", 500)
	if got := stoppedSessions(nil); len(got) != 1 {
		t.Errorf("legacy archive became sticky: %+v", got)
	}
}

// Only the newest instance per session name is considered, and the Live
// filter is applied after that narrowing. Filtering first would let
// forgetting the newest archive promote an older sticky one into its place,
// so the row would appear not to have gone away.
func TestStoppedSessionsForgetDoesNotPromoteOlder(t *testing.T) {
	newHistoryTestRepo(t, "foo-wd")

	oldID := historyID(1000, "foo-wd")
	newID := historyID(2000, "foo-wd")
	writeHistoryMeta(oldID, "foo-wd", 1000)
	writeHistoryMeta(newID, "foo-wd", 2000)

	stopped := stoppedSessions(nil)
	if len(stopped) != 1 || stopped[0].ID != newID {
		t.Fatalf("stoppedSessions = %+v, want only the newest (%s)", stopped, newID)
	}

	if err := setHistoryLive(newID, false); err != nil {
		t.Fatal(err)
	}
	if got := stoppedSessions(nil); len(got) != 0 {
		t.Errorf("forgetting the newest promoted an older archive: %+v", got)
	}
}

func TestStoppedSessionsNewestFirst(t *testing.T) {
	newHistoryTestRepo(t, "foo-wd")
	// Two more repos so all three sessions resolve.
	for _, base := range []string{"bar", "baz"} {
		if err := os.MkdirAll(filepath.Join(rootDir, base, ".git"), 0700); err != nil {
			t.Fatal(err)
		}
	}

	// Updated is set from time.Now() by writeHistoryMeta, so drive the
	// ordering by rewriting it explicitly.
	for _, tc := range []struct {
		session string
		updated int64
	}{
		{"foo-wd", 300},
		{"bar-wd", 100},
		{"baz-wd", 200},
	} {
		id := historyID(1000, tc.session)
		writeHistoryMeta(id, tc.session, 1000)
		m, _ := readHistoryMeta(id)
		m.Updated = tc.updated
		writeMetaFile(filepath.Join(historyRoot, id+".json"), m)
	}

	stopped := stoppedSessions(nil)
	want := []string{"foo-wd", "baz-wd", "bar-wd"}
	if len(stopped) != len(want) {
		t.Fatalf("len = %d, want %d", len(stopped), len(want))
	}
	for i, w := range want {
		if stopped[i].Session != w {
			t.Errorf("stopped[%d].Session = %q, want %q", i, stopped[i].Session, w)
		}
	}
}

func TestNewestArchive(t *testing.T) {
	newHistoryTestRepo(t, "foo-wd")

	writeHistoryMeta(historyID(1000, "foo-wd"), "foo-wd", 1000)
	writeHistoryMeta(historyID(3000, "foo-wd"), "foo-wd", 3000)
	writeHistoryMeta(historyID(2000, "foo-wd"), "foo-wd", 2000)

	m, ok := newestArchive("foo-wd")
	if !ok {
		t.Fatal("newestArchive not found")
	}
	if m.Started != 3000 {
		t.Errorf("Started = %d, want 3000", m.Started)
	}
	if _, ok := newestArchive("nope-wd"); ok {
		t.Error("newestArchive found a session that was never archived")
	}
}

// clearSessionLive is what the explicit-teardown paths call: they know the
// session name but not its instance id.
func TestClearSessionLive(t *testing.T) {
	newHistoryTestRepo(t, "foo-wd")

	id := historyID(1000, "foo-wd")
	writeHistoryMeta(id, "foo-wd", 1000)
	clearSessionLive("foo-wd")
	if m, _ := readHistoryMeta(id); m.Live {
		t.Error("clearSessionLive left the archive sticky")
	}
	// A session with no archive at all must be a no-op, not a panic.
	clearSessionLive("never-existed-wd")
}

func TestHistoryID(t *testing.T) {
	if got := historyID(1700000000, "webdiff-wd"); got != "1700000000-webdiff-wd" {
		t.Errorf("historyID = %q", got)
	}
	// Unsafe characters in the session collapse so the id is always a
	// single safe path component.
	if got := historyID(42, "a/b c.d-wd"); got != "42-a-b-c.d-wd" {
		t.Errorf("historyID sanitise = %q", got)
	}
}

func TestHistoryIDRE(t *testing.T) {
	valid := []string{"1700000000-webdiff-wd", "0-x", "42-a.b_c-wd"}
	invalid := []string{"", "webdiff-wd", "../etc", "12/34", "12-a/b", "abc-x"}
	for _, s := range valid {
		if !historyIDRE.MatchString(s) {
			t.Errorf("historyIDRE rejected valid id %q", s)
		}
	}
	for _, s := range invalid {
		if historyIDRE.MatchString(s) {
			t.Errorf("historyIDRE accepted invalid id %q", s)
		}
	}
}

func TestHistoryMetaRoundTrip(t *testing.T) {
	historyRoot = t.TempDir()
	// An empty rootDir means sessionRepoRef finds no match and
	// writeHistoryMeta falls back to the session-derived name.
	rootDir = t.TempDir()
	worktreesRoot = filepath.Join(rootDir, "wt")

	id := historyID(1000, "foo-wd")
	writeHistoryMeta(id, "foo-wd", 1000)

	metas := listHistory()
	if len(metas) != 1 {
		t.Fatalf("listHistory len = %d, want 1", len(metas))
	}
	m := metas[0]
	if m.Started != 1000 {
		t.Errorf("Started = %d, want 1000", m.Started)
	}
	if m.Name != "foo" {
		t.Errorf("Name = %q, want foo (session-derived fallback)", m.Name)
	}
	if m.Updated == 0 {
		t.Errorf("Updated not set")
	}

	// A second write must preserve Started (the instance's creation time)
	// rather than restamping it.
	writeHistoryMeta(id, "foo-wd", 1000)
	metas = listHistory()
	if len(metas) != 1 {
		t.Fatalf("second write created a duplicate: len = %d", len(metas))
	}
	if metas[0].Started != 1000 {
		t.Errorf("Started changed on update: %d", metas[0].Started)
	}
}

func TestListHistoryNewestFirst(t *testing.T) {
	historyRoot = t.TempDir()
	rootDir = t.TempDir()
	worktreesRoot = filepath.Join(rootDir, "wt")

	writeHistoryMeta(historyID(1000, "old-wd"), "old-wd", 1000)
	writeHistoryMeta(historyID(3000, "new-wd"), "new-wd", 3000)
	writeHistoryMeta(historyID(2000, "mid-wd"), "mid-wd", 2000)

	metas := listHistory()
	if len(metas) != 3 {
		t.Fatalf("len = %d, want 3", len(metas))
	}
	want := []int64{3000, 2000, 1000}
	for i, w := range want {
		if metas[i].Started != w {
			t.Errorf("metas[%d].Started = %d, want %d", i, metas[i].Started, w)
		}
	}
}
