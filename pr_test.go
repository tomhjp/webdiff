package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/bradfitz/gxbm/maintner/maintpb"
	"github.com/tomhjp/webdiff/internal/repostats"
)

// fakeGHAPI is a fake of the ghapi endpoint of maintner.
type fakeGHAPI struct {
	mu sync.Mutex
	// prs holds the pull requests of each repo.
	prs   map[string][]fakePR
	calls int
}

type fakePR struct {
	Number   int32
	Head     string
	State    string
	Approved bool
}

func (f *fakeGHAPI) RoundTrip(req *http.Request) (*http.Response, error) {
	w := httptest.NewRecorder()
	f.serve(w, req)
	return w.Result(), nil
}

func (f *fakeGHAPI) serve(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Method string `json:"method"`
		Params struct {
			Repo   string `json:"repo"`
			Head   string `json:"head"`
			Number int32  `json:"number"`
		} `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	prs, ok := f.prs[req.Params.Repo]
	if !ok {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{
			"error": `repo "` + req.Params.Repo + `" not tracked by maintner`,
		})
		return
	}
	switch req.Method {
	case "ListPRs":
		type item struct {
			Number int32  `json:"number"`
			State  string `json:"state"`
		}
		var list []item
		// The newest first, as ghapi does.
		for i := len(prs) - 1; i >= 0; i-- {
			if prs[i].Head == req.Params.Head {
				list = append(list, item{
					Number: prs[i].Number,
					State:  prs[i].State,
				})
			}
		}
		json.NewEncoder(w).Encode(map[string]any{
			"total": len(list),
			"prs":   list,
		})
	case "GetPR":
		for _, p := range prs {
			if p.Number != req.Params.Number {
				continue
			}
			decision := ""
			if p.Approved {
				decision = "APPROVED"
			}
			json.NewEncoder(w).Encode(map[string]any{
				"number":         p.Number,
				"title":          "title of " + p.Head,
				"state":          p.State,
				"reviewDecision": decision,
			})
			return
		}
		http.Error(w, "no such PR", http.StatusNotFound)
	default:
		http.Error(w, "unknown method", http.StatusBadRequest)
	}
}

func (f *fakeGHAPI) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeGHAPI) update(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

func newTestPRTracker(f *fakeGHAPI) *prTracker {
	t := newPRTracker(prConfig{
		Maintner: "http://maintner.test",
	})
	t.client.Transport = f
	t.repos["/wt/a"] = "tailscale/corp"
	t.repos["/wt/b"] = "tailscale/corp"
	t.repos["/wt/c"] = "someone/else"
	return t
}

func issueMutation(repo string, number int32, head string) *maintpb.Mutation {
	owner, name, _ := strings.Cut(repo, "/")
	gi := &maintpb.GithubIssueMutation{
		Owner:  owner,
		Repo:   name,
		Number: number,
	}
	if head != "" {
		gi.Head = &maintpb.GithubPullRequestBranch{
			Ref: head,
		}
	}
	return &maintpb.Mutation{
		GithubIssue: gi,
	}
}

func TestPRTrackerLookup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakeGHAPI{
			prs: map[string][]fakePR{
				"tailscale/corp": {
					{Number: 1, Head: "tomhjp/a", State: "CLOSED"},
					{Number: 2, Head: "tomhjp/a", State: "OPEN"},
					{Number: 3, Head: "tomhjp/a", State: "MERGED", Approved: true},
				},
			},
		}
		tr := newTestPRTracker(f)

		got := tr.lookupOne(t.Context(), "/wt/a", "tomhjp/a")
		if got == nil || got.Number != 2 || got.Approved || got.State != "OPEN" {
			t.Fatalf("got %+v, want the open PR 2, not approved", got)
		}
		if got.URL() != "https://github.com/tailscale/corp/pull/2" {
			t.Errorf("URL = %q", got.URL())
		}

		// A branch with no PR, a main branch, and a repo that maintner
		// does not watch have no PR.
		for _, q := range []prQuery{
			{Dir: "/wt/b", Branch: "tomhjp/b"},
			{Dir: "/wt/a", Branch: "main"},
			{Dir: "/wt/c", Branch: "tomhjp/c"},
		} {
			if got := tr.lookupOne(t.Context(), q.Dir, q.Branch); got != nil {
				t.Errorf("lookup %+v = %+v, want nil", q, got)
			}
		}

		// The second lookup uses the cache, and the tracker does not
		// ask again about the repo that maintner does not watch.
		n := f.callCount()
		tr.lookupOne(t.Context(), "/wt/a", "tomhjp/a")
		tr.lookupOne(t.Context(), "/wt/c", "tomhjp/other")
		if got := f.callCount(); got != n {
			t.Errorf("calls = %d after cached lookups, want %d", got, n)
		}
	})
}

func TestPRTrackerMutations(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakeGHAPI{
			prs: map[string][]fakePR{
				"tailscale/corp": {
					{Number: 10, Head: "tomhjp/a", State: "OPEN"},
				},
			},
		}
		tr := newTestPRTracker(f)
		if got := tr.lookupOne(t.Context(), "/wt/a", "tomhjp/a"); got == nil || got.Approved {
			t.Fatalf("got %+v, want PR 10, not approved", got)
		}
		if got := tr.lookupOne(t.Context(), "/wt/b", "tomhjp/b"); got != nil {
			t.Fatalf("got %+v, want no PR", got)
		}

		// Mutations about other PRs and other repos do not cause a fetch.
		n := f.callCount()
		tr.handleMutation(issueMutation("tailscale/corp", 99, "tomhjp/zzz"))
		tr.handleMutation(issueMutation("tailscale/tailscale", 10, "tomhjp/a"))
		tr.handleMutation(&maintpb.Mutation{
			Git: &maintpb.GitMutation{},
		})
		time.Sleep(time.Minute)
		synctest.Wait()
		if got := f.callCount(); got != n {
			t.Fatalf("calls = %d after unrelated mutations, want %d", got, n)
		}

		// A review of PR 10 changes it. The mutations of a review have
		// no head.
		f.update(func() {
			f.prs["tailscale/corp"][0].Approved = true
		})
		changed := tr.changes()
		tr.handleMutation(issueMutation("tailscale/corp", 10, ""))
		tr.handleMutation(issueMutation("tailscale/corp", 10, ""))
		time.Sleep(prDebounce)
		synctest.Wait()
		select {
		case <-changed:
		default:
			t.Fatal("changes not signalled")
		}
		if got := tr.lookupOne(t.Context(), "/wt/a", "tomhjp/a"); got == nil || !got.Approved {
			t.Fatalf("got %+v, want PR 10, approved", got)
		}
		// One refresh for the two mutations: ListPRs and GetPR.
		if got := f.callCount(); got != n+2 {
			t.Errorf("calls = %d, want %d", got, n+2)
		}

		// A new PR with the branch as its head.
		f.update(func() {
			f.prs["tailscale/corp"] = append(f.prs["tailscale/corp"], fakePR{
				Number: 11,
				Head:   "tomhjp/b",
				State:  "OPEN",
			})
		})
		tr.handleMutation(issueMutation("tailscale/corp", 11, "tomhjp/b"))
		time.Sleep(prDebounce)
		synctest.Wait()
		if got := tr.lookupOne(t.Context(), "/wt/b", "tomhjp/b"); got == nil || got.Number != 11 {
			t.Fatalf("got %+v, want PR 11", got)
		}

		// After a failure of the tail, the next mutation refreshes all.
		tr.mu.Lock()
		tr.gap = true
		tr.mu.Unlock()
		n = f.callCount()
		tr.handleMutation(&maintpb.Mutation{
			Git: &maintpb.GitMutation{},
		})
		time.Sleep(prDebounce)
		synctest.Wait()
		if got := f.callCount(); got != n+4 {
			t.Errorf("calls = %d after the gap, want %d", got, n+4)
		}
	})
}

func TestPRBadgeHTML(t *testing.T) {
	pr := &pullRequest{
		Repo:   "tailscale/corp",
		Number: 42,
		Title:  "fix <thing>",
		State:  "OPEN",
	}
	if got := prBadgeHTML(nil, false); got != "" {
		t.Errorf("nil PR: got %q", got)
	}
	got := prBadgeHTML(pr, false)
	for _, want := range []string{
		`class="pr-badge pr-pending"`,
		`href="https://github.com/tailscale/corp/pull/42"`,
		`>●</span>#42</a>`,
		`fix &lt;thing&gt;`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("pending badge %q does not contain %q", got, want)
		}
	}

	pr.Approved = true
	pr.State = "MERGED"
	got = prBadgeHTML(pr, true)
	for _, want := range []string{
		`class="pr-badge pr-approved"`,
		`>✓</span>corp#42<span class="pr-state">merged</span></a>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("approved badge %q does not contain %q", got, want)
		}
	}
}

// The pull requests of a branch group show on the line of the branch.
// They name their repos only when the repos are not all the same.
func TestBranchGroupPRs(t *testing.T) {
	g := branchGroup{
		Title: "foo",
		PRs: []*pullRequest{
			{Repo: "tailscale/corp", Number: 1, State: "OPEN", Approved: true},
		},
	}
	var b strings.Builder
	writeBranchGroup(&b, g, func(repostats.Target) []homeSession { return nil })
	want := `<div class="branch-title"><span class="branch-title-name">foo</span><a class="pr-badge pr-approved"`
	if !strings.Contains(b.String(), want) {
		t.Fatalf("got %s, want it to contain %s", b.String(), want)
	}
	if !strings.Contains(b.String(), `</span>#1</a></div>`) {
		t.Errorf("one repo: got %s, want the label #1", b.String())
	}

	g.PRs = append(g.PRs, &pullRequest{
		Repo:   "tailscale/tailscale",
		Number: 2,
		State:  "OPEN",
	})
	b.Reset()
	writeBranchGroup(&b, g, func(repostats.Target) []homeSession { return nil })
	for _, want := range []string{`</span>corp#1</a>`, `</span>tailscale#2</a></div>`} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("two repos: got %s, want it to contain %s", b.String(), want)
		}
	}
}
