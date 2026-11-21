#!/bin/sh
# The service's user and its configuration directory. The service is neither
# enabled nor started: it needs an environment file first.
set -e
getent passwd replica >/dev/null || useradd --system --user-group --home-dir /var/lib/replica --no-create-home --shell /usr/sbin/nologin replica
install -d -o root -g replica -m 0750 /etc/replica
if command -v systemctl >/dev/null && [ -d /run/systemd/system ]; then
	systemctl daemon-reload
fi
