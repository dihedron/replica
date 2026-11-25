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
# the AppArmor profiles, enforced, where AppArmor runs (.specs/veho-packaging.md)
if [ -f /etc/apparmor.d/veho ] && command -v apparmor_parser >/dev/null &&
	[ "$(cat /sys/module/apparmor/parameters/enabled 2>/dev/null)" = Y ]; then
	apparmor_parser -r -W /etc/apparmor.d/veho
fi
if command -v firewall-cmd >/dev/null && firewall-cmd --state >/dev/null 2>&1; then
	firewall-cmd --reload >/dev/null 2>&1 || true # the new service definition; nothing is opened
fi
