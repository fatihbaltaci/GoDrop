#!/bin/sh
# Runs after the package is removed. Uploaded files and the configuration are
# deliberately left in place: removing a package must never delete data.
set -e

if command -v systemctl >/dev/null 2>&1; then
	systemctl daemon-reload >/dev/null 2>&1 || true
fi

cat <<'MESSAGE'
GoDrop removed. Your files are still in /var/lib/godrop and the configuration
in /etc/godrop. Delete them yourself if you no longer need them.
MESSAGE
