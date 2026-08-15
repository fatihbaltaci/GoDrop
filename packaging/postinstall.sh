#!/bin/sh
# Runs after the package is installed or upgraded.
set -e

USER=godrop
GROUP=godrop
DATA_DIR=/var/lib/godrop
CONFIG=/etc/godrop/godrop.env

# A dedicated unprivileged account: GoDrop only needs to write its own data.
if ! getent group "$GROUP" >/dev/null 2>&1; then
	groupadd --system "$GROUP"
fi
if ! getent passwd "$USER" >/dev/null 2>&1; then
	useradd --system --gid "$GROUP" --home-dir "$DATA_DIR" \
		--shell /usr/sbin/nologin --comment "GoDrop file host" "$USER"
fi

mkdir -p "$DATA_DIR"
chown -R "$USER:$GROUP" "$DATA_DIR"
chmod 700 "$DATA_DIR"

# The configuration holds tokens, so it is readable only by the service.
if [ -f "$CONFIG" ]; then
	chown root:"$GROUP" "$CONFIG"
	chmod 640 "$CONFIG"
fi

if command -v systemctl >/dev/null 2>&1; then
	systemctl daemon-reload >/dev/null 2>&1 || true
fi

cat <<'MESSAGE'

GoDrop installed. Two steps remain:

  1. Create a token:   sudo -u godrop godrop token create --name default
  2. Start it:         sudo systemctl enable --now godrop

Then check the result with: godrop doctor

Configuration lives in /etc/godrop/godrop.env, uploads in /var/lib/godrop.

MESSAGE
