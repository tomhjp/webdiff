package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// The repos under rootDir stay on their main branch, because work
// happens in worktrees (see onMainRepos). The repo puller runs
// `git pull --ff-only` in each repo once a day, so that the repos stay
// near the latest main. New worktrees then start from a recent commit,
// and agents that run in a repo see recent code.
const (
	repoPullInterval = 24 * time.Hour
	repoPullCheck    = time.Hour
	repoPullTimeout  = 5 * time.Minute
)

// errNoUpstream tells that a repo has no branch to pull from: HEAD is
// detached, or the branch has no upstream (for example, a repo with no
// remote).
var errNoUpstream = errors.New("no upstream branch")

// startRepoPuller starts the daily pull. It checks at start and then
// each repoPullCheck. The time of the last attempt for each repo is in
// statePath, so a restart of webdiff does not cause a new pull.
func startRepoPuller(ctx context.Context, statePath string) {
	go func() {
		for {
			pullDueRepos(ctx, statePath, rootRepos(), time.Now())
			select {
			case <-ctx.Done():
				return
			case <-time.After(repoPullCheck):
			}
		}
	}()
}

// rootRepos maps the name of each repo under rootDir to its path.
func rootRepos() map[string]string {
	out := map[string]string{}
	for _, ref := range allManagedRefs() {
		if ref.kind == "repos" {
			out[ref.name] = ref.abs
		}
	}
	return out
}

// pullDueRepos pulls each repo in repos that had no pull attempt in the
// last repoPullInterval. It records a failed attempt too, so a repo
// that cannot fast-forward does not cause a new fetch and a new log
// line each hour.
func pullDueRepos(ctx context.Context, statePath string, repos map[string]string, now time.Time) {
	state := loadPullState(statePath)
	for name, path := range repos {
		if ctx.Err() != nil {
			return
		}
		if last, ok := state[name]; ok && now.Sub(time.Unix(last, 0)) < repoPullInterval {
			continue
		}
		pctx, cancel := context.WithTimeout(ctx, repoPullTimeout)
		err := pullRepo(pctx, path)
		cancel()
		switch {
		case errors.Is(err, errNoUpstream):
			log.Printf("daily pull %s: skipped: %v", name, err)
		case err != nil:
			log.Printf("daily pull %s: %v", name, err)
		default:
			log.Printf("daily pull %s: ok", name)
		}
		state[name] = now.Unix()
		if err := savePullState(statePath, state); err != nil {
			log.Printf("daily pull: save state: %v", err)
		}
	}
}

// pullRepo fast-forwards the current branch of repo to its upstream.
// It never merges or rebases. Git keeps the uncommitted changes, and it
// stops the pull if the pull would change them.
func pullRepo(ctx context.Context, repo string) error {
	up := exec.CommandContext(ctx, "git", "-C", repo, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}")
	if err := up.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errNoUpstream
	}
	cmd := exec.CommandContext(ctx, "git", "-C", repo, "pull", "--ff-only", "--quiet")
	// Do not wait for a credential prompt that nobody can answer.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git pull --ff-only: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// loadPullState reads the map of repo name to the unix time of its last
// pull attempt. A missing or bad file gives an empty map, so all repos
// get a pull.
func loadPullState(path string) map[string]int64 {
	state := map[string]int64{}
	b, err := os.ReadFile(path)
	if err != nil {
		return state
	}
	if err := json.Unmarshal(b, &state); err != nil || state == nil {
		return map[string]int64{}
	}
	return state
}

func savePullState(path string, state map[string]int64) error {
	b, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, b)
}

// writeFileAtomic writes b to path through a temporary file in the same
// directory, so a reader never sees a part of the file.
func writeFileAtomic(path string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}
