#!/usr/bin/env bash
set -euo pipefail
root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
command="${1:-}"
case "$command" in
  prepare|check)
    [[ $# == 1 ]] || { echo "Usage: test-db.sh prepare|check|reset --confirm petar_test" >&2; exit 2; }
    ;;
  reset)
    [[ $# == 3 && ${2:-} == --confirm && ${3:-} == petar_test ]] || {
      echo "Reset requires: reset --confirm petar_test" >&2; exit 2;
    }
    ;;
  *) echo "Usage: test-db.sh prepare|check|reset --confirm petar_test" >&2; exit 2 ;;
esac
cd "$root"
temp="$(mktemp -d "${TMPDIR:-/tmp/opencode}/petar-test-db.XXXXXXXX")"
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
# Never execute ordinary integration tests while bootstrapping the command.
env -u PETAR_TEST_DB_COMMAND -u PETAR_TEST_DB_CONFIRM go test -c -o "$temp/bridge" ./internal/db &
child=$!
wait "$child"
child=""
PETAR_TEST_DB_COMMAND="$command" PETAR_TEST_DB_CONFIRM="${3:-}" \
  "$temp/bridge" '-test.run=^TestPetarTestDBCommand$' -test.v &
child=$!
wait "$child"
child=""
