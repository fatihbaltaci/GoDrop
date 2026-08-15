#!/bin/sh
# Runs before the package is removed.
set -e

if command -v systemctl >/dev/null 2>&1; then
	systemctl stop godrop >/dev/null 2>&1 || true
	systemctl disable godrop >/dev/null 2>&1 || true
fi
