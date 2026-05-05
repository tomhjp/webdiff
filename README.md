# webdiff

A mobile-friendly web UI served over Tailscale for reviewing git diffs directly on your LLM sandbox, with an integrated Claude code-review workflow. It has 3 main views:

* Home page listing git repos and tmux sessions
* Diff page showing git diff output, with the ability to add comments and send them to claude
* Terminal page showing a stream of the claude session for a particular repo, with some minimal controls for interactivity

It uses tmux to persist long-lived sessions per repo, starting a new session on-demand if none exists.

Webdiff doesn't try to replace your existing tools. If you need the full terminal experience, just attach to the tmux session in your terminal via `tmux a -t <repo-dir>-wd`, where repo-dir is the repo's directory name. And if you need the full IDE experience you can still use your regular remote SSH workflows. In either case, webdiff seamlessly picks up the latest state when you come back to it.

## Install

```
go install github.com/tomhjp/webdiff@latest
```

Then run from a directory containing your git repos:

```
webdiff [-agent="<command>"] [port]
```

`-agent` accepts a full command line (e.g. `-agent="opencode --foo"`); the first whitespace-separated token is used as the display name on the diff page's agent button. Defaults to `claude`.

## Terminal colour palette

By default webdiff uses a built-in colour scheme. To match your terminal's actual palette:

```
webdiff palette > ~/.config/webdiff/palette.css
```

Run this outside tmux/screen — multiplexers strip the OSC escape sequences the command uses to query colour values from the terminal.
