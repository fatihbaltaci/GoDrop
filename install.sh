#!/bin/sh
# GoDrop installer.
#
#   curl -fsSL https://godrop.sh/install.sh | sh
#
# Downloads the release binary for this machine, verifies its SHA-256 checksum
# against the published SHA256SUMS, installs it, and hands over to the setup
# wizard. Nothing is executed before the checksum matches.
#
# Environment:
#   GODROP_VERSION   version to install (default: the latest release)
#   GODROP_BIN_DIR   install location (default: /usr/local/bin, or ~/.local/bin)
#   GODROP_NO_INIT   set to 1 to skip the setup wizard
set -eu

REPO="fatihbaltaci/GoDrop"
VERSION="${GODROP_VERSION:-}"
BIN_DIR="${GODROP_BIN_DIR:-}"

# Colour only on a terminal; a piped install produces clean logs.
if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
	BOLD=$(printf '\033[1m'); DIM=$(printf '\033[2m')
	GREEN=$(printf '\033[32m'); RED=$(printf '\033[31m')
	YELLOW=$(printf '\033[33m'); RESET=$(printf '\033[0m')
else
	BOLD=''; DIM=''; GREEN=''; RED=''; YELLOW=''; RESET=''
fi

say()  { printf '  %s\n' "$*"; }
ok()   { printf '  %s✓%s %s\n' "$GREEN" "$RESET" "$*"; }
warn() { printf '  %s⚠%s %s\n' "$YELLOW" "$RESET" "$*"; }
die()  { printf '  %s✗%s %s\n' "$RED" "$RESET" "$*" >&2; exit 1; }

need() {
	command -v "$1" >/dev/null 2>&1 || die "$1 is required but not installed"
}

detect_platform() {
	os=$(uname -s | tr '[:upper:]' '[:lower:]')
	arch=$(uname -m)
	case "$os" in
		linux|darwin) ;;
		*) die "unsupported operating system: $os (build from source: go install github.com/$REPO/cmd/godrop@latest)" ;;
	esac
	case "$arch" in
		x86_64|amd64) arch=amd64 ;;
		aarch64|arm64) arch=arm64 ;;
		*) die "unsupported architecture: $arch" ;;
	esac
	PLATFORM="${os}_${arch}"
}

latest_version() {
	curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" |
		sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n 1
}

choose_bin_dir() {
	if [ -n "$BIN_DIR" ]; then
		return
	fi
	if [ -w /usr/local/bin ] 2>/dev/null; then
		BIN_DIR=/usr/local/bin
	elif [ "$(id -u)" = "0" ]; then
		BIN_DIR=/usr/local/bin
	else
		BIN_DIR="$HOME/.local/bin"
	fi
}

main() {
	printf '\n  %sGoDrop%s — upload a file, get a hard-to-guess URL\n\n' "$BOLD" "$RESET"

	need curl
	need uname
	detect_platform
	ok "detected ${PLATFORM%_*}/${PLATFORM#*_}"

	if [ -z "$VERSION" ]; then
		VERSION=$(latest_version) || true
		[ -n "$VERSION" ] || die "could not determine the latest version; set GODROP_VERSION"
	fi
	ok "installing $VERSION"

	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT INT TERM

	archive="godrop_${VERSION#v}_${PLATFORM}.tar.gz"
	base="https://github.com/$REPO/releases/download/$VERSION"

	curl -fsSL "$base/$archive" -o "$tmp/$archive" ||
		die "download failed: $base/$archive"
	curl -fsSL "$base/SHA256SUMS" -o "$tmp/SHA256SUMS" ||
		die "could not download SHA256SUMS — refusing to install an unverified binary"

	# Verify before unpacking. This is the whole security model of `curl | sh`:
	# the script comes from godrop.sh over TLS, the binary is checked against a
	# checksum published with the release.
	expected=$(grep " $archive\$" "$tmp/SHA256SUMS" | awk '{print $1}')
	[ -n "$expected" ] || die "$archive is not listed in SHA256SUMS"

	if command -v sha256sum >/dev/null 2>&1; then
		actual=$(sha256sum "$tmp/$archive" | awk '{print $1}')
	elif command -v shasum >/dev/null 2>&1; then
		actual=$(shasum -a 256 "$tmp/$archive" | awk '{print $1}')
	else
		die "neither sha256sum nor shasum is available; cannot verify the download"
	fi
	[ "$expected" = "$actual" ] || die "checksum mismatch — expected $expected, got $actual"
	ok "checksum verified"

	tar -xzf "$tmp/$archive" -C "$tmp" || die "could not unpack $archive"
	[ -f "$tmp/godrop" ] || die "the archive does not contain a godrop binary"
	chmod +x "$tmp/godrop"

	choose_bin_dir
	mkdir -p "$BIN_DIR" 2>/dev/null || true
	if [ -w "$BIN_DIR" ]; then
		mv "$tmp/godrop" "$BIN_DIR/godrop"
	elif command -v sudo >/dev/null 2>&1; then
		say "installing to $BIN_DIR (needs sudo)"
		sudo mv "$tmp/godrop" "$BIN_DIR/godrop"
	else
		die "$BIN_DIR is not writable and sudo is unavailable; set GODROP_BIN_DIR"
	fi
	ok "installed $BIN_DIR/godrop"

	case ":$PATH:" in
		*":$BIN_DIR:"*) ;;
		*) warn "$BIN_DIR is not in your PATH — add: export PATH=\"$BIN_DIR:\$PATH\"" ;;
	esac

	printf '\n'
	"$BIN_DIR/godrop" version

	if [ "${GODROP_NO_INIT:-}" = "1" ]; then
		printf '\n  Next: %sgodrop init%s\n\n' "$BOLD" "$RESET"
		return
	fi

	# The wizard needs a terminal. Piping this script into sh leaves stdin
	# consumed, so reconnect it when we can and otherwise just say what to run.
	if [ -t 0 ]; then
		printf '\n'
		exec "$BIN_DIR/godrop" init
	elif [ -r /dev/tty ]; then
		printf '\n'
		exec "$BIN_DIR/godrop" init < /dev/tty
	else
		printf '\n  Next: %sgodrop init%s   (guided setup)\n' "$BOLD" "$RESET"
		printf '  %sor:%s   godrop init --non-interactive --base-url https://files.example.com\n\n' "$DIM" "$RESET"
	fi
}

main "$@"
