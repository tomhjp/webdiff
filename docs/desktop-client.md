# Desktop client

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
(see [Selecting changes](selecting-changes.md)). If HEAD changed after the page
loaded, the send stops. The client pushes from a bare repo at
`<user cache dir>/webdiff/send/<owner>/<repo>.git`, not from your checkouts.
The repo stays between sends, so each send transfers only the new objects. It
is safe to delete. The GitHub repo is the last two segments of the path of the
sandbox remote URL, so a GitHub mirror or proxy also works. `-github-url` sets
the URL to push to.

## VS Code extension

`vscode/` has a VS Code extension that does the same from a Remote-SSH window,
with no browser and no `webdiff client`. It runs on the laptop
(`"extensionKind": ["ui"]`), so it uses the git credentials of the laptop. It
adds two commands to the command palette of Remote-SSH windows:

- **webdiff: Send Branch to GitHub** shows the commits after main, asks for the
  branch name (the current branch is the default), and sends HEAD as the
  **send to github** button does.
- **webdiff: Fetch Branch from GitHub** fetches a branch from GitHub and pushes
  it to the remote-tracking ref in the checkout, for example
  `refs/remotes/origin/<branch>`. If the checkout is on that branch, and a
  fast-forward is possible, it moves the branch forward with
  `git merge --ff-only`. It does not make merges or move a branch backward; if
  the two have diverged, merge or rebase on the sandbox.

The extension runs `webdiff github {info,send,fetch} <ssh-host>:<path>` on the
laptop. That reaches the checkout with `ssh`, not with the webdiff server:
`git` fetches from and pushes to `<ssh-host>:<path>`. Thus the ssh host of the
Remote-SSH window must also work with `ssh` in a shell. It uses the same bare
repos as the client. It needs the same `webdiff` binary on the laptop as
`webdiff client` does, from this version or later. To install the extension:

```
cd vscode && npx @vscode/vsce package --skip-license
code --install-extension webdiff-github-0.0.1.vsix
```

Install it on the laptop, not on the sandbox. Test the parts that do not use VS
Code with `node --test vscode/*.test.cjs`.

Set `webdiff.path` if `webdiff` is not on the `PATH` of VS Code, and
`webdiff.githubURL` for the same reason as `-github-url`.
