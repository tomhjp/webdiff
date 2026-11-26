# webdiff

A mobile-friendly web UI served over Tailscale for reviewing git diffs directly on your LLM sandbox, with an integrated Claude code-review workflow. It has 3 main views:

* Home page listing git repos and agent sessions
* Diff page showing git diff output, with the ability to add comments and send them to claude
* Terminal page showing a stream of the claude session for a particular repo, with some minimal controls for interactivity

It uses [Rex](https://www.superlogical.com/rex/docs) to persist long-lived sessions per repo, starting a new session on-demand if none exists. Each session is named `<name>-wd`, where name is the repo's directory name, or for a managed worktree the `<repo>-<worktree>` slug from its URL. Agents tell webdiff when a turn ends: Pi reports its state with OSC 7501, and Claude Code shows it in its terminal title.

## Multiple agents per checkout

A repo or worktree can have up to 20 agent sessions at the same time. The **+ agent** button starts one more session with the lowest free number. It is on the home page row of each repo that is not developed on main, and in the **more** menu of the diff and stream pages. Session 1 keeps the name `<name>-wd`. Session n is named `<name>-wd<n>`, for example `corp-fix-thing-wd2`.

Pages select a session with the `?n=<n>` query parameter, and use session 1 when there is no parameter. For example, `/stream/repos/corp/?n=2` shows session 2. The stream page of session n links to the diff page with the same `?n=`, so the diff page's agent button and **send** go to that session. The home page shows every session of a checkout below it, with its number after the agent name, for example "pi #2 · ~/ai/corp". The **more** menu of the diff and stream pages links to the stream pages of the other running sessions of the checkout. **restart session** starts the same session number again, history **resume** uses the number of the archived session, and a worktree rename or remove changes all the sessions of the worktree.

Run the Rex server as a user service, so that sessions don't depend on webdiff:

```
systemctl --user enable --now rex.socket
```

`-terminal=tmux` starts new sessions in tmux instead. Sessions that already run in tmux keep working with either setting until they end.

Webdiff doesn't try to replace your existing tools. If you need the full terminal experience, open the session in the Rex app (or `tmux a -t <name>-wd` for a tmux session). And if you need the full IDE experience you can still use your regular remote SSH workflows. In either case, webdiff seamlessly picks up the latest state when you come back to it.

## Opening a checkout in Zed, VS Code, or Ghostty

Every repo and worktree — on the home page rows and in the diff page toolbar —
carries three links that jump straight from reviewing to working, marked with
the Zed, VS Code, and Ghostty icons: the first two open the checkout in their
respective editor, and the third opens a Ghostty window with a shell in it.

On the diff page, the heading of each changed file also has Zed and VS Code
links that open that file at its first change. A deep link can name only one
path, so these links can't open the checkout as well. If a window for the
checkout is already open, the file opens in that window. If not, the editor
opens the file by itself.

All the links target the checkout *on this host* over SSH, so they need to
know how your desktop reaches it:

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

The diff page's **changes** menu lists the first-parent commits since the merge
base with remote main (or local main when no remote main exists), oldest first,
and then the working tree.
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

The diff page shows the full messages of the selected commits, oldest first, in
a **commit messages** section before the files. The text of each commit in the
**changes** menu, and the id of each commit in that section, links to the commit
view, `?commit=<sha>`. The commit view shows only that commit: its message and
its diff. It does not change the saved picker choices, and **apply** goes back
to the normal diff page.

Each message in the **commit messages** section has an **edit** button. Save
changes the message of the commit, and makes the commits after it again on top
of it, with the same trees and messages. The index and the working tree do not
change. When the working tree is selected and has changes, it has an entry in
the section too: its edit button commits all of its changes, including files
that are not tracked, with the message and `git commit -s`.

The home page shows totals for all branch and working-tree changes, relative to
the same merge base as the default diff. It shows these totals only for worktrees
and for the repos that `-develop-on-main` names. These totals and the working-tree index
are cached. Saved picker choices do not change the home-page totals. Arbitrary
combinations are computed on request in temporary indexes and are not retained
or precomputed.

## Desktop client

`webdiff client` runs on your laptop. It is a reverse proxy in front of the
webdiff on the sandbox, and it adds the controls that need the credentials of
the laptop. All other pages work the same as on the sandbox.

```
webdiff client [-hostname=webdiff] [-listen=<addr>,...] [-github-url='git@github.com:{repo}.git'] [<sandbox-host[:port]>]
```

With no host, the client finds your LLM sandbox VM. With no port, it uses port
80 of the sandbox. By default the client listens on port 80 of each Tailscale IP
of the laptop. These are not the wildcard address, so on macOS the client needs
no privileges for port 80. Add an `/etc/hosts` entry for the Tailscale IPv4
address of the laptop (`tailscale ip -4`), and then open http://webdiff/:

```
100.x.y.z webdiff
```

The client accepts only connections from the laptop itself and only the host
names that it listens on, and `/_local/` accepts only same-origin JSON
requests.

Through the client, the diff page has a **send to github** button. It asks for
the name of the branch on GitHub, with the current branch as the default. The
client then fetches the commits of the branch, up to HEAD, from the sandbox,
pushes them to that branch on GitHub, and opens the GitHub page that compares
the branch with main. It replaces the branch on GitHub if it is there already.
It does not send the working tree; to send those changes, commit them first
(see [Selecting changes](#selecting-changes)). If HEAD changed after the page
loaded, the send stops. The client pushes from a bare repo at
`<user cache dir>/webdiff/send/<owner>/<repo>.git`, not from your checkouts.
The repo stays between sends, so each send transfers only the new objects. It
is safe to delete. The GitHub repo is the last two segments of the path of the
sandbox remote URL, so a GitHub mirror or proxy also works. `-github-url` sets
the URL to push to.

## Managed worktrees

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

## Install

```
go install github.com/tomhjp/webdiff@latest
```

Then run from a directory containing your git repos:

```
webdiff [-agent="<command>"] [-branch-prefix="<prefix>"] [-develop-on-main="<repo>,..."] [port]
```

`-agent` accepts a full command line (e.g. `-agent="opencode --foo"`); the first whitespace-separated token is used as the display name on the diff page's agent button. Defaults to `claude`.

`-branch-prefix` prepopulates the branch field in the worktree creation form. The home page and managed worktree directory names omit that prefix. It defaults to empty.

`-develop-on-main` names the repos that you develop directly on main. It defaults to empty. The home page shows these repos above the repos section, with their branch and diff stats. Do the work for all other repos in managed worktrees. In the repos section, each of the other repos has a row with inline **+ worktree** and **+ agent** buttons, and below it one row for each agent session. **+ worktree** creates a worktree, from main by default, and opens its agent page. **+ agent** starts a new session in the repo, beside any sessions that already run (see [Multiple agents per checkout](#multiple-agents-per-checkout)). The agent page links to the diff and files.

Each agent session is labelled with its agent and its working directory from `~`, for example "pi · ~/ai/wt/corp/safer-ciguestlet-shutdown". The home page shows the label on the agent row. The Rex apps show it as the agent window label, which webdiff sets when it starts the session and again when a worktree rename moves the directory.

Once a day, webdiff runs `git pull --ff-only` in each repo under the code directory, so that the repos stay near their latest main. It records the time of each attempt in `~/.cache/webdiff/pull.json`, so a restart does not start a new pull. A repo with no upstream branch, or one that cannot fast-forward, stays as it is, and the journal shows why.

The `-agent` flag sets the *default* agent. Individual sessions can run something else: the diff page's spawn button has an agent drop-down beside it, and the stream page's "restart session" modal picks which agent the fresh session runs. The choice travels as `?agent=<id>`, so reloading the stream page still shows which agent is running. Selectable ids are `claude` and `pi`, plus whatever `-agent` names.

Alternatively, get a more opinionated install from `deploy/install.sh` which:

* Installs and starts a user systemd unit
* Serves webdiff's own parent directory as the code directory
* Enables linger

## Terminal colour palette

By default webdiff uses a built-in colour scheme. To match your terminal's actual palette:

```
webdiff palette > ~/.config/webdiff/palette.css
```

Run this outside tmux/screen — multiplexers strip the OSC escape sequences the command uses to query colour values from the terminal.
