# Repo layout on disk

`rootDir` is the code directory webdiff serves (`~/ai` on the deployed host).
Managed worktrees live *inside* it at `<rootDir>/wt/<repo>/<worktree>`, so any
code that classifies a path by prefix must test `worktreesRoot` **before**
`rootDir` or every worktree looks like a repo named `wt`.

A worktree's identity everywhere outside the filesystem is the flat slug
`<repo>-<worktree>` (`worktreeSlug`): the URL (`/worktrees/<slug>/`), the tmux
session (`<slug>-wd`), the diff-cache dir, and the `(kind, name)` pair in the
desktop-sync protocol. `repoRef.name` is that slug, *not* `filepath.Base(abs)`.
Since the slug flattens two path segments it can't be split back into a path,
so resolution matches it against what's on disk rather than joining it onto
`worktreesRoot`.

# Deploying

When a feature or change is complete, always redeploy by running
`deploy/redeploy.sh`. It rebuilds the binary, restores the
cap_net_bind_service capability, and restarts the `webdiff` user service.

See `deploy/README.md` for one-time setup (`deploy/install.sh`), the systemd
unit, and prerequisites.
