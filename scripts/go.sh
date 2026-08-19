#!/usr/bin/env bash
# Runs the Go toolchain inside the golang:1.26-trixie image with the repo
# mounted at /src. See scripts/go.ps1 for the Windows equivalent.
#
#   ./scripts/go.sh test ./...
#
# Env passthrough: any EZLG_* variables set in the calling shell.
set -euo pipefail
repo="$(cd "$(dirname "$0")/.." && pwd)"
envargs=()
while IFS= read -r name; do
  envargs+=(-e "$name=${!name}")
done < <(compgen -e | grep '^EZLG_' || true)
# "${arr[@]+"${arr[@]}"}" expands to nothing when the array is empty instead
# of tripping `set -u` on bash 3.2 (macOS /bin/bash).
exec docker run --rm \
  -v "$repo:/src" -w /src \
  -v ezlg-gomod:/go/pkg/mod -v ezlg-gocache:/root/.cache/go-build \
  -e GOFLAGS=-buildvcs=false -e CGO_ENABLED=0 \
  "${envargs[@]+"${envargs[@]}"}" \
  golang:1.26-trixie go "$@"
