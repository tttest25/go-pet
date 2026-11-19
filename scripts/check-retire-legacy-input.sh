#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)
[[ "$root" == /opt/go-pet && $(realpath "$root/postgres") == "$root/postgres" ]] ||
    { echo 'T28 drill requires /opt/go-pet and its local postgres directory' >&2; exit 1; }
cd "$root"

fail() { printf 'T28: %s\n' "$*" >&2; exit 1; }
for tool in docker go curl jq python3 flock tar timeout; do
    command -v "$tool" >/dev/null || fail "missing tool: $tool"
done
pg_image=pgvector/pgvector:pg18
runtime=alpine:3.20
docker info >/dev/null
for image in "$pg_image" "$runtime"; do
    docker image inspect "$image" >/dev/null || fail "required local image: $image (this drill does not pull images)"
done

work= data= network= source_pg= restored_pg= app= relay=
cleanup() {
    local status=$? id pg_removed=true
    trap - ERR
    set +e
    if ((status)); then
        for id in "$app" "$relay" "$restored_pg" "$source_pg"; do
            [[ -z "$id" ]] || docker logs --tail 30 "$id" >&2
        done
    fi
    for id in "$relay" "$app" "$restored_pg" "$source_pg"; do
        if [[ -n "$id" ]] && ! docker rm -fv "$id" >/dev/null; then
            status=1
            if [[ "$id" == "$restored_pg" || "$id" == "$source_pg" ]]; then pg_removed=false; fi
        fi
    done
    [[ -z "$network" ]] || docker network rm "$network" >/dev/null || status=1
    if [[ -n "$data" ]]; then
        if "$pg_removed" && docker run --rm --pull=never --network none --entrypoint sh \
            --mount "type=bind,src=$data,dst=/cleanup" "$pg_image" \
            -c 'rm -rf -- /cleanup/source/18 /cleanup/restored/18'; then
            rm -rf -- "$data" || status=1
        else
            printf 'T28: could not remove isolated PGDATA in %s\n' "$data" >&2
            status=1
        fi
    fi
    [[ -z "$work" ]] || rm -rf -- "$work" || status=1
    if ((status == 0)); then echo 'RETIRE LEGACY INPUT DRILL PASSED'; fi
    exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'printf "T28: failed at line %s: %s\n" "$LINENO" "$BASH_COMMAND" >&2' ERR
scratch=/tmp/opencode
[[ -d "$scratch" ]] || scratch=${TMPDIR:-/tmp}
work=$(mktemp -d "$scratch/t28.XXXXXXXX")
data=$(mktemp -d "$root/postgres/.t28-cutover.XXXXXXXX")
mkdir -- "$data/source" "$data/restored" "$work/bin" "$work/control" "$work/restored"
# The entrypoint changes to postgres after setting ownership of its versioned
# PGDATA, so that user must be able to traverse the bind-mount parent.
chmod 0755 "$data/source" "$data/restored"
source_spool="$work/source"
spool="$work/restored"
mkdir -p -- "$source_spool"/{in,processing,out}

CGO_ENABLED=0 GOOS=linux go build -o "$work/bin/petar" ./cmd/petar
# Let real host curl and the real host runner reach only this isolated app:
# Unix socket in a private bind mount -> in-network HTTP, no host TCP listener.
cat > "$work/relay.go" <<'GO'
package main

import (
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"
)

func main() {
	listener, err := net.Listen("unix", "/control/http.sock")
	if err != nil {
		log.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(&url.URL{Scheme: "http", Host: "petar:8080"})
	proxy.Transport = &http.Transport{} // No inherited HTTP proxy.
	server := &http.Server{Handler: proxy, ReadHeaderTimeout: 3 * time.Second}
	log.Fatal(server.Serve(listener))
}
GO
CGO_ENABLED=0 GOOS=linux go build -o "$work/bin/relay" "$work/relay.go"

network=$(docker network create --internal "petar-${work##*/}")
start_pg() {
    docker create --pull=never --network "$network" --network-alias "$1" \
        -e "POSTGRES_USER=$2" -e POSTGRES_PASSWORD=t28-only -e "POSTGRES_DB=$3" \
        --mount "type=bind,src=$data/$1,dst=/var/lib/postgresql" "$pg_image" -c io_method=worker
}
source_pg=$(start_pg source t28_owner t28)
restored_pg=$(start_pg restored t28_restore_admin postgres)
[[ $(docker network inspect -f '{{.Internal}}' "$network") == true ]]
no_ports() {
    [[ $(docker inspect -f '{{len .HostConfig.PortBindings}}' "$1") == 0 ]]
}
no_ports "$source_pg"
no_ports "$restored_pg"

psql_exec() {
    local container=$1 user=$2 database=$3
    shift 3
    timeout 30 docker exec -i -e PGPASSWORD=t28-only "$container" \
        psql -XqAt -h 127.0.0.1 -U "$user" -d "$database" -v ON_ERROR_STOP=1 "$@"
}
wait_until() {
    local label=$1 deadline=$((SECONDS + 45))
    shift
    until "$@"; do
        ((SECONDS < deadline)) || fail "timed out waiting for $label"
        sleep 0.25
    done
}
pg_ready() {
    local version
    version=$(timeout 3 docker exec -e PGPASSWORD=t28-only "$1" \
        psql -XqAt -h 127.0.0.1 -U "$2" -d "$3" -c 'SHOW server_version_num' 2>/dev/null) &&
        [[ "$version" =~ ^18[0-9]{4}$ ]]
}
docker start "$source_pg" >/dev/null
wait_until 'source PostgreSQL 18' pg_ready "$source_pg" t28_owner t28
pg=$source_pg admin=t28_owner
psql_admin() { psql_exec "$pg" "$admin" t28 "$@"; }
query() { psql_admin -c "$1"; }
expect() {
    local actual
    actual=$(query "$1")
    [[ "$actual" == "$2" ]] || fail "expected [$2], got [$actual] for $1"
}
apply() { psql_admin -1 -f - < "$1"; }

baseline=(postgres/migrations/00000[1-7]_*.up.sql)
[[ ${#baseline[@]} == 7 ]] || fail 'expected exactly seven baseline up files'
for file in "${baseline[@]}"; do apply "$file"; done
psql_admin -1 -f - <<'SQL'
CREATE ROLE t28_app LOGIN PASSWORD 't28-only';
REVOKE CONNECT ON DATABASE t28 FROM PUBLIC;
GRANT CONNECT ON DATABASE t28 TO t28_app;
GRANT USAGE ON SCHEMA public TO t28_app;
GRANT SELECT ON ALL TABLES IN SCHEMA public TO t28_app;
GRANT INSERT ON sm_inputs, sm_processes, sm_jobs, sm_job_logs, sm_outputs TO t28_app;
GRANT UPDATE ON sm_jobs, sm_processes TO t28_app;
GRANT USAGE ON ALL SEQUENCES IN SCHEMA public TO t28_app;
REVOKE EXECUTE ON FUNCTION public.sm_create_input(text,jsonb), public.sm_create_action(text,integer,jsonb) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.sm_create_input(text,jsonb), public.sm_create_action(text,integer,jsonb) TO t28_app;

DO $$
DECLARE
    fixture text;
    job uuid;
BEGIN
    FOREACH fixture IN ARRAY ARRAY['completed', 'missing', 'failed', 'dispatched', 'pending'] LOOP
        PERFORM public.sm_create_input('legacy-' || fixture,
            jsonb_build_object('tool','word-count','text',fixture || ' one two'));
        IF fixture = 'pending' THEN CONTINUE; END IF;
        SELECT id INTO STRICT job FROM public.sm_pick_next_job();
        IF fixture = 'dispatched' THEN CONTINUE; END IF;
        PERFORM public.sm_update_job_status(job, 'in_progress');
        IF fixture = 'failed' THEN
            PERFORM public.sm_update_job_status(job, 'failed', NULL, 'historical failure');
        ELSE
            PERFORM public.sm_update_job_status(job, 'completed', '/spool/out/' || job || '/result.json');
        END IF;
    END LOOP;
END $$;
SQL
job_id() { query "SELECT j.id FROM sm_jobs j JOIN sm_processes p ON j.process_id=p.id
    JOIN sm_inputs i ON p.input_id=i.id WHERE i.source='legacy-$1'"; }
is_uuid() { [[ "$1" =~ ^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$ ]]; }
completed=$(job_id completed) missing=$(job_id missing) failed=$(job_id failed)
dispatched=$(job_id dispatched) pending=$(job_id pending)
for id in "$completed" "$missing" "$failed" "$dispatched" "$pending"; do
    is_uuid "$id"
done
query "SELECT jsonb_build_object('id',id,'process_id',process_id,'tool_name',tool_name,
    'payload',payload,'old_status','pending','status',status) FROM sm_jobs WHERE id='$dispatched'" \
    > "$source_spool/in/$dispatched.json"
jq -e --arg id "$dispatched" '.id == $id and (has("action_version") | not)' "$source_spool/in/$dispatched.json" >/dev/null
mkdir -- "$source_spool/out/$completed" "$source_spool/out/$failed"
printf '  {"version":1,"data":{"words":3},"type":"word-count"}\n\n' > "$source_spool/out/$completed/public.json"
printf '{"words":3}\n' > "$source_spool/out/$completed/result.json"
printf 'test fixture error output\n' > "$source_spool/out/$failed/stderr.log"

history() {
    local closed_jobs=TRUE closed_processes=TRUE closed_inputs=TRUE closed_logs=TRUE closed_outputs=TRUE
    if [[ "$1" == closed ]]; then
        closed_jobs="id IN ('$completed','$missing','$failed')"
        closed_processes='id IN (SELECT process_id FROM jobs)'
        closed_inputs='id IN (SELECT input_id FROM processes)'
        closed_logs='job_id IN (SELECT id FROM jobs)'
        closed_outputs='process_id IN (SELECT id FROM processes)'
    fi
    query "WITH jobs AS (SELECT * FROM sm_jobs WHERE $closed_jobs),
    processes AS (SELECT * FROM sm_processes WHERE $closed_processes),
    inputs AS (SELECT * FROM sm_inputs WHERE $closed_inputs),
    logs AS (SELECT * FROM sm_job_logs WHERE $closed_logs),
    outputs AS (SELECT * FROM sm_outputs WHERE $closed_outputs)
    SELECT jsonb_build_object(
      'inputs',(SELECT jsonb_agg(to_jsonb(t) ORDER BY id) FROM inputs t),
      'processes',(SELECT jsonb_agg(to_jsonb(t) ORDER BY id) FROM processes t),
      'jobs',(SELECT jsonb_agg(to_jsonb(t) ORDER BY id) FROM jobs t),
      'logs',(SELECT jsonb_agg(to_jsonb(t) ORDER BY id) FROM logs t),
      'outputs',(SELECT jsonb_agg(to_jsonb(t) ORDER BY id) FROM outputs t),
      'catalog',(SELECT jsonb_agg(to_jsonb(t) ORDER BY action,version) FROM sm_actions t));"
}
identities() {
    query "SELECT jsonb_build_array(to_jsonb(i), to_jsonb(p) - ARRAY['state','updated_at'],
        to_jsonb(j) - ARRAY['status','updated_at','result_path','error_msg'])
        FROM sm_jobs j JOIN sm_processes p ON p.id=j.process_id JOIN sm_inputs i ON i.id=p.input_id
        WHERE j.id IN ('$completed','$missing','$failed','$pending','$dispatched') ORDER BY j.id;"
}
counts="SELECT concat_ws('/',(SELECT count(*) FROM sm_inputs),(SELECT count(*) FROM sm_processes),
    (SELECT count(*) FROM sm_jobs),(SELECT count(*) FROM sm_outputs),(SELECT count(*) FROM sm_job_logs))"
expect "$counts" 5/5/5/2/10
expect "SELECT string_agg(i.source||':'||j.status||':'||j.action_version, ',' ORDER BY i.source)
    FROM sm_inputs i JOIN sm_processes p ON p.input_id=i.id JOIN sm_jobs j ON j.process_id=p.id" \
    'legacy-completed:completed:1,legacy-dispatched:dispatched:1,legacy-failed:failed:1,legacy-missing:completed:1,legacy-pending:pending:1'
audit_before=$(query 'SELECT max(id) FROM sm_job_logs')
history all > "$data/history"
history closed > "$data/closed"
identities > "$data/identities"
[[ $(find "$source_spool/processing" -mindepth 1 -print) == '' ]]

pg_archive() {
    timeout 30 docker exec -i -e PGPASSWORD=t28-only "$pg" "$@"
}
# A fresh logical archive including DB owner/ACL and a separate globals dump.
# No process can write here during backup; neither app nor runner exists yet.
pg_archive pg_dumpall -h 127.0.0.1 -U t28_owner --globals-only > "$data/globals.sql"
pg_archive pg_dump -h 127.0.0.1 -U t28_owner -Fc -d t28 > "$data/t28.dump"
tar -C "$source_spool" -cpf "$work/spool.tar" in processing out

# Compare schema definitions, ownership and ACLs as well as rows.
# pg_dump generates random psql restrict keys; they are not schema/ACL state.
schema_snapshot() {
    pg_archive pg_dump -h 127.0.0.1 -U "$admin" --schema-only --create -d t28 |
        sed -E '/^\\(un)?restrict /d'
}
schema_snapshot > "$data/schema.sql"
docker stop -t 5 "$source_pg" >/dev/null
docker start "$restored_pg" >/dev/null
wait_until 'fresh restore PostgreSQL 18' pg_ready "$restored_pg" t28_restore_admin postgres
pg=$restored_pg admin=t28_restore_admin
[[ $(psql_exec "$pg" "$admin" postgres -c "SELECT count(*) FROM pg_database WHERE datname='t28'") == 0 ]]
psql_exec "$pg" "$admin" postgres -f - < "$data/globals.sql"
pg_archive pg_restore -h 127.0.0.1 -U "$admin" --exit-on-error --create -d postgres < "$data/t28.dump"
tar -C "$spool" -xpf "$work/spool.tar"
tar -C "$spool" -df "$work/spool.tar"
diff -r -- "$source_spool" "$spool"
history all | diff -u "$data/history" -
schema_snapshot | diff -u "$data/schema.sql" -
[[ $(psql_exec "$pg" t28_app t28 -f - <<'SQL'
BEGIN;
SELECT count(*) FROM public.sm_create_input('restore-probe','{"tool":"word-count","text":"old"}');
SELECT count(*) FROM public.sm_create_action('word-count',1,'{"text":"old"}');
ROLLBACK;
SQL
) == $'1\n1' ]]
history all | diff -u "$data/history" -
echo 'T28: fresh restore matched complete rows, schema/ownership/ACLs and spool/result bytes; restricted role used both old entry points'

action_acl="SELECT concat_ws('|',pg_get_userbyid(proowner),proacl::text) FROM pg_proc
    WHERE oid='public.sm_create_action(text,integer,jsonb)'::regprocedure"
saved_acl=$(query "$action_acl")
apply postgres/migrations/000008_retire_legacy_input.up.sql
expect "SELECT to_regprocedure('public.sm_create_input(text,jsonb)') IS NULL" t
expect "$action_acl" "$saved_acl"
history all | diff -u "$data/history" -
diff -r -- "$source_spool" "$spool"

# petar starts HTTP and worker together. A temporary SHARE lock keeps job
# writes frozen while allowing result reads; the runner is not started yet.
freeze_app=t28-drill-freeze
docker exec -d -e PGPASSWORD=t28-only -e "PGAPPNAME=$freeze_app" "$pg" \
    psql -Xq -h 127.0.0.1 -U "$admin" -d t28 -v ON_ERROR_STOP=1 \
    -c 'BEGIN; LOCK TABLE public.sm_jobs IN SHARE MODE; SELECT pg_sleep(180); ROLLBACK;'
freeze_pid_sql="SELECT pid FROM pg_locks l JOIN pg_stat_activity a USING (pid)
    WHERE a.application_name='$freeze_app' AND l.relation='public.sm_jobs'::regclass
    AND l.mode='ShareLock' AND l.granted"
freeze_held() { [[ -n $(query "$freeze_pid_sql") ]]; }
wait_until 'queue write freeze' freeze_held

uid_gid="$(id -u):$(id -g)"
app=$(docker create --pull=never --network "$network" --network-alias petar --user "$uid_gid" \
    --mount "type=bind,src=$work/bin,dst=/drill-bin,readonly" \
    --mount "type=bind,src=$spool,dst=/drill-spool" \
    -e DB_DSN='postgres://t28_app:t28-only@restored:5432/t28?sslmode=disable' \
    -e SPOOL_DIR=/drill-spool -e HTTP_ADDR=0.0.0.0:8080 -e POLL_INTERVAL=1s \
    --entrypoint /drill-bin/petar "$runtime")
relay=$(docker create --pull=never --network "$network" --user "$uid_gid" \
    --mount "type=bind,src=$work/bin,dst=/drill-bin,readonly" \
    --mount "type=bind,src=$work/control,dst=/control" \
    --entrypoint /drill-bin/relay "$runtime")
no_ports "$app"
no_ports "$relay"
docker start "$app" "$relay" >/dev/null

export DRILL_CURL=$(command -v curl) DRILL_TIMEOUT=$(command -v timeout) DRILL_SOCKET="$work/control/http.sock"
export DRILL_RUNS="$work/runs" SPOOL_DIR="$spool" API_BASE_URL=http://petar:8080/api/v1
: > "$DRILL_RUNS"
cat > "$work/bin/curl" <<'BASH'
#!/usr/bin/env bash
exec "$DRILL_CURL" -q --noproxy '*' --unix-socket "$DRILL_SOCKET" "$@"
BASH
cat > "$work/bin/timeout" <<'BASH'
#!/usr/bin/env bash
set -euo pipefail
# Observe the real tool execution, not the separate action-version validation.
if [[ $# == 5 && $1 == --kill-after=1s && $3 == python3 && $4 == - && $5 == "$SPOOL_DIR"/processing/*.json ]]; then
    job=${5##*/}
    printf '%s\n' "${job%.json}" >> "$DRILL_RUNS"
fi
exec "$DRILL_TIMEOUT" "$@"
BASH
chmod +x "$work/bin/curl" "$work/bin/timeout"
app_ready() { "$work/bin/curl" -fs --max-time 2 http://petar:8080/readyz >/dev/null; }
wait_until 'petar readiness through isolated relay' app_ready
http_expect() {
    local want=$1 code
    shift
    code=$("$work/bin/curl" -sS --max-time 10 -o "$work/body" -w '%{http_code}' "$@")
    [[ "$code" == "$want" ]] || fail "expected HTTP $want, got $code for $*"
}
http_expect 200 "$API_BASE_URL/jobs/$completed/result"
cmp -- "$source_spool/out/$completed/public.json" "$work/body"
http_expect 409 "$API_BASE_URL/jobs/$failed/result"
jq -e '. == {"error":"job_failed"}' "$work/body" >/dev/null
http_expect 410 "$API_BASE_URL/jobs/$missing/result"
jq -e '. == {"error":"result_unavailable"}' "$work/body" >/dev/null
http_expect 404 -H 'Content-Type: application/json' \
    -d '{"source":"retired","payload":{"tool":"word-count","text":"should not run"}}' "$API_BASE_URL/inputs"
history all | diff -u "$data/history" -
diff -r -- "$source_spool" "$spool"
freeze_held
expect "SELECT pg_terminate_backend(pid) FROM ($freeze_pid_sql) AS lock_holder" t
echo 'T28: post-up frozen history matched; completed 200 byte-for-byte, failed 409, missing 410, legacy POST 404'

published() { [[ -f "$spool/in/$1.json" ]]; }
run_once() {
    PATH="$work/bin:$PATH" RUNNER_TOOL_TIMEOUT=10 timeout 30 bash "$root/scripts/spool-runner.sh"
}
execution_evidence() {
    local id
    printf '%s\n' "$@" | sort | diff -u - <(sort "$DRILL_RUNS")
    # Count all HTTP requests to status paths, including any non-204 duplicates.
    for id in "$@"; do printf '/api/v1/jobs/%s/status 204\n' "$id" "$id"; done | sort > "$work/expected-callbacks"
    docker logs "$app" | jq -r 'select(.msg == "http request" and (.path | endswith("/status"))) |
        "\(.path) \(.status)"' | sort | diff -u "$work/expected-callbacks" -
    [[ $(find "$spool/in" "$spool/processing" -mindepth 1 -print) == '' ]]
}
completed_result() {
    local id=$1
    expect "SELECT concat_ws('/',j.status,p.state,j.action_version,o.artifact_type,
        (o.metadata=jsonb_build_object('result_path',j.result_path))::text,(o.content IS NULL)::text)
        FROM sm_jobs j JOIN sm_processes p ON j.process_id=p.id
        JOIN sm_outputs o ON o.process_id=p.id WHERE j.id='$id'
          AND j.result_path='/spool/out/$id/result.json'" 'completed/completed/1/result/true/true'
    http_expect 200 "$API_BASE_URL/jobs/$id/result"
    jq -e '. == {"type":"word-count","version":1,"data":{"words":3}}' "$work/body" >/dev/null
}
audit_transitions() {
    expect "SELECT string_agg(old_status||'>'||new_status, ',' ORDER BY id)
        FROM sm_job_logs WHERE id > $audit_before AND job_id='$1' AND message IS NULL" "$2"
}
wait_until 'publication of old pending job' published "$pending"
jq -e --arg id "$pending" '.id == $id and .action_version == 1' "$spool/in/$pending.json" >/dev/null
cmp -- "$source_spool/in/$dispatched.json" "$spool/in/$dispatched.json"
run_once
expect "$counts" 5/5/5/4/15
execution_evidence "$pending" "$dispatched"
audit_transitions "$pending" 'pending>dispatched,dispatched>in_progress,in_progress>completed'
audit_transitions "$dispatched" 'dispatched>in_progress,in_progress>completed'
completed_result "$pending"
completed_result "$dispatched"
identities | diff -u "$data/identities" -
history closed | diff -u "$data/closed" -
for id in "$completed" "$failed"; do diff -r -- "$source_spool/out/$id" "$spool/out/$id"; done
history all > "$data/drained"
sleep 2 # Give the one-second fallback poll another opportunity to dispatch.
run_once
history all | diff -u "$data/drained" -
execution_evidence "$pending" "$dispatched"
echo "T28: old pending $pending and unversioned dispatched $dispatched each ran once and received two callbacks; empty second pass"

http_expect 201 -H 'Content-Type: application/json' \
    -d '{"action":"word-count","version":1,"data":{"text":"new one two"}}' "$API_BASE_URL/actions"
jq -e '.status == "new" and .id == .input_id and (.input_id | type == "string") and (.process_id | type == "string")' "$work/body" >/dev/null
input_id=$(jq -r .input_id "$work/body")
process_id=$(jq -r .process_id "$work/body")
is_uuid "$input_id"
is_uuid "$process_id"
new=$(query "SELECT j.id FROM sm_jobs j JOIN sm_processes p ON j.process_id=p.id
    JOIN sm_inputs i ON p.input_id=i.id WHERE p.id='$process_id' AND i.id='$input_id'
    AND i.source='api' AND j.tool_name='word-count' AND j.action_version=1
    AND i.payload=j.payload AND i.payload='{\"tool\":\"word-count\",\"text\":\"new one two\"}'")
is_uuid "$new"
wait_until 'publication of new action job' published "$new"
run_once
expect "$counts" 6/6/6/5/18
audit_transitions "$new" 'pending>dispatched,dispatched>in_progress,in_progress>completed'
completed_result "$new"
execution_evidence "$pending" "$dispatched" "$new"
identities | diff -u "$data/identities" -
history closed | diff -u "$data/closed" -
echo "T28: new POST 201 created one input/process/job chain; job $new ran once with two confirmed callbacks"
