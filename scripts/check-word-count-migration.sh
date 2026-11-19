#!/usr/bin/env bash
set -euo pipefail

root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
if [[ "$root" != /opt/go-pet ]]; then
    echo 'Word-count migration drill requires this checkout at /opt/go-pet' >&2
    exit 1
fi
image=pgvector/pgvector:pg18
data=$(mktemp -d "$root/postgres/.word-count-migration.XXXXXXXX")
container=
cleanup() {
    if [[ -n "$container" ]]; then
        docker rm -f "$container" >/dev/null
        docker run --rm --network none --entrypoint sh \
            --mount "type=bind,src=$data,dst=/cleanup" "$image" \
            -c 'rm -rf -- /cleanup/18'
    fi
    rmdir -- "$data"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

chmod 0755 "$data"
container=$(docker create --network none \
    -e POSTGRES_USER=petar_user -e POSTGRES_PASSWORD=petar_password -e POSTGRES_DB=word_count_drill \
    --mount "type=bind,src=$data,dst=/var/lib/postgresql" "$image")
docker start "$container" >/dev/null
psql=(docker exec -i -e PGPASSWORD=petar_password "$container"
    psql -X -h 127.0.0.1 -U petar_user -d word_count_drill -v ON_ERROR_STOP=1)
ready=false
deadline=$((SECONDS + 60))
while ((SECONDS < deadline)); do
    [[ $(docker inspect -f '{{.State.Running}}' "$container") == true ]] || break
    if timeout 3 "${psql[@]}" -Atc "SELECT current_setting('server_version_num')::int BETWEEN 180000 AND 189999" |
        grep -qx t; then
        ready=true
        break
    fi
    sleep 1
done 2>/dev/null
if ! "$ready"; then
    docker logs "$container" >&2
    echo 'Word-count migration drill: isolated PG18 not ready' >&2
    exit 1
fi

cd "$root/postgres/migrations"
apply() { timeout 30 "${psql[@]}" -1 -f - < "$1"; }
query() { timeout 10 "${psql[@]}" -Atc "$1"; }
expect() {
    local actual
    actual=$(query "$1")
    if [[ "$actual" != "$2" ]]; then
        printf 'Expected %s, got %s for %s\n' "$2" "$actual" "$1" >&2
        exit 1
    fi
}

for file in 00000[1-6]*.up.sql; do apply "$file"; done
query "SELECT * FROM public.sm_create_input('old-job','{\"tool\":\"echo-test\"}')"
old_job=$(query "SELECT id FROM public.sm_jobs WHERE tool_name='echo-test'")
[[ $old_job =~ ^[0-9a-f-]{36}$ ]]
constraint_query="SELECT pg_get_constraintdef(oid) FROM pg_constraint
 WHERE conrelid='public.sm_jobs'::regclass AND conname='sm_jobs_tool_name_check'"
validator_query="SELECT pg_get_functiondef('public.sm_validate_tool_payload(jsonb)'::regprocedure)"
old_constraint=$(query "$constraint_query")
old_validator=$(query "$validator_query")
query "INSERT INTO public.sm_actions VALUES ('word-count',2,'Custom version',false)"

apply 000007_word_count.up.sql
timeout 10 "${psql[@]}" -f - <<'SQL'
BEGIN;
DO $$
DECLARE
    p UUID;
BEGIN
    SELECT process_id INTO STRICT p FROM public.sm_create_action('word-count',1,'{"text":"some words"}');
    IF (SELECT count(*) FROM public.sm_jobs WHERE process_id=p) <> 1 OR NOT EXISTS (
        SELECT FROM public.sm_jobs WHERE process_id=p AND action_version=1 AND status='pending'
            AND tool_name='word-count' AND payload='{"tool":"word-count","text":"some words"}'
    ) THEN
        RAISE EXCEPTION 'word-count did not create exactly one pending v1 job';
    END IF;
END $$;
ROLLBACK;
SQL
apply 000007_word_count.down.sql
expect "$constraint_query" "$old_constraint"
expect "$validator_query" "$old_validator"
expect "SELECT status FROM public.sm_jobs WHERE id='$old_job' AND tool_name='echo-test'" pending
expect "SELECT string_agg(action||':'||version,',' ORDER BY action,version) FROM public.sm_actions" \
    'echo-test:1,fabric:1,word-count:2,yt-dlp:1'
timeout 10 "${psql[@]}" -f - <<'SQL'
BEGIN;
SELECT * FROM public.sm_create_action('echo-test',1,'{}');
SELECT * FROM public.sm_create_action('fabric',1,'{"text":"old rule"}');
SELECT * FROM public.sm_create_action('yt-dlp',1,'{"url":"https://example.org/video"}');
DO $$
BEGIN
    BEGIN
        PERFORM public.sm_create_action('word-count',1,'{"text":"some words"}');
        RAISE EXCEPTION 'word-count still accepted after down';
    EXCEPTION WHEN SQLSTATE '22023' THEN
        IF SQLERRM <> 'unsupported payload.tool' THEN RAISE; END IF;
    END;
END $$;
ROLLBACK;
SQL

apply 000007_word_count.up.sql
query "SELECT * FROM public.sm_create_action('word-count',1,'{\"text\":\"keep history\"}')"
word_job=$(query "SELECT id FROM public.sm_jobs WHERE tool_name='word-count'")
[[ $word_job =~ ^[0-9a-f-]{36}$ ]]
query "UPDATE public.sm_jobs SET status='dispatched' WHERE id='$word_job';
       SELECT public.sm_update_job_status('$word_job','completed','/spool/out/$word_job/result.json',NULL)"
expect "SELECT status FROM public.sm_jobs WHERE id='$word_job'" completed
job_query="SELECT to_jsonb(j) FROM public.sm_jobs j WHERE j.id='$word_job'"
catalog_query="SELECT jsonb_agg(to_jsonb(a) ORDER BY action,version) FROM public.sm_actions a"
saved_job=$(query "$job_query")
saved_catalog=$(query "$catalog_query")
saved_constraint=$(query "$constraint_query")
saved_validator=$(query "$validator_query")
if error=$(apply 000007_word_count.down.sql 2>&1); then
    echo 'Down unexpectedly accepted word-count history' >&2
    exit 1
fi
printf '%s\n' "$error"
[[ "$error" == *'cannot roll back 000007: word-count jobs exist'* ]]
expect "$job_query" "$saved_job"
expect "$catalog_query" "$saved_catalog"
expect "$constraint_query" "$saved_constraint"
expect "$validator_query" "$saved_validator"
expect "SELECT count(*) FROM public.sm_create_action('word-count',1,'{\"text\":\"still works\"}')" 1
expect "SELECT count(*) FROM public.sm_jobs WHERE tool_name='word-count' AND action_version=1" 2
echo 'WORD-COUNT MIGRATION PASSED'
