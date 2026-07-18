# Deploying webdiff

Run as a user systemd unit listening on port 80 over tailscale.

## First time setup

Check out the repo and run:

```
deploy/install.sh
```

Installs a user unit to `~/.config/systemd/user/webdiff.service`, enables linger,
then enables and starts the service.

By default it assumes you check out all repos into the same directory, including
webdiff, but if you want to serve repos from a different folder, pass it as an
arg:

```
deploy/install.sh ~/code
```

## Updates

```
deploy/redeploy.sh
```
