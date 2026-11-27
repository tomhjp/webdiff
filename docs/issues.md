# Issues

The home page also shows the GitHub issues of each branch at the right end of
the line of the branch, apart from its pull request. They come from the commits of the branch that
are not on main: a line of a commit message that starts with a word such as
"Updates" or "Fixes" names issues, for example `Updates #123` or
`Fixes tailscale/corp#45, #46`. An issue of the repo of the branch shows as
`#123`, an issue of tailscale/corp or tailscale/tailscale as `corp#123` or
`oss#123`, and an issue of another repo as `owner/repo#123`. Each links to
GitHub.

When the commits name no issue, **+ issue** on the branch line opens a field
where you can write them: `#123`, `corp#45`, `oss#6`, `owner/repo#7`, or a
GitHub URL, separated by spaces or commas. Enter saves, and Escape cancels.
The pencil after issues that you wrote changes them; an empty list removes
them. Webdiff keeps them for each branch in
`~/.config/webdiff/branch-issues.json`, and a worktree rename moves them to
the new branch. The diff and stream pages do not show issues.
