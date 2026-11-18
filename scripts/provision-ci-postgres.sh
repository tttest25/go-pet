#!/usr/bin/env bash
set -euo pipefail

# Only for a disposable runner. Never stop, remove or replace an existing DB.
cd /opt/go-pet
mkdir -p /tmp/opencode
image=pgvector/pgvector:pg18
data=$(mktemp -d "$PWD/postgres/.ci-postgres.XXXXXXXX")
chmod 0755 "$data"
container=$(docker create --name petar_postgres \
    -p 127.0.0.1:5432:5432 \
    -e POSTGRES_USER=petar_user -e POSTGRES_PASSWORD=petar_password -e POSTGRES_DB=postgres \
    --mount "type=bind,src=$data,dst=/var/lib/postgresql" \
    "$image")
docker start "$container"

# Query the published port from the runner's network, not the initdb socket.
query=(psql -X -w -h 127.0.0.1 -p 5432 -U petar_user -d postgres -v ON_ERROR_STOP=1)
deadline=$((SECONDS + 60))
while ((SECONDS < deadline)); do
    [[ $(docker inspect -f '{{.State.Running}}' "$container") == true ]] || break
    if PGPASSWORD=petar_password timeout 3 "${query[@]}" -Atc \
        "SELECT current_setting('server_version_num')::int BETWEEN 180000 AND 189999
         AND NOT EXISTS (SELECT FROM pg_database WHERE datname='petar_test')" |
        grep -qx t; then
        echo 'Fresh PG18 TCP ready; petar_test does not exist'
        exit 0
    fi
    sleep 1
done
echo 'CI PostgreSQL did not reach the expected fresh PG18 state over TCP' >&2
docker inspect -f 'status={{.State.Status}} exit={{.State.ExitCode}}' "$container"
docker logs "$container"
exit 1
