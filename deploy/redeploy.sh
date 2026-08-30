#!/usr/bin/env bash
# Rebuild the binary and restart the running service. Run this after every
# code change once install.sh has done the one-time setup.
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
export PATH=/usr/local/go/bin:$PATH

( cd "$repo_root" && go build -o webdiff . )
# setcap is dropped whenever the binary is replaced, so reapply it each build.
sudo setcap cap_net_bind_service=+ep "$repo_root/webdiff"
systemctl --user restart webdiff
systemctl --user --no-pager status webdiff | head -n 5
