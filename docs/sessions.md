# Agent sessions

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
