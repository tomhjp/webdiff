package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bradfitz/gxbm/maintner"
	"github.com/bradfitz/gxbm/maintner/maintpb"
	"github.com/tomhjp/webdiff/internal/repostats"
)

// prs finds the GitHub pull request of the branch of each checkout. It
// is nil when the tracker has no source (see prConfig). A nil
// *prTracker finds no pull requests.
var prs *prTracker

// prTracker finds the GitHub pull request of a branch and keeps it
// current. It gets the data from a maintner server (see
// tailscale/corp/maintner), which keeps a copy of the GitHub data of
// the repos that it watches. maintner gets GitHub webhooks, thus it
// knows about a change in some seconds.
//
// The tracker does not poll for each branch. It follows the mutation
// log of maintner with a long poll, and it refreshes a branch only when
// a mutation is about its pull request, or about a pull request that has
// the branch as its head. When nothing changes, the only traffic is one
// open long poll. A full refresh each hour catches changes that the
// tail did not see.
//
// For a repo that maintner does not watch, or with no maintner, the
// tracker polls GitHub with git ls-remote at a slow interval (see
// pr_git.go). Git does not tell if a pull request is approved.
type prTracker struct {
	// apiURL is the URL of the ghapi endpoint of maintner, or "" for
	// no maintner.
	apiURL string
	// logURL is the URL of the mutation log of maintner.
	logURL string
	client *http.Client
	// gitPoll is the interval of the git poll of an active repo, or 0
	// for no git poll. gitPollIdle is the interval for the other repos
	// (see gitPollInterval).
	gitPoll, gitPollIdle time.Duration
	// gitBases are the base URLs of the git servers to ask about a
	// repo, in order. The tracker uses the first that answers.
	gitBases []string
	// sem limits the number of fetches that run at the same time.
	sem chan struct{}

	mu      sync.Mutex
	entries map[prKey]*prEntry
	// repos holds the GitHub repo ("owner/name") of each checkout
	// directory. An empty value means that the checkout has no GitHub
	// remote.
	repos map[string]string
	// untracked holds the GitHub repos that maintner does not watch.
	untracked map[string]bool
	// gitURLs holds the git URL that answered for each repo.
	gitURLs map[string]string
	// gitPolled holds the last time that the git poll asked about each
	// repo.
	gitPolled map[string]time.Time
	// pending holds the keys to refresh when debounce fires.
	pending  map[prKey]bool
	debounce *time.Timer
	// gap is true after the tail failed. The tail starts again at the
	// end of the log, thus the next mutation causes a full refresh.
	gap bool
	// changed is closed and replaced when a pull request changes.
	changed chan struct{}
}

// prKey identifies a branch of a GitHub repo.
type prKey struct {
	// Repo is the "owner/name" of the GitHub repo.
	Repo   string
	Branch string
}

type prEntry struct {
	pr *pullRequest // nil if the branch has no pull request
	// ready is closed when the first fetch is done.
	ready    chan struct{}
	fetched  bool
	fetching bool
	// again is true if the entry must be fetched again when the
	// current fetch is done.
	again bool
	// used is the last time a page showed the entry.
	used time.Time
	// dir is the checkout of the branch.
	dir string
}

// pullRequest is the GitHub pull request of a branch.
type pullRequest struct {
	Repo     string // "owner/name"
	Number   int32
	Title    string
	State    string // "OPEN", "MERGED" or "CLOSED"
	Draft    bool
	Approved bool
	// ChangesRequested is true if a reviewer requested changes.
	ChangesRequested bool
	// ReviewUnknown is true if the pull request comes from git, which
	// does not tell the review state. Then Title is empty, and State is
	// "OPEN" or "" for not known.
	ReviewUnknown bool
}

// URL returns the GitHub page of p.
func (p *pullRequest) URL() string {
	return "https://github.com/" + p.Repo + "/pull/" + strconv.Itoa(int(p.Number))
}

const (
	// prDebounce is how long the tracker waits after a mutation before
	// it refreshes. maintner often writes a number of mutations for
	// one change.
	prDebounce = 2 * time.Second
	// prFullRefresh is the interval of the full refresh.
	prFullRefresh = time.Hour
	// prUnusedTTL is how long the tracker keeps an entry that no page
	// showed.
	prUnusedTTL = 24 * time.Hour
	// prLookupWait is how long a page waits for the first fetch of
	// its pull requests.
	prLookupWait = 2 * time.Second
	// prMaxFetches is the maximum number of fetches that run at the
	// same time.
	prMaxFetches = 4
)

// errUntrackedRepo is the error of a fetch for a repo that maintner
// does not watch.
var errUntrackedRepo = errors.New("repo not tracked by maintner")

// prConfig configures a prTracker.
type prConfig struct {
	// Maintner is the base URL of a maintner server, for example
	// "http://maintner.corp.ts.net", or "" for no maintner.
	Maintner string
	// GitPoll is the interval of the git poll for a repo with a commit
	// in the last prActiveWindow, or 0 for no git poll.
	GitPoll time.Duration
	// GitPollIdle is the interval of the git poll for the other repos.
	// If it is less than GitPoll, the tracker uses GitPoll.
	GitPollIdle time.Duration
	// GitProxy is the base URL of a read-only git proxy for private
	// repos, for example "git://rogitproxy.corp.ts.net", or "". The
	// git poll tries public GitHub first.
	GitProxy string
}

// enabled reports whether c has a source of pull requests.
func (c prConfig) enabled() bool {
	return c.Maintner != "" || c.GitPoll > 0
}

// publicGitBase is the base URL of public GitHub for the git poll.
const publicGitBase = "https://github.com"

// newPRTracker returns a tracker with the configuration c. Call run to
// start the tail and the polls.
func newPRTracker(c prConfig) *prTracker {
	t := &prTracker{
		client: &http.Client{
			Timeout: 15 * time.Second,
		},
		gitPoll:     c.GitPoll,
		gitPollIdle: max(c.GitPollIdle, c.GitPoll),
		gitBases:    []string{publicGitBase},
		sem:         make(chan struct{}, prMaxFetches),
		entries:     make(map[prKey]*prEntry),
		repos:       make(map[string]string),
		untracked:   make(map[string]bool),
		gitURLs:     make(map[string]string),
		gitPolled:   make(map[string]time.Time),
		pending:     make(map[prKey]bool),
		changed:     make(chan struct{}),
	}
	if base := strings.TrimSuffix(c.Maintner, "/"); base != "" {
		t.apiURL = base + "/api/gh"
		t.logURL = base + "/logs/ts-v1/"
	}
	if p := strings.TrimSuffix(c.GitProxy, "/"); p != "" {
		t.gitBases = append(t.gitBases, p)
	}
	return t
}

// viaGitLocked reports whether the pull requests of repo come from the
// git poll and not from maintner.
func (t *prTracker) viaGitLocked(repo string) bool {
	return t.apiURL == "" || t.untracked[repo]
}

// startPRTracker starts t. The tail of the mutation log keeps a copy of
// the last log segment in a temporary directory, which it removes only
// when it stops. Thus on SIGTERM or SIGINT, stop t before the process
// exits.
func startPRTracker(t *prTracker) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		t.run(ctx)
	}()
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, os.Interrupt)
	go func() {
		sig := <-sigs
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
		log.Printf("exit on %v", sig)
		os.Exit(0)
	}()
}

// run follows the mutation log, and does the full refreshes and the git
// polls. It returns when ctx is done.
func (t *prTracker) run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Go(func() { t.refreshLoop(ctx) })
	if t.gitPoll > 0 {
		wg.Go(func() { t.gitPollLoop(ctx) })
	}
	if t.apiURL != "" {
		t.tailLoop(ctx)
	}
	wg.Wait()
}

// tailLoop follows the mutation log of maintner until ctx is done.
func (t *prTracker) tailLoop(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := maintner.TailNetworkMutationSource(ctx, t.logURL, func(ev maintner.MutationStreamEvent) error {
			if ev.Err != nil {
				// Stop and wait with backoff here, not with the
				// short constant retry of the tail.
				return ev.Err
			}
			t.handleMutation(ev.Mutation)
			return nil
		})
		if ctx.Err() != nil {
			return
		}
		t.mu.Lock()
		t.gap = true
		t.mu.Unlock()
		if time.Since(start) > 10*time.Minute {
			backoff = time.Second
		}
		log.Printf("maintner tail: %v; retry in %v", err, backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 5*time.Minute)
	}
}

// refreshLoop refreshes the entries from maintner each prFullRefresh,
// and removes the entries that no page showed for prUnusedTTL. The git
// poll refreshes the other entries.
func (t *prTracker) refreshLoop(ctx context.Context) {
	ticker := time.NewTicker(prFullRefresh)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		t.mu.Lock()
		for k, e := range t.entries {
			if time.Since(e.used) > prUnusedTTL && !e.fetching {
				delete(t.entries, k)
			}
		}
		t.mu.Unlock()
		t.refreshMaintner()
	}
}

// handleMutation refreshes the entries that the mutation m can change.
func (t *prTracker) handleMutation(m *maintpb.Mutation) {
	gi := m.GetGithubIssue()
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.gap {
		t.gap = false
		for k := range t.entries {
			if !t.viaGitLocked(k.Repo) {
				t.scheduleLocked(k)
			}
		}
		return
	}
	if gi == nil {
		return
	}
	repo := gi.GetOwner() + "/" + gi.GetRepo()
	head := gi.GetHead().GetRef()
	for k, e := range t.entries {
		if k.Repo != repo {
			continue
		}
		if (head != "" && head == k.Branch) || (e.pr != nil && e.pr.Number == gi.GetNumber()) {
			t.scheduleLocked(k)
		}
	}
}

// scheduleLocked refreshes k when the debounce timer fires.
func (t *prTracker) scheduleLocked(k prKey) {
	t.pending[k] = true
	if t.debounce == nil {
		t.debounce = time.AfterFunc(prDebounce, t.flushPending)
	}
}

func (t *prTracker) flushPending() {
	t.mu.Lock()
	keys := make([]prKey, 0, len(t.pending))
	for k := range t.pending {
		keys = append(keys, k)
	}
	clear(t.pending)
	t.debounce = nil
	t.mu.Unlock()
	t.refresh(keys)
}

// refreshMaintner refreshes the entries that come from maintner.
func (t *prTracker) refreshMaintner() {
	t.mu.Lock()
	keys := make([]prKey, 0, len(t.entries))
	for k := range t.entries {
		if !t.viaGitLocked(k.Repo) {
			keys = append(keys, k)
		}
	}
	t.mu.Unlock()
	t.refresh(keys)
}

// refresh fetches the entries of keys again, at most prMaxFetches at
// the same time. It does not wait for the fetches.
func (t *prTracker) refresh(keys []prKey) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, k := range keys {
		if e, ok := t.entries[k]; ok {
			t.startLocked(k, e)
		}
	}
}

// startLocked starts a fetch of the entry e of k. If a fetch of e runs,
// it does the fetch again when that fetch is done.
func (t *prTracker) startLocked(k prKey, e *prEntry) {
	if e.fetching {
		e.again = true
		return
	}
	e.fetching = true
	go func() {
		t.mu.Lock()
		for {
			dir := e.dir
			t.mu.Unlock()
			t.sem <- struct{}{}
			pr, err := t.fetchEntry(context.Background(), k, dir)
			<-t.sem
			t.mu.Lock()
			if err == nil {
				if !samePR(e.pr, pr) {
					e.pr = pr
					t.signalLocked()
				}
			} else {
				// Keep the last good value.
				log.Printf("pull request of %s %s: %v", k.Repo, k.Branch, err)
			}
			if !e.fetched {
				e.fetched = true
				close(e.ready)
			}
			if e.again {
				e.again = false
				continue
			}
			e.fetching = false
			t.mu.Unlock()
			return
		}
	}()
}

func samePR(a, b *pullRequest) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func (t *prTracker) signalLocked() {
	close(t.changed)
	t.changed = make(chan struct{})
}

// changes returns a channel that is closed when a pull request
// changes. It returns nil, which blocks for all time, if t is nil.
func (t *prTracker) changes() <-chan struct{} {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.changed
}

// prQuery identifies the branch of a checkout.
type prQuery struct {
	Dir    string
	Branch string
}

// lookup returns the pull request of each query, or nil for a query
// that has no pull request. For a query that it did not fetch before,
// it waits for the fetch until ctx is done. It does not wait more
// than prLookupWait.
func (t *prTracker) lookup(ctx context.Context, queries []prQuery) []*pullRequest {
	out := make([]*pullRequest, len(queries))
	if t == nil {
		return out
	}
	ctx, cancel := context.WithTimeout(ctx, prLookupWait)
	defer cancel()

	keys := make([]prKey, len(queries))
	for i, q := range queries {
		keys[i], _ = t.key(q)
	}

	t.mu.Lock()
	now := time.Now()
	waits := make([]chan struct{}, len(queries))
	for i, k := range keys {
		if k == (prKey{}) || (t.gitPoll <= 0 && t.viaGitLocked(k.Repo)) {
			continue
		}
		e, ok := t.entries[k]
		if !ok {
			e = &prEntry{
				ready: make(chan struct{}),
				dir:   queries[i].Dir,
			}
			t.entries[k] = e
			t.startLocked(k, e)
		}
		e.used = now
		e.dir = queries[i].Dir
		waits[i] = e.ready
	}
	t.mu.Unlock()

	for _, w := range waits {
		if w == nil {
			continue
		}
		select {
		case <-w:
		case <-ctx.Done():
		}
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	for i, k := range keys {
		if e, ok := t.entries[k]; ok && e.pr != nil {
			pr := *e.pr
			out[i] = &pr
		}
	}
	return out
}

// lookupOne returns the pull request of the branch of the checkout at
// dir, or nil.
func (t *prTracker) lookupOne(ctx context.Context, dir, branch string) *pullRequest {
	return t.lookup(ctx, []prQuery{{
		Dir:    dir,
		Branch: branch,
	}})[0]
}

// key returns the key of q. It returns false if q can have no pull
// request: the branch is a main branch or a detached HEAD, or the
// checkout has no GitHub remote.
func (t *prTracker) key(q prQuery) (prKey, bool) {
	switch q.Branch {
	case "", "HEAD", "unknown":
		return prKey{}, false
	}
	if q.Dir == "" || isMainBranchName(q.Branch) {
		return prKey{}, false
	}
	t.mu.Lock()
	repo, ok := t.repos[q.Dir]
	t.mu.Unlock()
	if !ok {
		repo = checkoutGitHubRepo(q.Dir)
		t.mu.Lock()
		t.repos[q.Dir] = repo
		t.mu.Unlock()
	}
	if repo == "" {
		return prKey{}, false
	}
	return prKey{
		Repo:   repo,
		Branch: q.Branch,
	}, true
}

func isMainBranchName(branch string) bool {
	for _, name := range repostats.MainBranchNames {
		if branch == name {
			return true
		}
	}
	return false
}

// checkoutGitHubRepo returns the "owner/name" of the GitHub repo of the
// default remote of the checkout at dir, or "" if it has none.
func checkoutGitHubRepo(dir string) string {
	remote, ok := defaultRemote(dir)
	if !ok {
		return ""
	}
	out, err := exec.Command("git", "-C", dir, "remote", "get-url", remote).Output()
	if err != nil {
		return ""
	}
	repo, err := gitHubRepo(strings.TrimSpace(string(out)))
	if err != nil {
		return ""
	}
	return repo
}

// fetchEntry gets the pull request of k, from maintner if it watches
// the repo, or else from git. dir is the checkout of the branch.
func (t *prTracker) fetchEntry(ctx context.Context, k prKey, dir string) (*pullRequest, error) {
	t.mu.Lock()
	viaGit := t.viaGitLocked(k.Repo)
	t.mu.Unlock()
	if !viaGit {
		pr, err := t.fetch(ctx, k)
		if !errors.Is(err, errUntrackedRepo) {
			return pr, err
		}
		t.mu.Lock()
		t.untracked[k.Repo] = true
		t.mu.Unlock()
	}
	if t.gitPoll <= 0 {
		return nil, nil
	}
	found, err := t.fetchGit(ctx, k.Repo, []gitQuery{{
		Branch: k.Branch,
		Dir:    dir,
	}})
	if err != nil {
		return nil, err
	}
	return found[0], nil
}

// fetch gets the pull request of k from maintner. If the branch has a
// number of pull requests, it uses the newest open one, or else the
// newest one. It returns nil and no error if the branch has no pull
// request.
func (t *prTracker) fetch(ctx context.Context, k prKey) (*pullRequest, error) {
	var list struct {
		PRs []struct {
			Number int32  `json:"number"`
			State  string `json:"state"`
		} `json:"prs"`
	}
	err := t.call(ctx, "ListPRs", map[string]any{
		"repo":  k.Repo,
		"head":  k.Branch,
		"limit": 10,
	}, &list)
	if err != nil {
		return nil, err
	}
	if len(list.PRs) == 0 {
		return nil, nil
	}
	// The list has the newest first.
	number := list.PRs[0].Number
	for _, p := range list.PRs {
		if strings.EqualFold(p.State, "open") {
			number = p.Number
			break
		}
	}

	var got struct {
		Number         int32  `json:"number"`
		Title          string `json:"title"`
		State          string `json:"state"`
		Draft          bool   `json:"isDraft"`
		ReviewDecision string `json:"reviewDecision"`
	}
	err = t.call(ctx, "GetPR", map[string]any{
		"repo":   k.Repo,
		"number": number,
	}, &got)
	if err != nil {
		return nil, err
	}
	return &pullRequest{
		Repo:             k.Repo,
		Number:           got.Number,
		Title:            got.Title,
		State:            strings.ToUpper(got.State),
		Draft:            got.Draft,
		Approved:         got.ReviewDecision == "APPROVED",
		ChangesRequested: got.ReviewDecision == "CHANGES_REQUESTED",
	}, nil
}

// call does one request to the ghapi endpoint of maintner.
func (t *prTracker) call(ctx context.Context, method string, params, dst any) error {
	body, err := json.Marshal(map[string]any{
		"method": method,
		"params": params,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.apiURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := t.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 16<<20))
	if err != nil {
		return err
	}
	if res.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			if strings.Contains(e.Error, "not tracked") {
				return fmt.Errorf("%w: %s", errUntrackedRepo, e.Error)
			}
			return fmt.Errorf("%s: %s", method, e.Error)
		}
		return fmt.Errorf("%s: %s", method, res.Status)
	}
	return json.Unmarshal(data, dst)
}

// prBadgeHTML returns a link to the GitHub page of pr, or "" if pr is
// nil. A green tick shows that the pull request is approved, and a
// yellow circle shows that it is not. A grey ring shows that the review
// state is not known. withRepo adds the name of the
// repo to the label, for a line that shows pull requests of more than
// one repo.
func prBadgeHTML(pr *pullRequest, withRepo bool) string {
	if pr == nil {
		return ""
	}
	cls := "pr-badge"
	mark := "●"
	review := "not approved"
	switch {
	case pr.ReviewUnknown:
		cls += " pr-unknown"
		mark = "○"
		review = "review state not known"
	case pr.Approved:
		cls += " pr-approved"
		mark = "✓"
		review = "approved"
	case pr.ChangesRequested:
		cls += " pr-pending"
		review = "changes requested"
	default:
		cls += " pr-pending"
	}
	state := ""
	switch {
	case pr.State == "MERGED":
		state = "merged"
	case pr.State == "CLOSED":
		state = "closed"
	case pr.Draft:
		state = "draft"
	}
	label := "#" + strconv.Itoa(int(pr.Number))
	if withRepo {
		_, name, _ := strings.Cut(pr.Repo, "/")
		label = name + label
	}
	title := pr.Repo + "#" + strconv.Itoa(int(pr.Number))
	if pr.Title != "" {
		title += ": " + pr.Title
	}
	title += " (" + review
	if state != "" {
		title += ", " + state
	}
	title += ")"

	var b strings.Builder
	fmt.Fprintf(&b, `<a class="%s" href="%s" target="_blank" rel="noopener" title="%s"><span class="pr-mark" aria-label="%s">%s</span>%s`,
		cls,
		template.HTMLEscapeString(pr.URL()),
		template.HTMLEscapeString(title),
		review,
		mark,
		template.HTMLEscapeString(label))
	if state != "" {
		fmt.Fprintf(&b, `<span class="pr-state">%s</span>`, state)
	}
	b.WriteString(`</a>`)
	return b.String()
}
