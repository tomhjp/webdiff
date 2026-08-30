#!/usr/bin/env bash
# One-time setup for running webdiff as a per-user systemd service on this
# machine. Safe to re-run: it's idempotent. Day-to-day rebuilds should use
# redeploy.sh instead.
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
unit_dir="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"

# The serve root: a directory containing your git repos. Defaults to the parent
# of webdiff's repo root; pass a path to override.
code_dir="$(cd "${1:-$repo_root/..}" && pwd)"

# Optional second arg: how your laptop reaches this host over SSH, for the
# diff page's "open in Zed / Ghostty" buttons. An ~/.ssh/config alias is the
# usual answer. Omit it to fall back to this node's MagicDNS name.
ssh_host_arg=""
if [[ -n "${2:-}" ]]; then
	ssh_host_arg="-ssh-host=$2"
fi

# go isn't always on the PATH under a bare systemd/cron environment.
export PATH=/usr/local/go/bin:$PATH

echo "==> Building webdiff"
( cd "$repo_root" && go build -o webdiff ./... )

# Port 80 is privileged; grant the binary the capability to bind it rather
# than running the whole service as root. setcap needs sudo but the service
# itself stays a normal user unit.
echo "==> Granting cap_net_bind_service (needed to bind port 80)"
sudo setcap cap_net_bind_service=+ep "$repo_root/webdiff"

echo "==> Installing user service to $unit_dir (code dir: $code_dir)"
mkdir -p "$unit_dir"
sed -e "s|__CODE_DIR__|$code_dir|g" \
	-e "s|__SSH_HOST_ARG__|$ssh_host_arg|g" \
	"$repo_root/deploy/webdiff.service" \
	> "$unit_dir/webdiff.service"
chmod 644 "$unit_dir/webdiff.service"

# Linger lets the user service run without an active login session, so
# webdiff survives logout and starts on boot.
echo "==> Enabling linger for $USER"
sudo loginctl enable-linger "$USER"

echo "==> Enabling and starting webdiff"
systemctl --user daemon-reload
systemctl --user enable --now webdiff

systemctl --user --no-pager status webdiff | head -n 5
