#!/usr/bin/env bash
# Build and atomically install the Go server. Does not restart a running service.
set -euo pipefail
root=$(cd -- "$(dirname -- "$0")/.." && pwd)
prefix=${1:-"$HOME/.local"}
[[ $prefix = /* ]] || { echo 'Install prefix must be absolute' >&2; exit 2; }
mkdir -p -- "$prefix/bin"
staging=$(mktemp "$prefix/bin/.mirage-server.XXXXXXXX")
trap 'rm -f -- "$staging"' EXIT
cd -- "$root"
go build -tags purego -trimpath -o "$staging" ./cmd/mirage-server
chmod 755 "$staging"
mv -f -- "$staging" "$prefix/bin/mirage-server"
printf 'Installed %s/bin/mirage-server\n' "$prefix"
