#!/bin/sh
# Regenerate the codex app-server JSON schema bundle from the installed CLI.
#
# Usage: scripts/gen-schema.sh
#
# Output always goes to .schema/ at the repository root; the directory is
# gitignored and is the only path this script deletes. VERSION inside it
# records the CLI that produced the bundle so protocol claims can be checked
# against docs/MVP.md, which names the version Counterpoint is developed
# against.
set -eu

if [ "$#" -ne 0 ]; then
	echo "usage: scripts/gen-schema.sh (takes no arguments)" >&2
	exit 2
fi

if ! command -v codex >/dev/null 2>&1; then
	echo "codex CLI not found on PATH; install it first" >&2
	exit 1
fi

if ! root="$(git rev-parse --show-toplevel 2>/dev/null)"; then
	echo "not inside a git worktree; run from a checkout of the repository" >&2
	exit 1
fi
out="$root/.schema"

rm -rf "$out"
mkdir -p "$out"
codex app-server generate-json-schema --out "$out"
codex --version > "$out/VERSION"
echo "schema written to $out for $(cat "$out/VERSION")"
