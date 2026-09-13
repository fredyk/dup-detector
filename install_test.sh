#!/usr/bin/env bash
# install_test.sh — install.sh must succeed while the installed binary is running.
# A scan of /tank runs for days; "text file busy" from writing over the running
# executable blocked every install during that time.
set -euo pipefail
cd "$(dirname "$0")"

# Inside the repo, not /tmp: go only copies (and hits the busy file) when its
# build dir and GOBIN are on different filesystems, as with ~/go/bin in real life.
tmp="$(mktemp -d -p "$PWD" .install-test-XXXXXX)"
trap 'kill "$busy" 2>/dev/null || true; rm -rf "$tmp"' EXIT

mkdir -p "$tmp/real" "$tmp/bin" "$tmp/home"
cp /bin/sleep "$tmp/real/dup-detector"
ln -s "$tmp/real/dup-detector" "$tmp/bin/dup-detector"   # same shape as ~/go/bin -> /unsafe/gopath/bin
"$tmp/real/dup-detector" 60 &
busy=$!
sleep 0.2

if ! HOME="$tmp/home" GOBIN="$tmp/bin" PATH="/usr/bin:/bin:$(dirname "$(command -v go)")" ./install.sh >"$tmp/out" 2>&1; then
	echo "FAIL: install.sh failed with the binary running:"; cat "$tmp/out"; exit 1
fi
if ! "$tmp/bin/dup-detector" --help >/dev/null 2>&1; then
	echo "FAIL: the installed binary is not the fresh dup-detector build"; exit 1
fi
if [ ! -L "$tmp/bin/dup-detector" ]; then
	echo "FAIL: install.sh replaced the symlink instead of the file it points to"; exit 1
fi
echo "ok"
