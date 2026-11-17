#!/usr/bin/env bash
set -euo pipefail

[[ $# == 0 ]] || { echo "Usage: quality-gate.sh" >&2; exit 2; }
root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"
temp="$(mktemp -d "${TMPDIR:-/tmp/opencode}/petar-quality-gate.XXXXXXXX")"
child=""
cleanup() {
  status=$?
  trap - EXIT INT TERM
  if [[ -n "$child" ]]; then
    kill -TERM "$child" 2>/dev/null || true
    wait "$child" 2>/dev/null || true
  fi
  rm -f -- "$temp/bridge"
  rmdir -- "$temp"
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
env -u PETAR_TEST_DB_COMMAND -u PETAR_TEST_DB_CONFIRM go test -c -o "$temp/bridge" ./internal/db &
child=$!
wait "$child"
child=""
PETAR_TEST_DB_COMMAND=gate PETAR_TEST_DB_CONFIRM= \
  "$temp/bridge" '-test.run=^TestPetarTestDBCommand$' -test.v &
child=$!
wait "$child"
child=""
