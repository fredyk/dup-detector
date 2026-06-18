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
echo "Installed: ${dest}/dup-detector"
echo "Live profiling: http://127.0.0.1:8158/debug/pprof/  (set DUP_DETECTOR_PPROF=off to disable)"
