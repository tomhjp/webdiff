# Deploying

When a feature or change is complete, always redeploy by running
`deploy/redeploy.sh`. It rebuilds the binary, restores the
cap_net_bind_service capability, and restarts the `webdiff` user service.

See `deploy/README.md` for one-time setup (`deploy/install.sh`), the systemd
unit, and prerequisites.
