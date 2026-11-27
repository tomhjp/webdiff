# Managed worktrees

Webdiff can create git worktrees for you (`POST /api/worktrees/create` with
`{"repo":..., "branch":...}`), which is how you get a second checkout of a repo
under review without leaving the UI. They're checked out at
`<code-dir>/wt/<repo>/<worktree>` — beside your repos rather than in a cache
dir, since they hold real uncommitted work — and addressed everywhere else by
the flat slug `<repo>-<worktree>`, e.g. `/worktrees/corp-fix-thing/` with a
`corp-fix-thing-wd` agent session. The worktree name is the branch name with any
`<owner>/` prefix dropped.

The **+ worktree** button is also available on worktree diff pages. It defaults
to the source worktree's HEAD, without copying uncommitted changes; **from main**
instead starts a new branch from main. Child worktrees stay under the original
repo's directory, not under their source worktree. An existing local or remote
branch is checked out as before. API callers can select a source worktree with
`{"kind":"worktrees", "repo":"<repo>-<worktree>", "branch":"..."}`;
`kind` defaults to `repos` for existing callers, and `fromMain` is optional.
