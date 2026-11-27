# Pull requests

With `-maintner=<url>` (for example `-maintner=http://maintner.corp.ts.net`),
webdiff shows the GitHub pull request of each branch next to the branch name:
in the worktree groups on the home page, and in the heading of the diff and
stream pages. The link has a green tick when the pull request is approved, and
a yellow circle when it is not. It also says "draft", "merged" or "closed" when
that applies. A branch with more than one pull request shows the newest open
one, or the newest one if none is open.

The data comes from [maintner](https://github.com/tailscale/corp/tree/main/maintner),
which keeps a copy of the GitHub data of the Tailscale repos and gets GitHub
webhooks. Webdiff looks up each branch one time, and then follows the maintner
mutation log with one long poll. It looks up a branch again only when a
mutation is about its pull request or about a pull request that has the branch
as its head, so a change shows in some seconds without polling. A full refresh
each hour catches changes that the long poll missed. The stream page updates
its heading when the pull request changes; the other pages show the value when
they load.

The GitHub repo of a checkout comes from the URL of its default remote.

For repos that maintner does not watch, or for all repos with no `-maintner`,
webdiff polls git instead. The poll is adaptive: it asks about a repo each
`-pr-poll` (default 15m; `0` turns the poll off) while one of its worktree
branches has a commit from the last 3 hours, and else each `-pr-poll-idle`
(default 24h). A branch that a page shows for the first time is looked up at
once. One `git ls-remote` for each repo lists the `refs/pull/<n>/head` and
`refs/pull/<n>/merge` refs that GitHub keeps for each pull request. The pull
request of a branch is the one whose head is the commit of the branch on GitHub
or in the checkout; a branch at the commit of main has none. The poll asks
public GitHub without credentials first. For a private repo it then asks the
read-only git proxy at `-git-proxy` (for example
`-git-proxy=git://rogitproxy.corp.ts.net`). Git does not tell the review state,
so these pull requests show a grey ring in place of the tick or circle. An open
pull request has a merge ref; one without it is closed, merged or in conflict,
so it shows no state.
