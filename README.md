# webdiff

A mobile-friendly web UI served over Tailscale for reviewing git diffs directly on your LLM sandbox, with an integrated Claude code-review workflow. It has 3 main views:

* Home page listing git repos and tmux sessions
* Diff page showing git diff output, with the ability to add comments and send them to claude
* Terminal page showing a stream of the claude session for a particular repo, with some minimal controls for interactivity

It uses tmux to persist long-lived sessions per repo, starting a new session on-demand if none exists.

Webdiff doesn't try to replace your existing tools. If you need the full terminal experience, just attach to the tmux session in your terminal via `tmux a -t <name>-wd`, where name is the repo's directory name, or for a managed worktree the `<repo>-<worktree>` slug from its URL. And if you need the full IDE experience you can still use your regular remote SSH workflows. In either case, webdiff seamlessly picks up the latest state when you come back to it.

## Opening a checkout in Zed, VS Code, or Ghostty

Every repo and worktree — on the home page rows and in the diff page toolbar —
carries three links that jump straight from reviewing to working, marked with
the Zed, VS Code, and Ghostty icons: the first two open the checkout in their
respective editor, and the third opens a Ghostty window with a shell in it. All
target the checkout *on this host* over SSH, so they need to know how your
desktop reaches it:

```
webdiff -ssh-host=<alias> [port]
```

`<alias>` is whatever you'd type after `ssh` — usually an `~/.ssh/config` entry.
It defaults to this node's MagicDNS name, which is right when you SSH by
hostname and wrong the moment your config needs a different user, port or key.
`deploy/install.sh` takes it as a second argument, after the code dir.

Zed and VS Code need nothing extra installed: they register `zed://` and
`vscode://` themselves, and the links use their SSH remote URL forms. Ghostty
has no equivalent, so the terminal link goes via a `webdiff://` handler you
register once, on the machine running the browser:

```
deploy/install-url-handler.sh <alias>
```

Pass the same alias. The handler pins it and rejects URLs naming any other
host — once a scheme is registered, any page your browser loads can feed it a
URL, so it treats its input as hostile.

All three links are hidden below 900px: their schemes do not resolve on iOS.

The three marks are traced from the apps' own icons into `assets/icons.svg` as a
`<symbol>` sprite, inlined once per page and referenced with `<use>`. They're a
few KB of path data each, which is too much to repeat on every home page row.

## Selecting changes

The diff page's **changes** menu lists the working tree and first-parent commits
since the merge base with remote main (or local main when no remote main exists).
A merge commit is one change relative to its first parent. Select any combination,
then choose **apply**. **All** and **none** select or clear the complete list.
All changes are selected by default. Picker choices are remembered in this browser
for each repository or worktree separately. New commits start selected. Selections
also stay in the page URL, including an explicitly empty selection; an explicit
URL selection takes precedence over saved choices. The old diff `mode` parameter
is no longer used.

The combined diff starts at the parent of the oldest selected commit and applies
only selected changes in chronological order, with working-tree changes last.
Working-tree changes include staged, unstaged, and untracked files. If omitted
commits are required to apply a selected change, the page reports a conflict;
select the prerequisite commits to resolve it. The real index is not changed.

The home page shows totals for all branch and working-tree changes, relative to
the same merge base as the default diff. These totals and the working-tree index
are cached. Saved picker choices do not change the home-page totals. Arbitrary
combinations are computed on request in temporary indexes and are not retained
or precomputed.

## Managed worktrees

Webdiff can create git worktrees for you (`POST /api/worktrees/create` with
`{"repo":..., "branch":...}`), which is how you get a second checkout of a repo
under review without leaving the UI. They're checked out at
`<code-dir>/wt/<repo>/<worktree>` — beside your repos rather than in a cache
dir, since they hold real uncommitted work — and addressed everywhere else by
the flat slug `<repo>-<worktree>`, e.g. `/worktrees/corp-fix-thing/` with a
`corp-fix-thing-wd` tmux session. The worktree name is the branch name with any
`<owner>/` prefix dropped.

The **+ worktree** button is also available on worktree diff pages. It defaults
to the source worktree's HEAD, without copying uncommitted changes; **from main**
instead starts a new branch from main. Child worktrees stay under the original
repo's directory, not under their source worktree. An existing local or remote
branch is checked out as before. API callers can select a source worktree with
`{"kind":"worktrees", "repo":"<repo>-<worktree>", "branch":"..."}`;
`kind` defaults to `repos` for existing callers, and `fromMain` is optional.

## Install

```
go install github.com/tomhjp/webdiff@latest
```

Then run from a directory containing your git repos:

```
webdiff [-agent="<command>"] [-branch-prefix="<prefix>"] [port]
```

`-agent` accepts a full command line (e.g. `-agent="opencode --foo"`); the first whitespace-separated token is used as the display name on the diff page's agent button. Defaults to `claude`.

`-branch-prefix` prepopulates the branch field in the worktree creation form. The home page and managed worktree directory names omit that prefix. It defaults to empty.

The `-agent` flag sets the *default* agent. Individual sessions can run something else: the diff page's spawn button has an agent drop-down beside it, and the stream page's "restart session" modal picks which agent the fresh session runs. The choice travels as `?agent=<id>`, so reloading the stream page still shows which agent is running. Selectable ids are `claude` and `pi`, plus whatever `-agent` names.

Alternatively, get a more opinionated install from `deploy/install.sh` which:

* Installs and starts a user systemd unit
* Serves webdiff's own parent directory as the code directory
* Enables linger

## Future designs

* [Agent transcript views](TRANSCRIPT-PLAN.md): agent-specific transcript adapters
  and incremental source watching, with tmux retained for interaction and fallback.
* [Zellij migration analysis](ZELLIJ-PLAN.md): an assessment of event-driven
  terminal capture and multiplexer trade-offs.

## Terminal colour palette

By default webdiff uses a built-in colour scheme. To match your terminal's actual palette:

```
webdiff palette > ~/.config/webdiff/palette.css
```

Run this outside tmux/screen — multiplexers strip the OSC escape sequences the command uses to query colour values from the terminal.
