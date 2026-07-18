package main

import (
	"testing"
)

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
	worktreesRoot = t.TempDir()

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
	worktreesRoot = t.TempDir()

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
