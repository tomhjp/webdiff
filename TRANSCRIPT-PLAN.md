# Agent transcripts: future design

Status: **planned direction, not implemented.** Start with Pi, then add
agent-specific adapters for Claude Code, Codex, and OpenCode.

## Motivation

The conversation view should read the agent's saved transcript, not infer the
conversation from its terminal screen. Watch the transcript source for changes
and read new records incrementally. This should cost much less than repeatedly
starting tmux commands and capturing and rendering the whole pane history.

Pi 1.0 made fullscreen rendering the default. In this mode, Pi keeps its
transcript inside the application and draws a viewport into the terminal's
alternate screen buffer. Older messages do not accumulate in tmux history.
Other agent CLIs also support or use application-owned transcript scrolling.
A multiplexer change does not solve this: a screen capture cannot expose
messages that the agent has not drawn.

The current implementation has three related costs and limitations:

- `handlePaneStream` in `claude.go` polls `capture-pane` every 250 ms, capturing
  up to 2,000 history lines plus the screen. It renders changed captures to
  HTML and sends line patches over SSE.
- `pollWdSessions` captures each managed session's screen every second to
  estimate activity and turn completion.
- `captureHistory` in `history.go` saves full tmux scrollback snapshots. For
  fullscreen agents, these are not complete conversation archives.

Regular/inline terminal mode remains the immediate compatibility workaround.
The future transcript view must work without that setting.

## Separate the conversation from the terminal

Provide two views of a running session:

- **Transcript:** agent messages and tool results, with browser-owned scrolling,
  search, and durable history. Read this from the agent's persisted source.
- **Pane:** the current terminal display and existing input controls. Keep this
  for startup dialogs, permission prompts, extension interfaces, diagnostics,
  and agents without a supported transcript adapter.

Keep tmux for process lifetime, terminal attachment, and prompt/key input.
This design does not require RPC mode, a new multiplexer, or replacing the
agent's interactive CLI. Only poll the live pane when it is needed by a viewer
or a fallback path; do not keep full-history captures running behind a supported
transcript view.

## Agent-specific adapters

Keep source discovery and format parsing outside the shared browser and watcher
code. Each adapter must:

1. Associate a running agent with its exact conversation identity and source.
2. Read initial history and decode subsequent changes.
3. Map records into a shared transcript model.
4. Report its capabilities and any unsupported source or format version.
5. Detect source changes, such as `/new`, resume, fork, or session switching.

The shared model should include stable message IDs, ordering, roles, text,
structured tool calls/results, and timestamps where available. Keep tool-call
IDs so calls and results can be linked. Support message updates, not only
appends, and a reset when the active conversation changes. Retain unknown
record types without letting them break the whole view.

Initial adapter targets:

| Agent | Candidate source | Work required |
|---|---|---|
| Pi | Persisted session JSONL, normally under `~/.pi/agent/sessions/` | Resolve the exact session path; account for custom directories, the session tree, branches, and compaction. |
| Claude Code | Persisted conversation files | Verify the current schema, session association, resume behavior, and subagent relationships. |
| Codex | Persisted session/rollout files | Verify the current schema, configured storage location, and which records form the visible conversation. |
| OpenCode | Persisted session/message storage | Verify the current storage format. It may need database or API access rather than a JSONL file tail. |

These are research targets, not stable cross-version format guarantees. An
adapter may watch a file, a directory, or another change source. Do not force a
database-backed agent into an append-only file abstraction.

## Session identity and discovery

A checkout can have several conversations and agent processes. Do not select the
newest file in a checkout's transcript directory and assume it owns the pane.

Prefer an explicit source binding recorded at spawn time. If the CLI cannot
provide one, investigate process metadata or a small agent-specific integration
that reports the session identity. Only use discovery heuristics when they can
validate the association; otherwise show the pane fallback.

Persist the binding so a webdiff restart can reconnect to agents that remain
alive in tmux. Include the agent ID, conversation ID, source locator, and managed
session instance. A tmux session instance can contain several conversations
across `/new` or resume; archive them as distinct conversations associated with
that instance, not one file that silently overwrites its predecessor.

Keep existing repo/worktree identity rules: test `worktreesRoot` before
`rootDir`, and use the flat worktree slug outside the filesystem.

## Incremental watching

Use one server-side reader per source and share its updates among browser
subscribers. The repo already uses `fsnotify`; reuse that dependency where it
fits the source format.

For an append-only JSONL source:

- Register the watch before the initial read, then reconcile pending events so
  writes during startup are not missed.
- Read existing records once. Track the byte offset and retain an incomplete
  final record until its terminating newline arrives.
- Treat file notifications as wakeups to read through the current end, not as
  one notification per record. Coalesce bursts and avoid duplicate delivery.
- Watch the parent directory as needed so rename, atomic replacement, deletion,
  and recreation do not leave a watch on an obsolete file.
- Detect truncation or file replacement and rebuild with an explicit reset.
- On watcher overflow or error, reconcile source identity and offsets. Use a
  low-frequency metadata check as recovery, not repeated full transcript reads.

Bound record sizes, queues, and caches. Read large histories in batches and
paginate the browser view rather than rendering all messages on every update.
A reconnect should fetch a snapshot or replay from a revision cursor, then
receive incremental message changes over SSE. Slow clients must be able to
resynchronize without blocking the source reader.

Persisted transcripts may only be written at message or turn boundaries.
Measure each agent's write behavior before promising token-by-token streaming.
The live pane remains available; a later event integration could supply finer
updates without changing the transcript model.

## Activity and turn completion

A transcript write is not proof that a turn started or finished. No writes can
mean the agent is thinking, running a tool, or waiting for permission.

Use explicit lifecycle records where the adapter can identify them reliably.
Keep the existing activity heuristic or a separate integration where the file
format lacks that information. Do not trigger desktop sync from file silence
alone. Moving the conversation view to file watching will not, by itself,
remove all tmux polling from `pollWdSessions`.

## Durable history and safety

Archive transcript data under webdiff's history storage, with enough metadata
to identify the agent, format, conversation, and checkout. Do not depend only
on a pointer into the agent's own storage: that source may be deleted or moved.
Update the archive incrementally and finish a best-effort read before an
explicit restart or worktree removal. Preserve existing ANSI archives and
history links; mark terminal-only history as such rather than presenting it as
a complete transcript.

Treat transcript content as untrusted input. Escape text and sanitize rendered
Markdown and links. Resolve source paths on the server from validated bindings;
do not offer an endpoint that reads an arbitrary client-supplied path. Keep
archives private because messages and tool results can contain credentials or
other sensitive data.

## Phased implementation

1. Build a Pi adapter and tested session binding. Use fixtures for partial
   writes, duplicate notifications, file replacement, branches, and switching.
2. Add a Transcript view next to Pane. Use one incremental watcher per source
   and keep current prompt/key input unchanged.
3. Add durable transcript archives and restart/reconnect recovery. Keep the
   existing terminal archive format readable.
4. Stop conversation-related pane polling and full-history capture for supported
   sessions. Keep terminal fallback and separately address activity detection.
5. Add Claude Code, Codex, and OpenCode adapters after checking their installed
   formats and source update behavior.

Success means older messages remain accessible with fullscreen Pi, idle
transcript views issue no repeated `capture-pane` commands, and updates read
only new bytes during normal append-only operation. Test multiple browser
subscribers, webdiff restart, conversation switching, and source deletion.
Measure command count, CPU, bytes read, update latency, and browser payload size
against the current pane stream.

## Related design

[ZELLIJ-PLAN.md](ZELLIJ-PLAN.md) explores event-driven terminal capture. That is
still relevant to the Pane fallback, but terminal subscriptions are not a
replacement for agent transcript adapters.
