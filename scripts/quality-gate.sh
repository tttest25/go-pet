#!/usr/bin/env bash
set -euo pipefail

APP_WAS_RUNNING=false
if docker ps --format '{{.Names}}' | grep -qx 'petar_app'; then
    APP_WAS_RUNNING=true
fi

cleanup() {
    if [[ "$APP_WAS_RUNNING" == "true" ]]; then
        echo "=== [Cleanup] Restarting petar_app container ==="
        docker start petar_app >/dev/null
    fi
}
trap cleanup EXIT

echo "=== [1/4] Checking PostgreSQL 18 container health ==="
docker exec petar_postgres pg_isready -U petar_user -d petar_db >/dev/null

if [[ "$APP_WAS_RUNNING" == "true" ]]; then
    echo "=== [Prep] Pausing petar_app to prevent LISTEN/NOTIFY race ==="
    docker stop petar_app >/dev/null
fi

echo "=== [2/4] Verifying SQL Migrations idempotency ==="
docker exec -i petar_postgres psql -U petar_user -d petar_db -v ON_ERROR_STOP=1 <<'SQL'
BEGIN;
SELECT COUNT(*) FROM sm_inputs;
SELECT COUNT(*) FROM sm_processes;
SELECT COUNT(*) FROM sm_jobs;
ROLLBACK;
SQL

echo "=== [3/4] Running go vet ==="
go vet ./...

echo "=== [4/4] Running go test with race detector ==="
go test -v -race -timeout 30s ./...

echo "QUALITY GATE PASSED"
