#!/bin/sh
# Regenerate the codex app-server JSON schema bundle from the installed CLI.
#
# Usage: scripts/gen-schema.sh [output-dir]   (default: .schema)
#
# The output directory is gitignored. VERSION inside it records the CLI that
# produced the bundle so protocol claims can be checked against docs/MVP.md,
# which names the version Counterpoint is developed against.
set -eu

out="${1:-.schema}"

if ! command -v codex >/dev/null 2>&1; then
	echo "codex CLI not found on PATH; install it first" >&2
	exit 1
fi

rm -rf "$out"
mkdir -p "$out"
codex app-server generate-json-schema --out "$out"
codex --version > "$out/VERSION"
echo "schema written to $out for $(cat "$out/VERSION")"
