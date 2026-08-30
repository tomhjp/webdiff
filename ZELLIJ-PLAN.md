# Zellij migration: sketch + net code estimate

Status: **analysis only, not recommended as-is.** See "Verdict".

## What the API actually gives us

Zellij 0.44.0 added the piece that matters:

```
zellij subscribe --pane-id terminal_1 --format json --ansi
```

NDJSON on stdout, change-driven (no polling):

```json
{"event":"pane_update","pane_id":"terminal_1","viewport":["line1","line2"],"scrollback":null,"is_initial":true}
{"event":"pane_closed","pane_id":"terminal_1"}
```

Also relevant: `action dump-screen --pane-id --full --ansi`, `action paste
--pane-id`, `action send-keys --pane-id`, `action list-panes --json`,
`attach --create-background`, `action new-pane` (returns the pane id).

## Mapping the current tmux surface

26 `tmuxOut` call sites across claude.go (23), history.go (2), webdiff.go (1).

| Concern | Today | Zellij | Verdict |
|---|---|---|---|
| Live tail | `capture-pane` @100 ms + `streamDelta` | `subscribe` NDJSON | **Win** |
| Scrollback archive | `capture-pane -p -e -S -` | `dump-screen --full --ansi` | Even |
| Send review comments | `load-buffer`(stdin)→`paste-buffer -p`→`Enter` | `action paste` (argv) | **Regression** — see R1 |
| Single keypress | `send-keys -t <pane> <key>` | `action send-keys --pane-id` | Even |
| Spawn session | `new-session`+`set-option`+`new-window`+`kill-window`+`move-window` | `attach --create-background` + `new-pane` | **Win** — see W2 |
| Session exists? | `has-session -t =name` | parse `list-sessions` text | Slight regression |
| Session created-at | `#{session_created}` → unix secs | humanized string only | **Blocker** — see R2 |
| List all panes | `list-panes -a -F <fmt>` | `list-panes --json` | Even/slight win |
| Idle/spin detection | 1 Hz `capture-pane` diff over all sessions | `subscribe` all panes | **Win** — see W3 |
| Kill session | `kill-session` | `kill-session` + `delete-session` | Even |
| History limit | `set-option history-limit 100000` | `scroll_buffer_size` in config.kdl | Regression — global file, not per-session |

### W2 — session spawn gets genuinely cleaner

`ensureRepoSession` (claude.go:1074-1158, 93 lines) burns five tmux calls on a
workaround: history-limit is fixed at window-creation time, so it spawns a
throwaway bash window, sets the option, opens the real agent window, kills the
throwaway, then moves the agent back to `:0.0`. Zellij's `new-pane` returns the
pane id directly and scrollback size is config-level, so that whole dance
collapses to two calls. **~25 lines saved.**

The 40-line readiness poll (`pane_current_command` == agent name + content
stable 300 ms) still has to stay — it's about the *agent TUI* being ready for
bracketed paste, which no multiplexer knows about.

### W3 — the watcher gets much cheaper

`pollWdSessions` (claude.go:160-245, 104 lines) runs `list-sessions` plus one
`capture-pane` *per session* every second, forever, whether or not anything
changed. With `subscribe` on all `-wd` panes, "content changed" is an event
rather than a diff we compute. The turn-event/spinner state machine and history
archiving stay; the polling scaffolding and `wdLastRaw` bookkeeping go.
**~30 lines saved**, and the idle-CPU win is the real prize.

### R1 — `sendToPane` regresses, and this one bites

The current implementation (claude.go:1326-1348) routes through `tmux
load-buffer` reading the prompt from **stdin**. The comment says why in plain
terms: the earlier `send-keys` path could exceed `MAX_ARG_STRLEN` (~128 KB) and
leave the pane wedged in half-open paste mode, silently swallowing every
subsequent keystroke.

`zellij action paste` takes the text as an **argv argument**. I found no
documented stdin flag. A long multi-file review — exactly webdiff's core use
case — walks straight back into the bug this code was written to fix. Chunking
across multiple `paste` calls reintroduces interleaving risk and is strictly
more code than today. **Net: +10-20 lines and a worse failure mode.**

### R2 — history IDs lose their key

`history.go` keys every archived session on `<created>-<session>`, where
`created` is tmux's `#{session_created}` as unix seconds (history.go:49, :197).
It's the stable identity that survives restarts and orders the history list.

Zellij's `list-sessions` prints creation time as a *humanized relative string*
("Created 20days 23h 21m 4s ago"). No `--json`; `--no-formatting` only strips
ANSI, `--short` gives bare names. Reconstructing absolute unix seconds by
parsing that and subtracting from now is lossy and drifts — unusable as a
primary key.

Workaround: mint our own creation timestamp on spawn and persist it. That's a
new sidecar store plus migration for existing archives. **+30-40 lines**, and
it moves a fact the multiplexer owns into state we now have to keep correct.

## Net accounting

| Change | Lines |
|---|---|
| Delete `handlePaneStream` poll/throttle/render loop | −110 |
| Delete `streamDelta` + its test | −30 |
| Add `subscribe` subprocess supervisor (spawn, NDJSON scan, reconnect, teardown) | +70 |
| Simplify `ensureRepoSession` spawn dance | −25 |
| Simplify `pollWdSessions` to event-driven | −30 |
| Rework `sendToPane` chunking around argv limit | +15 |
| Session-creation-time sidecar store + migration | +35 |
| `sessionExists` / created-at via text parsing | +15 |
| Adapt `listAllPanes` to `--json` | ~0 |
| Client JS: `applyPatch` for whole-viewport replaces | +10 |
| **Net** | **≈ −50 lines** |

Call it **50-100 lines net reduction on ~7,500** — roughly 1%. Not the order of
magnitude the framing assumed.

The reason the win is small: `subscribe` sends **the full viewport on every
change**, not a line diff. So `streamDelta` doesn't actually disappear — it
gets *renamed*. We still have to diff consecutive viewports ourselves to avoid
pushing the whole screen down the wire on every cursor blink, which is the
exact mobile-bandwidth problem `streamDelta` exists to solve. We trade a
100 ms poll for a push, and keep the diffing.

## Steps, if we did it anyway

1. `zellij.go`: `zjOut()` mirroring `tmuxOut`, plus `zjSubscribe()` returning a
   channel of `paneUpdate`. Keep `tmuxOut` alongside — no big-bang switch.
2. Put the multiplexer behind an interface (`ensureSession`, `capture`,
   `send`, `sendKey`, `kill`, `subscribe`) with tmux and zellij impls, selected
   by a `-mux` flag. This is +40 lines not in the table above; without it
   there's no incremental path and no way back.
3. Move the stream endpoint onto `subscribe`, keeping the existing
   `{reset,drop,append}` wire protocol so `app.js` is untouched at first.
4. Migrate the watcher to events.
5. Migrate history capture + the created-at sidecar.
6. Cut over `sendToPane` last — highest risk, least benefit.

## Verdict

Don't do this now. The clean-API intuition is right about `subscribe` and about
session spawn, but three things blunt it:

- **The renderer question is untouched.** Zellij's own web client is xterm.js,
  which the mobile-first constraint already vetoed. This plan deliberately
  keeps our DOM renderer, so the multiplexer swap buys nothing on that axis.
- **Full-viewport pushes mean we keep the diffing.** The single biggest deletion
  candidate survives under another name.
- **Two hard regressions** (argv-limit paste, non-machine-readable creation
  time) trade robust code for workarounds — against a stable tmux 3.4 that's
  already installed and that every one of these 26 call sites already speaks.

The latency complaint that motivates this is better served by the existing
stream-refactor plan: per-row `set` patches over WebSocket. That's the actual
fix, is independent of the multiplexer, and doesn't put a young CLI surface
under the whole app.

**Revisit if:** 10 Hz `capture-pane` shows up as a measurable CPU hot-spot
(then `subscribe` beats the FIFO/`pipe-pane` workaround cleanly), or zellij
ships stdin-capable `paste` and machine-readable `list-sessions`.
