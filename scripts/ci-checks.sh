#!/usr/bin/env bash
set -euo pipefail

cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.."
./scripts/test-db.sh prepare
./scripts/test-db.sh check
GOFLAGS=-count=1 ./scripts/quality-gate.sh
go build ./...
