package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// newTestRepo makes a repo with a main branch and an origin remote,
// and a branch "feature" with three commits on top of origin/main.
// It returns the repo, the git helper, and the SHAs of the three
// commits, the oldest first.
func newTestRepo(t *testing.T) (string, func(args ...string) string, []string) {
	t.Helper()
	dir := t.TempDir()
	remote := filepath.Join(dir, "github", "owner", "proj.git")
	repo := filepath.Join(dir, "root", "proj")
	run := func(path string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", path}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_GLOBAL=/dev/null",
			"GIT_CONFIG_SYSTEM=/dev/null",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git -C %s %s: %v: %s", path, strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	for _, d := range []string{remote, repo} {
		if err := os.MkdirAll(d, 0700); err != nil {
			t.Fatal(err)
		}
	}
	run(remote, "init", "-q", "--bare", "-b", "main")
	run(repo, "init", "-q", "-b", "main")
	run(repo, "config", "user.name", "Test")
	run(repo, "config", "user.email", "test@example.com")
	run(repo, "commit", "-q", "--allow-empty", "-m", "base")
	run(repo, "remote", "add", "origin", remote)
	run(repo, "push", "-q", "origin", "main")
	run(repo, "fetch", "-q", "origin")
	run(repo, "checkout", "-q", "-b", "feature")
	var shas []string
	for i, name := range []string{"a", "b", "c"} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(name+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		run(repo, "add", name)
		env := []string{"GIT_AUTHOR_DATE=@" + []string{"1000000000", "1000000100", "1000000200"}[i] + " +0100"}
		cmd := exec.Command("git", "-C", repo, "commit", "-q", "-m", "add "+name+"\n\nbody "+name)
		cmd.Env = append(os.Environ(), env...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("commit: %v: %s", err, out)
		}
		shas = append(shas, run(repo, "rev-parse", "HEAD"))
	}
	git := func(args ...string) string {
		t.Helper()
		return run(repo, args...)
	}
	return repo, git, shas
}

func TestRewordCommit(t *testing.T) {
	repo, git, shas := newTestRepo(t)
	os.WriteFile(filepath.Join(repo, "dirty"), []byte("x\n"), 0600)
	treeBefore := git("rev-parse", "HEAD^{tree}")

	rewritten, err := rewordCommit(context.Background(), repo, shas[1], "new subject\n\nnew body\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(rewritten) != 2 || rewritten[shas[0]] != "" {
		t.Fatalf("rewritten = %v, want new SHAs for the last two commits", rewritten)
	}
	head := git("rev-parse", "HEAD")
	if head != rewritten[shas[2]] {
		t.Errorf("HEAD = %s, want %s", head, rewritten[shas[2]])
	}
	if got := git("symbolic-ref", "HEAD"); got != "refs/heads/feature" {
		t.Errorf("HEAD = %s, want the feature branch", got)
	}
	if got := git("rev-parse", "HEAD^{tree}"); got != treeBefore {
		t.Errorf("tree changed")
	}
	if got := git("log", "-1", "--format=%B", rewritten[shas[1]]); got != "new subject\n\nnew body" {
		t.Errorf("message = %q", got)
	}
	if got := git("rev-parse", rewritten[shas[1]]+"^"); got != shas[0] {
		t.Errorf("parent of the edited commit = %s, want %s", got, shas[0])
	}
	if got := git("rev-parse", "HEAD^"); got != rewritten[shas[1]] {
		t.Errorf("parent of HEAD = %s, want %s", got, rewritten[shas[1]])
	}
	// The commits after the edited commit keep their messages and
	// author dates.
	if got := git("log", "-1", "--format=%B%x00%ad", "--date=raw", "HEAD"); got != "add c\n\nbody c\n\x001000000200 +0100" {
		t.Errorf("HEAD message and date = %q", got)
	}
	// The working tree does not change.
	if _, err := os.Stat(filepath.Join(repo, "dirty")); err != nil {
		t.Errorf("working tree file is gone: %v", err)
	}

	if _, err := rewordCommit(context.Background(), repo, shas[1], "x\n"); err == nil {
		t.Errorf("reword of a commit that is not on the branch: no error")
	}
	// The same message changes nothing.
	same, err := rewordCommit(context.Background(), repo, head, "add c\n\nbody c\n")
	if err != nil || len(same) != 0 {
		t.Errorf("same message: rewritten = %v, err = %v", same, err)
	}
}

func TestRewordHeadCommit(t *testing.T) {
	repo, git, shas := newTestRepo(t)
	git("add", "-N", ".")
	rewritten, err := rewordCommit(context.Background(), repo, shas[2], "last\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := git("log", "-1", "--format=%s"); got != "last" || rewritten[shas[2]] != git("rev-parse", "HEAD") {
		t.Errorf("subject = %q, rewritten = %v", got, rewritten)
	}
	if got := git("rev-parse", "HEAD^"); got != shas[1] {
		t.Errorf("parent = %s, want %s", got, shas[1])
	}
}

func TestCommitWorkingTree(t *testing.T) {
	repo, git, shas := newTestRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "new"), []byte("new\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "a"), []byte("changed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	sha, err := commitWorkingTree(context.Background(), repo, "working\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := git("rev-parse", "HEAD"); got != sha {
		t.Errorf("HEAD = %s, want %s", got, sha)
	}
	if got := git("rev-parse", "HEAD^"); got != shas[2] {
		t.Errorf("parent = %s, want %s", got, shas[2])
	}
	if got := git("log", "-1", "--format=%B"); got != "working\n\nSigned-off-by: Test <test@example.com>" {
		t.Errorf("message = %q", got)
	}
	if got := git("status", "--porcelain"); got != "" {
		t.Errorf("status = %q, want clean", got)
	}
	if _, err := commitWorkingTree(context.Background(), repo, "nothing\n"); err == nil {
		t.Errorf("commit of a clean working tree: no error")
	}
}

func TestCleanCommitMessage(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"subject", "subject\n"},
		{"\n\nsubject  \r\n\r\nbody\t\n\n", "subject\n\nbody\n"},
	}
	for _, tt := range tests {
		got, err := cleanCommitMessage(tt.in)
		if err != nil || got != tt.want {
			t.Errorf("cleanCommitMessage(%q) = %q, %v, want %q", tt.in, got, err, tt.want)
		}
	}
	if _, err := cleanCommitMessage(" \n\t\n"); err == nil {
		t.Errorf("empty message: no error")
	}
}

func TestHandleCommitMessage(t *testing.T) {
	repo, git, shas := newTestRepo(t)
	oldRoot := rootDir
	rootDir = filepath.Dir(repo)
	t.Cleanup(func() { rootDir = oldRoot })
	post := func(ctype, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/api/commits/message", strings.NewReader(body))
		r.Header.Set("Content-Type", ctype)
		w := httptest.NewRecorder()
		handleCommitMessage(w, r)
		return w
	}
	if w := post("text/plain", `{}`); w.Code != http.StatusUnsupportedMediaType {
		t.Errorf("text/plain: status %d", w.Code)
	}
	w := post("application/json", `{"kind":"repos","name":"proj","commit":"`+shas[0]+`","message":"edited"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var res struct {
		Rewritten map[string]string `json:"rewritten"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil || len(res.Rewritten) != 3 {
		t.Fatalf("response %s: %v", w.Body, err)
	}
	if got := git("log", "-1", "--format=%B", res.Rewritten[shas[0]]); got != "edited" {
		t.Errorf("message = %q", got)
	}
	if w := post("application/json", `{"kind":"repos","name":"proj","commit":"working","message":"x"}`); w.Code != http.StatusConflict {
		t.Errorf("commit of a clean working tree: status %d", w.Code)
	}
}
