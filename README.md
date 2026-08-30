# webdiff

A mobile-friendly web UI served over Tailscale for reviewing git diffs directly on your LLM sandbox, with an integrated Claude code-review workflow. It has 3 main views:

* Home page listing git repos and tmux sessions
* Diff page showing git diff output, with the ability to add comments and send them to claude
* Terminal page showing a stream of the claude session for a particular repo, with some minimal controls for interactivity

It uses tmux to persist long-lived sessions per repo, starting a new session on-demand if none exists.

Webdiff doesn't try to replace your existing tools. If you need the full terminal experience, just attach to the tmux session in your terminal via `tmux a -t <name>-wd`, where name is the repo's directory name, or for a managed worktree the `<repo>-<worktree>` slug from its URL. And if you need the full IDE experience you can still use your regular remote SSH workflows. In either case, webdiff seamlessly picks up the latest state when you come back to it.

## Opening a checkout in Zed or Ghostty

Every repo and worktree — on the home page rows and in the diff page toolbar —
carries two links that jump straight from reviewing to working, marked with the
Zed and Ghostty icons: one opens the checkout in Zed, the other opens a Ghostty
window with a shell in it. Both target the checkout *on this host* over SSH, so
they need to know how your desktop reaches it:

```
webdiff -ssh-host=<alias> [port]
```

`<alias>` is whatever you'd type after `ssh` — usually an `~/.ssh/config` entry.
It defaults to this node's MagicDNS name, which is right when you SSH by
hostname and wrong the moment your config needs a different user, port or key.
`deploy/install.sh` takes it as a second argument, after the code dir.

Zed needs nothing installed: it registers `zed://` itself, and the link uses its
documented `zed://ssh/<host>/<path>` form. Ghostty has no equivalent, so the
terminal link goes via a `webdiff://` handler you register once, on the machine
running the browser:

```
deploy/install-url-handler.sh <alias>
```

Pass the same alias. The handler pins it and rejects URLs naming any other
host — once a scheme is registered, any page your browser loads can feed it a
URL, so it treats its input as hostile.

Both links are hidden below 900px: neither scheme resolves on iOS.

The two marks are traced from the apps' own icons into `assets/icons.svg` as a
`<symbol>` sprite, inlined once per page and referenced with `<use>`. They're a
few KB of path data each, which is too much to repeat on every home page row.

## Managed worktrees

Webdiff can create git worktrees for you (`POST /api/worktrees/create` with
`{"repo":..., "branch":...}`), which is how you get a second checkout of a repo
under review without leaving the UI. They're checked out at
`<code-dir>/wt/<repo>/<worktree>` — beside your repos rather than in a cache
dir, since they hold real uncommitted work — and addressed everywhere else by
the flat slug `<repo>-<worktree>`, e.g. `/worktrees/corp-fix-thing/` with a
`corp-fix-thing-wd` tmux session. The worktree name is the branch name with any
`<owner>/` prefix dropped.

## Install

```
go install github.com/tomhjp/webdiff@latest
```

Then run from a directory containing your git repos:

```
webdiff [-agent="<command>"] [port]
```

`-agent` accepts a full command line (e.g. `-agent="opencode --foo"`); the first whitespace-separated token is used as the display name on the diff page's agent button. Defaults to `claude`.

That flag sets the *default* agent. Individual sessions can run something else: the diff page's spawn button has an agent drop-down beside it, and the stream page's "restart session" modal picks which agent the fresh session runs. The choice travels as `?agent=<id>`, so reloading the stream page still shows which agent is running. Selectable ids are `claude` and `pi`, plus whatever `-agent` names.

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
