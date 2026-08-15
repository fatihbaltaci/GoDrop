#!/bin/sh
# Rewrite the command line reference in README.md from the binary's own help.
#
# The README promises the real output, so it is generated rather than typed.
# TestReadmeCommandLineIsUpToDate fails if the two drift apart.
set -eu

BIN=${1:-./godrop}
README=README.md
BEGIN='<!-- BEGIN CLI -->'
END='<!-- END CLI -->'

[ -x "$BIN" ] || { echo "no binary at $BIN (run: make build)" >&2; exit 1; }
grep -q "$BEGIN" "$README" || { echo "$BEGIN marker missing from $README" >&2; exit 1; }

tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT

{
	printf '%s\n\n' "$BEGIN"
	for cmd in "" "serve" "init" "token" "token create" "token list" "token revoke" \
		"doctor" "telemetry" "health" "version"; do
		if [ -z "$cmd" ]; then label="godrop --help"; else label="godrop $cmd --help"; fi
		printf '<details>\n<summary><code>%s</code></summary>\n\n' "$label"
		printf '```console\n$ %s\n' "$label"
		# shellcheck disable=SC2086 # the command words must split
		"$BIN" $cmd --help
		printf '```\n\n</details>\n\n'
	done
	printf '%s\n' "$END"
} >"$tmp"

awk -v begin="$BEGIN" -v end="$END" -v block="$tmp" '
	$0 == begin { while ((getline line < block) > 0) print line; skip = 1; next }
	$0 == end   { skip = 0; next }
	!skip       { print }
' "$README" >"$README.new"
mv "$README.new" "$README"
echo "updated the command line reference in $README"
