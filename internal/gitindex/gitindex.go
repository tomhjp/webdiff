// Package gitindex maintains private, warm Git indexes for inspecting
// working trees without changing their real indexes.
package gitindex

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

type entry struct {
	mu   sync.Mutex
	dir  string
	head string
}

// Cache owns one private index per checkout key. Keys must be stable and
// filesystem-safe (webdiff uses the repo/worktree URL slug).
type Cache struct {
	root  string
	mu    sync.Mutex
	byKey map[string]*entry
}

func New(root string) *Cache {
	return &Cache{root: root, byKey: make(map[string]*entry)}
}

func (c *Cache) get(key string) *entry {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.byKey[key]
	if e == nil {
		e = &entry{dir: filepath.Join(c.root, key)}
		c.byKey[key] = e
	}
	return e
}

// Acquire locks checkout's private index, updates it from the working tree,
// and returns an environment suitable for read-only diff commands. The caller
// must call release. The checkout's real index is never touched.
func (c *Cache) Acquire(ctx context.Context, checkout, key string) (env []string, release func(), err error) {
	e := c.get(key)
	e.mu.Lock()
	locked := true
	release = func() {
		if locked {
			locked = false
			e.mu.Unlock()
		}
	}
	fail := func(err error) ([]string, func(), error) {
		release()
		return nil, func() {}, err
	}

	runOutput := func(args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, "git", args...)
		return cmd.Output()
	}
	headOut, err := runOutput("-C", checkout, "rev-parse", "HEAD")
	if err != nil {
		return fail(fmt.Errorf("resolve HEAD: %w", err))
	}
	head := strings.TrimSpace(string(headOut))
	objOut, err := runOutput("-C", checkout, "rev-parse", "--git-path", "objects")
	if err != nil {
		return fail(fmt.Errorf("resolve objects: %w", err))
	}
	objects := strings.TrimSpace(string(objOut))
	if !filepath.IsAbs(objects) {
		objects = filepath.Join(checkout, objects)
	}

	indexFile := filepath.Join(e.dir, "index")
	objDir := filepath.Join(e.dir, "objects")
	headFile := filepath.Join(e.dir, "HEAD")
	if e.head == "" {
		if b, readErr := os.ReadFile(headFile); readErr == nil {
			e.head = strings.TrimSpace(string(b))
		}
	}
	rebuild := head != e.head
	if !rebuild {
		if _, statErr := os.Stat(indexFile); statErr != nil {
			rebuild = true
		}
	}
	if rebuild {
		if err := os.RemoveAll(e.dir); err != nil {
			return fail(fmt.Errorf("clear private index: %w", err))
		}
		if err := os.MkdirAll(objDir, 0700); err != nil {
			return fail(fmt.Errorf("create private index: %w", err))
		}
	}

	env = append(os.Environ(),
		"GIT_INDEX_FILE="+indexFile,
		"GIT_OBJECT_DIRECTORY="+objDir,
		"GIT_ALTERNATE_OBJECT_DIRECTORIES="+objects,
	)
	run := func(args ...string) error {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = checkout
		cmd.Env = env
		return cmd.Run()
	}
	if rebuild {
		if err := run("read-tree", "HEAD"); err != nil {
			return fail(fmt.Errorf("seed private index: %w", err))
		}
		if err := os.WriteFile(headFile, []byte(head), 0600); err != nil {
			return fail(fmt.Errorf("record private index HEAD: %w", err))
		}
		e.head = head
	}
	if err := run("add", "-A"); err != nil {
		return fail(fmt.Errorf("update private index: %w", err))
	}
	return env, release, nil
}

func (c *Cache) Remove(key string) error {
	c.mu.Lock()
	e := c.byKey[key]
	delete(c.byKey, key)
	c.mu.Unlock()
	if e != nil {
		e.mu.Lock()
		defer e.mu.Unlock()
	}
	return os.RemoveAll(filepath.Join(c.root, key))
}
