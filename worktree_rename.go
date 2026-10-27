package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

var worktreeRenameMu sync.Mutex

func writeWorktreeRenameButton(w io.Writer, name, dir string) {
	fmt.Fprintf(w, `<button class="toggle" type="button" id="wt-rename-btn" data-name="%s" data-branch="%s" title="rename this worktree and branch">rename</button>`,
		template.HTMLEscapeString(name), template.HTMLEscapeString(currentBranch(dir)))
}

func handleWorktreeRename(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Name   string `json:"name"`
		Branch string `json:"branch"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	worktreeRenameMu.Lock()
	defer worktreeRenameMu.Unlock()
	ref, ok := resolveRepoRef("/worktrees/" + req.Name + "/")
	if !ok || ref.kind != "worktrees" {
		http.Error(w, "unknown worktree", http.StatusNotFound)
		return
	}
	path, status, err := renameWorktree(r.Context(), ref, strings.TrimSpace(req.Branch))
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}
	if statsManager != nil {
		// The rename is complete. A discovery failure must not report it as failed.
		_ = statsManager.RefreshDiscovery(r.Context())
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"url":  "/worktrees/" + worktreeSlug(path) + "/",
		"path": path,
	})
}

func renameWorktree(ctx context.Context, ref repoRef, branch string) (string, int, error) {
	fail := func(status int, message string) (string, int, error) {
		return "", status, fmt.Errorf("%s", message)
	}
	if branch == "" || exec.Command("git", "check-ref-format", "refs/heads/"+branch).Run() != nil || strings.HasPrefix(branch, "-") || branch == "HEAD" {
		return fail(http.StatusBadRequest, "invalid branch name")
	}
	leaf := worktreeLeaf(branch)
	if leaf == "" {
		return fail(http.StatusBadRequest, "branch name has no usable characters")
	}
	out, err := exec.Command("git", "-C", ref.abs, "symbolic-ref", "--quiet", "--short", "HEAD").Output()
	if err != nil {
		return fail(http.StatusBadRequest, "worktree must have a checked-out branch")
	}
	oldBranch := strings.TrimSpace(string(out))
	newPath := filepath.Join(filepath.Dir(ref.abs), leaf)
	newName := worktreeSlug(newPath)
	if newPath != ref.abs {
		if _, err := os.Lstat(newPath); err == nil {
			return fail(http.StatusConflict, "worktree directory already exists")
		} else if !os.IsNotExist(err) {
			return fail(http.StatusInternalServerError, err.Error())
		}
		for _, wt := range listManagedWorktrees() {
			if wt.Name == newName {
				return fail(http.StatusConflict, "worktree slug already exists")
			}
		}
	}
	if branch != oldBranch && exec.Command("git", "-C", ref.abs, "show-ref", "--verify", "--quiet", "refs/heads/"+branch).Run() == nil {
		return fail(http.StatusConflict, "branch already exists")
	}
	oldSession, newSession := repoSessionName(ref.abs), repoSessionName(newPath)
	live := sessionExists(ctx, oldSession)
	if oldSession != newSession && sessionExists(ctx, newSession) {
		return fail(http.StatusConflict, "tmux session already exists")
	}
	git := func(dir string, args ...string) error {
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(out)))
		}
		return nil
	}
	if live && oldSession != newSession {
		snapshotSessionHistory(ctx, oldSession)
	}
	if branch != oldBranch {
		if err := git(ref.abs, "branch", "-m", branch); err != nil {
			return fail(http.StatusBadGateway, err.Error())
		}
	}
	rollbackBranch := func(dir string, cause error) error {
		if branch != oldBranch {
			if err := git(dir, "branch", "-m", oldBranch); err != nil {
				return fmt.Errorf("%v; rollback failed: %w", cause, err)
			}
		}
		return cause
	}
	if newPath != ref.abs {
		if err := git(ref.abs, "worktree", "move", ref.abs, newPath); err != nil {
			return fail(http.StatusBadGateway, rollbackBranch(ref.abs, err).Error())
		}
	}
	if live && oldSession != newSession {
		if _, err := tmuxOut(ctx, "rename-session", "-t", "="+oldSession, newSession); err != nil {
			if moveErr := git(newPath, "worktree", "move", newPath, ref.abs); moveErr != nil {
				return fail(http.StatusBadGateway, fmt.Sprintf("rename session: %v; rollback move failed: %v", err, moveErr))
			}
			return fail(http.StatusBadGateway, rollbackBranch(ref.abs, err).Error())
		}
		clearSessionLive(oldSession)
		wdSessionsMu.Lock()
		wdSpinning[newSession] = wdSpinning[oldSession]
		wdLastRaw[newSession] = wdLastRaw[oldSession]
		wdLastChange[newSession] = wdLastChange[oldSession]
		delete(wdSpinning, oldSession)
		delete(wdLastRaw, oldSession)
		delete(wdLastChange, oldSession)
		delete(wdSessionRepo, oldSession)
		wdSessionsMu.Unlock()
	}
	if live {
		recordRepoSession(newSession, newPath)
	}
	if indexCache != nil {
		_ = indexCache.Remove("worktrees-" + ref.name)
		if newName != ref.name {
			_ = indexCache.Remove("worktrees-" + newName)
		}
	}
	return newPath, http.StatusOK, nil
}
