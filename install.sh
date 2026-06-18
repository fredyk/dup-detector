#!/usr/bin/env bash
# install.sh — build & install dup-detector into $GOBIN (or ~/go/bin).
#
# The SQLite driver is mattn/go-sqlite3 (C-SQLite via CGo), so a C toolchain
# (gcc + libc headers) is REQUIRED — a pure-Go build will not link.
#
# Live profiling: the installed binary serves net/http/pprof on
# http://127.0.0.1:8158/debug/pprof/ by default (loopback-only). Override the
# address with DUP_DETECTOR_PPROF=":6060", or disable with DUP_DETECTOR_PPROF=off.
set -euo pipefail

cd "$(dirname "$0")"

if ! command -v gcc >/dev/null 2>&1; then
	echo "error: gcc not found — mattn/go-sqlite3 needs a C toolchain (apt install build-essential)" >&2
	exit 1
fi

export CGO_ENABLED=1

echo "Building dup-detector (CGo / C-SQLite)..."
go install .

dest="$(go env GOBIN)"
[ -n "$dest" ] || dest="$(go env GOPATH)/bin"
installed="${dest}/dup-detector"
echo "Installed: ${installed}"

# go install writes to GOBIN/GOPATH-bin, which may NOT be the dup-detector that
# actually runs: a stale copy earlier on PATH (or one invoked by full path) would
# shadow the fresh build. Point any such existing shadow at the canonical binary
# via symlink, so `dup-detector` (and the legacy ~/go/bin path) always run the
# build we just made. We only refresh paths that ALREADY exist — never create new
# shadows.
refresh_shadow() {
	local p="$1"
	[ -n "$p" ] || return 0
	[ -e "$p" ] || [ -L "$p" ] || return 0           # only touch existing copies
	[ "$p" = "$installed" ] && return 0
	[ "$(readlink -f "$p" 2>/dev/null)" = "$installed" ] && return 0  # already linked
	if ln -sf "$installed" "$p" 2>/dev/null; then
		echo "Refreshed stale copy on PATH: ${p} -> ${installed}"
	else
		echo "WARNING: ${p} shadows the fresh build but could not be updated (permissions?)." >&2
		echo "         Remove it or run: ln -sf '${installed}' '${p}'" >&2
	fi
}

refresh_shadow "$(command -v dup-detector 2>/dev/null || true)"
refresh_shadow "$HOME/go/bin/dup-detector"

# Final truth: what will actually run?
resolved="$(command -v dup-detector 2>/dev/null || true)"
if [ -n "$resolved" ]; then
	echo "On PATH: ${resolved} -> $(readlink -f "$resolved")"
	case ":$PATH:" in
		*":$dest:"*) ;;
		*) echo "Note: ${dest} is not on your PATH; add it so plain 'dup-detector' resolves to the canonical install." >&2 ;;
	esac
else
	echo "Note: 'dup-detector' is not on your PATH. Add ${dest} to PATH, or run it as ${installed}." >&2
fi
echo "Live profiling: http://127.0.0.1:8158/debug/pprof/  (set DUP_DETECTOR_PPROF=off to disable)"
