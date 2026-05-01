# webdiff

A mobile-friendly web UI for reviewing git diffs, with an integrated Claude code-review workflow.

## Dependencies

- **Go 1.26+** — to build
- **git** — required
- **Tailscale** — required; the server binds to your Tailscale IP(s) and gates access by Tailscale identity
- **tmux** — optional; needed for the Claude session integration
- **delta** — optional; if configured as your `core.pager`, diffs are rendered through it for syntax highlighting and line numbers

## Install

```
go install github.com/tomhjp/webdiff@latest
```

Then run from a directory containing your git repos:

```
webdiff [port]
```

## Terminal colour palette

By default webdiff uses a built-in colour scheme. To match your terminal's actual palette:

```
webdiff palette > ~/.config/webdiff/palette.css
```

Run this outside tmux/screen — multiplexers strip the OSC escape sequences the command uses to query colour values from the terminal.
