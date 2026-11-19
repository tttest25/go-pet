#!/bin/sh
set -eu
export LC_ALL=C

for migration in /migrations/*.up.sql; do
    printf 'Applying %s\n' "$migration"
    psql -X -v ON_ERROR_STOP=1 --single-transaction \
        --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" --file "$migration"
done
