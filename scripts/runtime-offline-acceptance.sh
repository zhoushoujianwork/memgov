#!/bin/sh
# Usage: scripts/runtime-offline-acceptance.sh [/absolute/path/to/memgov]
# All state is temporary. A passed binary is executed but never overwritten.
set -eu
repo=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
work=$(mktemp -d "${TMPDIR:-/tmp}/memgov-offline-acceptance.XXXXXX")
trap 'rm -rf "$work"' EXIT HUP INT TERM
cd "$repo"
if [ "$#" -gt 1 ]; then
  echo 'Usage: runtime-offline-acceptance.sh [memgov-binary]' >&2
  exit 2
fi
if [ "$#" -eq 1 ]; then
  case "$1" in /*) acceptance_binary=$1 ;; *) acceptance_binary="$PWD/$1" ;; esac
else
  acceptance_binary="$work/memgov"
  go build -trimpath -o "$acceptance_binary" ./cmd/memgov
fi
if [ ! -x "$acceptance_binary" ]; then
  echo 'Acceptance binary must be executable.' >&2
  exit 2
fi
export MEMGOV_ACCEPTANCE_BINARY="$acceptance_binary"
echo 'Running isolated schema 6/21 migration, Cyber owner, restart recovery and fake DWS binary acceptance.'
go test ./internal/core -run 'Test(DualModeRestart|RuntimeRestartUnknown|InstalledRuntimeAcceptance|Schema22|CyberOwner|RuntimeOwnerMessage)' -count=1 -v
echo 'Offline acceptance passed. No live DingTalk or model request was made.'
