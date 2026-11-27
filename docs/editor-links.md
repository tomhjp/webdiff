# Opening a checkout in Zed, VS Code, or Ghostty

Every repo and worktree — on the home page rows and in the diff page toolbar —
carries three links that jump straight from reviewing to working, marked with
the Zed, VS Code, and Ghostty icons: the first two open the checkout in their
respective editor, and the third opens a Ghostty window with a shell in it.

On the diff page, the heading of each changed file also has Zed and VS Code
links that open that file at its first change. A deep link can name only one
path, so these links can't open the checkout as well. If a window for the
checkout is already open, the file opens in that window. If not, the editor
opens the file by itself.

All the links target the checkout *on this host* over SSH, so they need to
know how your desktop reaches it:

```
webdiff -ssh-host=<alias> [port]
```

`<alias>` is whatever you'd type after `ssh` — usually an `~/.ssh/config` entry.
It defaults to this node's MagicDNS name, which is right when you SSH by
hostname and wrong the moment your config needs a different user, port or key.
`deploy/install.sh` takes it as a second argument, after the code dir.

Zed and VS Code need nothing extra installed: they register `zed://` and
`vscode://` themselves, and the links use their SSH remote URL forms. Ghostty
has no equivalent, so the terminal link goes via a `webdiff://` handler you
register once, on the machine running the browser:

```
deploy/install-url-handler.sh <alias>
```

Pass the same alias. The handler pins it and rejects URLs naming any other
host — once a scheme is registered, any page your browser loads can feed it a
URL, so it treats its input as hostile.

All three links are hidden below 900px: their schemes do not resolve on iOS.

The three marks are traced from the apps' own icons into `assets/icons.svg` as a
`<symbol>` sprite, inlined once per page and referenced with `<use>`. They're a
few KB of path data each, which is too much to repeat on every home page row.
