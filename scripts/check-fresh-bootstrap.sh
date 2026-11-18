#!/usr/bin/env bash
set -euo pipefail

if ! [[ "$(dirname -- "${BASH_SOURCE[0]}")/.." -ef /opt/go-pet ]]; then
    echo 'Fresh bootstrap requires this checkout at /opt/go-pet' >&2
    exit 1
fi
cd /opt/go-pet
root=$PWD
image=pgvector/pgvector:pg18
data=$(mktemp -d "$root/postgres/.fresh-bootstrap.XXXXXXXX")
name="petar-bootstrap-${data##*.}"
container=

cleanup() {
    if [[ -n "$container" ]]; then
        docker rm -f "$container" >/dev/null
        # The entrypoint owns the PGDATA children as postgres, not the caller.
        docker run --rm --network none --entrypoint sh \
            --mount "type=bind,src=$data,dst=/cleanup" "$image" \
            -c 'rm -rf -- /cleanup/18'
    fi
    rmdir -- "$data"
}
trap cleanup EXIT
# The entrypoint drops privileges; postgres needs to traverse the mount root.
chmod 0755 "$data"
container=$(docker create --name "$name" --network none --shm-size 128m --memory 450m \
    -e POSTGRES_USER=petar_user -e POSTGRES_PASSWORD=petar_password -e POSTGRES_DB=petar_db \
    --mount "type=bind,src=$data,dst=/var/lib/postgresql" \
    --mount "type=bind,src=$root/postgres/migrations,dst=/docker-entrypoint-initdb.d,readonly" \
    --mount "type=bind,src=$root/postgres/postgresql.conf,dst=/etc/postgresql/postgresql.conf,readonly" \
    "$image" postgres -c config_file=/etc/postgresql/postgresql.conf)
docker start "$container"

exec_db=(docker exec -e PGPASSWORD=petar_password "$container")
psql=(psql -X -h 127.0.0.1 -U petar_user -d petar_db -v ON_ERROR_STOP=1)
ready=false
deadline=$((SECONDS + 60))
while ((SECONDS < deadline)); do
    [[ $(docker inspect -f '{{.State.Running}}' "$container") == true ]] || break
    # The temporary initdb server listens on a Unix socket only.
    if timeout 3 "${exec_db[@]}" "${psql[@]}" -c 'SELECT 1' >/dev/null 2>&1; then
        ready=true
        break
    fi
    sleep 1
done
docker inspect -f 'status={{.State.Status}} exit={{.State.ExitCode}}' "$container"
docker logs "$container"
if ! "$ready"; then
    echo 'Fresh bootstrap: final TCP server did not become ready' >&2
    exit 1
fi

timeout 10 docker exec -i -e PGPASSWORD=petar_password "$container" "${psql[@]}" <<'SQL'
SHOW server_version;
SELECT extname FROM pg_extension ORDER BY extname;
SELECT srvname FROM pg_foreign_server;
SELECT to_regclass('sm_inputs'), to_regclass('sm_processes'), to_regclass('sm_jobs'),
       to_regclass('sm_job_logs'), to_regclass('sm_outputs');
SELECT to_regprocedure('sm_create_input(text,jsonb)'),
       to_regprocedure('sm_validate_tool_payload(jsonb)'),
       to_regprocedure('sm_pick_next_job()'),
       to_regprocedure('sm_update_job_status(uuid,text,text,text)');
SELECT tgrelid::regclass, tgname, tgenabled FROM pg_trigger WHERE NOT tgisinternal ORDER BY tgname;
SELECT current_setting('server_version_num')::int BETWEEN 180000 AND 189999
   AND (SELECT count(*) = 3 FROM pg_extension WHERE extname IN ('vector','file_fdw','pg_trgm'))
   AND EXISTS (SELECT FROM pg_foreign_server WHERE srvname='dokuwiki_files')
   AND to_regclass('public.sm_inputs') IS NOT NULL
   AND to_regclass('public.sm_processes') IS NOT NULL
   AND to_regclass('public.sm_jobs') IS NOT NULL
   AND to_regclass('public.sm_job_logs') IS NOT NULL
   AND to_regclass('public.sm_outputs') IS NOT NULL
   AND to_regprocedure('public.sm_create_input(text,jsonb)') IS NOT NULL
   AND to_regprocedure('public.sm_validate_tool_payload(jsonb)') IS NOT NULL
   AND to_regprocedure('public.sm_pick_next_job()') IS NOT NULL
   AND to_regprocedure('public.sm_update_job_status(uuid,text,text,text)') IS NOT NULL
   AND (SELECT count(*) = 3 FROM pg_trigger
        WHERE NOT tgisinternal AND tgenabled IN ('O','A') AND
          ((tgrelid=to_regclass('public.sm_inputs') AND tgname='trg_sm_inputs_after_insert') OR
           (tgrelid=to_regclass('public.sm_jobs') AND tgname IN ('trg_sm_jobs_notify','trg_sm_jobs_status_audit'))))
   AS schema_ok \gset
\if :schema_ok
\else
    DO $$ BEGIN RAISE EXCEPTION 'fresh bootstrap schema check failed'; END $$;
\endif
BEGIN;
SELECT * FROM sm_create_input('fresh-bootstrap-probe','{"tool":"echo-test"}') \gset
SELECT :'input_id' AS input_id, :'process_id' AS process_id, :'status' AS status;
SELECT count(*) = 1 AND coalesce(bool_and(p.state='init' AND j.status='pending' AND j.tool_name='echo-test'),false)
   AND :'status'='new' AS probe_ok
FROM sm_processes p JOIN sm_jobs j ON j.process_id=p.id
WHERE p.id=:'process_id' AND p.input_id=:'input_id' \gset
\if :probe_ok
\else
    DO $$ BEGIN RAISE EXCEPTION 'fresh bootstrap smoke input did not enqueue one pending job'; END $$;
\endif
ROLLBACK;
SQL
echo 'FRESH BOOTSTRAP PASSED'
