#!/bin/sh
# Unload veho's AppArmor profiles before their file goes, on removal only
# (an upgrade keeps them loaded; the postinstall reloads the new ones).
set -e
case "$1" in
remove | purge | 0)
	if [ -f /etc/apparmor.d/veho ] && command -v apparmor_parser >/dev/null; then
		apparmor_parser -R /etc/apparmor.d/veho 2>/dev/null || true
	fi
	;;
esac
