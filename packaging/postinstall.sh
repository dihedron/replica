#!/bin/sh
# The service's user and its configuration directory. The service is neither
# enabled nor started: it needs /etc/veho/veho.yaml, veho.env and the unwrap
# key first (the Setup Guide).
set -e
getent passwd veho >/dev/null || useradd --system --user-group --home-dir /var/lib/veho --no-create-home --shell /usr/sbin/nologin veho
install -d -o root -g veho -m 0750 /etc/veho
if command -v systemctl >/dev/null && [ -d /run/systemd/system ]; then
	systemctl daemon-reload
fi
