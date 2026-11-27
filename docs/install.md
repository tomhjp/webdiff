# Install

```
go install github.com/tomhjp/webdiff@latest
```

Then run from a directory containing your git repos:

```
webdiff [-agent="<command>"] [-branch-prefix="<prefix>"] [-develop-on-main="<repo>,..."] [port]
```

`-agent` accepts a full command line (e.g. `-agent="opencode --foo"`); the first whitespace-separated token is used as the display name on the diff page's agent button. Defaults to `claude`.

`-branch-prefix` prepopulates the branch field in the worktree creation form. The home page and managed worktree directory names omit that prefix. It defaults to empty.

`-develop-on-main` names the repos that you develop directly on main. It defaults to empty. The home page shows these repos above the repos section, with their branch and diff stats. Do the work for all other repos in managed worktrees. In the repos section, each of the other repos has a row with inline **+ worktree** and **+ agent** buttons, and below it one row for each agent session. **+ worktree** creates a worktree, from main by default, and opens its agent page. **+ agent** starts a new session in the repo, beside any sessions that already run (see [Multiple agents per checkout](sessions.md#multiple-agents-per-checkout)). The agent page links to the diff and files.

Each agent session is labelled with its agent and its working directory from `~`, for example "pi · ~/ai/wt/corp/safer-ciguestlet-shutdown". The home page shows the label on the agent row. The Rex apps show it as the agent window label, which webdiff sets when it starts the session and again when a worktree rename moves the directory.

Once a day, webdiff runs `git pull --ff-only` in each repo under the code directory, so that the repos stay near their latest main. It records the time of each attempt in `~/.cache/webdiff/pull.json`, so a restart does not start a new pull. A repo with no upstream branch, or one that cannot fast-forward, stays as it is, and the journal shows why.

The `-agent` flag sets the *default* agent. Individual sessions can run something else: the diff page's spawn button has an agent drop-down beside it, and the stream page's "restart session" modal picks which agent the fresh session runs. The choice travels as `?agent=<id>`, so reloading the stream page still shows which agent is running. Selectable ids are `claude` and `pi`, plus whatever `-agent` names.

Alternatively, get a more opinionated install from `deploy/install.sh` which:

* Installs and starts a user systemd unit
* Serves webdiff's own parent directory as the code directory
* Enables linger
