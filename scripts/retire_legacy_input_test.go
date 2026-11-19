package scripts_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRetireLegacyInputMigration(t *testing.T) {
	if os.Getenv("PETAR_T28_DRILL") != "1" {
		t.Skip("set PETAR_T28_DRILL=1 for the isolated PG18 migration drill")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	root, err := filepath.Abs("..")
	if err != nil || root != "/opt/go-pet" {
		t.Fatalf("T28 drill requires /opt/go-pet: %q, %v", root, err)
	}
	data, err := os.MkdirTemp(filepath.Join(root, "postgres"), ".t28-migration.")
	if err != nil {
		t.Fatal(err)
	}
	const image = "pgvector/pgvector:pg18"
	docker := func(ctx context.Context, input string, args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "docker", args...)
		cmd.Stdin = strings.NewReader(input)
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	var container string
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if container != "" {
			if out, err := docker(ctx, "", "rm", "-f", container); err != nil {
				t.Errorf("remove test container: %v: %s", err, out)
				return
			}
			if out, err := docker(ctx, "", "run", "--rm", "--network", "none", "--entrypoint", "sh",
				"--mount", "type=bind,src="+data+",dst=/cleanup", image, "-c", "rm -rf -- /cleanup/18"); err != nil {
				t.Errorf("clean test PGDATA: %v: %s", err, out)
			}
		}
		if err := os.Remove(data); err != nil {
			t.Error(err)
		}
	})
	if err := os.Chmod(data, 0755); err != nil {
		t.Fatal(err)
	}
	out, err := docker(ctx, "", "create", "--network", "none",
		"-e", "POSTGRES_USER=petar_user", "-e", "POSTGRES_PASSWORD=petar_password",
		"-e", "POSTGRES_DB=t28", "--mount", "type=bind,src="+data+",dst=/var/lib/postgresql", image)
	if err != nil {
		t.Fatalf("create isolated PG18: %v: %s", err, out)
	}
	container = out
	if out, err := docker(ctx, "", "start", container); err != nil {
		t.Fatalf("start isolated PG18: %v: %s", err, out)
	}
	psql := func(ctx context.Context, sql string) (string, error) {
		return docker(ctx, sql, "exec", "-i", "-e", "PGPASSWORD=petar_password", container,
			"psql", "-XqAt", "-h", "127.0.0.1", "-U", "petar_user", "-d", "t28", "-v", "ON_ERROR_STOP=1", "-1", "-f", "-")
	}
	const version = "SELECT current_setting('server_version_num')::int BETWEEN 180000 AND 189999;"
	readyCtx, stopReady := context.WithTimeout(ctx, time.Minute)
	defer stopReady()
	for {
		out, err := psql(readyCtx, version)
		if err == nil && out == "t" {
			break
		}
		select {
		case <-readyCtx.Done():
			logs, _ := docker(ctx, "", "logs", container)
			t.Fatalf("isolated PG18 not ready: %v: %s\n%s", err, out, logs)
		case <-time.After(time.Second):
		}
	}
	stopReady()
	query := func(ctx context.Context, sql string) string {
		t.Helper()
		out, err := psql(ctx, sql)
		if err != nil {
			t.Fatalf("SQL failed: %v: %s\n%s", err, out, sql)
		}
		return out
	}
	expect := func(ctx context.Context, sql, want string) {
		t.Helper()
		if got := query(ctx, sql); got != want {
			t.Fatalf("%s: got %q, want %q", sql, got, want)
		}
	}
	apply := func(ctx context.Context, path string) {
		t.Helper()
		sql, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		query(ctx, string(sql))
	}
	migrations := filepath.Join(root, "postgres", "migrations")
	up, err := filepath.Glob(filepath.Join(migrations, "*.up.sql"))
	if err != nil || len(up) < 7 {
		t.Fatalf("missing baseline migrations: %v, %v", up, err)
	}
	for _, path := range up[:7] {
		apply(ctx, path)
	}
	query(ctx, `
DO $$
DECLARE
    fixture text;
    job uuid;
BEGIN
    FOREACH fixture IN ARRAY ARRAY['completed', 'missing', 'failed', 'dispatched', 'pending'] LOOP
        PERFORM public.sm_create_input('legacy-' || fixture,
            jsonb_build_object('tool','echo-test','message',fixture));
        IF fixture = 'pending' THEN CONTINUE; END IF;
        SELECT id INTO STRICT job FROM public.sm_pick_next_job();
        IF fixture = 'dispatched' THEN CONTINUE; END IF;
        PERFORM public.sm_update_job_status(job, 'in_progress');
        IF fixture = 'failed' THEN
            PERFORM public.sm_update_job_status(job, 'failed', NULL, 'test failure');
        ELSE
            PERFORM public.sm_update_job_status(job, 'completed', '/spool/out/' || job || '/result.json');
        END IF;
    END LOOP;
END $$;`)
	expect(ctx, `SELECT string_agg(i.source||':'||j.status||':'||j.action_version, ',' ORDER BY i.source)
FROM sm_inputs i JOIN sm_processes p ON p.input_id=i.id JOIN sm_jobs j ON j.process_id=p.id;`,
		"legacy-completed:completed:1,legacy-dispatched:dispatched:1,legacy-failed:failed:1,legacy-missing:completed:1,legacy-pending:pending:1")
	const counts = `SELECT concat_ws('/', (SELECT count(*) FROM sm_inputs), (SELECT count(*) FROM sm_processes),
(SELECT count(*) FROM sm_jobs), (SELECT count(*) FROM sm_outputs), (SELECT count(*) FROM sm_job_logs));`
	expect(ctx, counts, "5/5/5/2/10")
	spool := t.TempDir()
	paths := map[string]string{
		"in/" + query(ctx, `SELECT id FROM sm_jobs WHERE status='dispatched';`) + ".json": query(ctx, `
SELECT to_jsonb(j) - 'action_version' FROM sm_jobs j WHERE status='dispatched';`),
		"out/" + query(ctx, `SELECT id FROM sm_jobs WHERE payload->>'message'='completed';`) + "/public.json": "{\"type\":\"echo-test\",\"version\":1,\"data\":{\"echo\":\"completed\"}}\n",
	}
	for path, content := range paths {
		full := filepath.Join(spool, path)
		if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := func(ctx context.Context) []string {
		t.Helper()
		var result []string
		for _, table := range []string{"sm_inputs", "sm_processes", "sm_jobs", "sm_job_logs", "sm_outputs", "sm_actions"} {
			result = append(result, query(ctx, "SELECT to_jsonb(t) FROM public."+table+" t ORDER BY to_jsonb(t);"))
		}
		for _, path := range []string{
			"in/" + query(ctx, `SELECT id FROM sm_jobs WHERE status='dispatched';`) + ".json",
			"out/" + query(ctx, `SELECT id FROM sm_jobs WHERE payload->>'message'='completed';`) + "/public.json",
		} {
			body, err := os.ReadFile(filepath.Join(spool, path))
			if err != nil {
				t.Fatal(err)
			}
			result = append(result, string(body))
		}
		return result
	}
	before := snapshot(ctx)
	const actionDefinition = "SELECT pg_get_functiondef('public.sm_create_action(text,integer,jsonb)'::regprocedure);"
	const legacyDefinition = "SELECT pg_get_functiondef('public.sm_create_input(text,jsonb)'::regprocedure);"
	oldAction, oldLegacy := query(ctx, actionDefinition), query(ctx, legacyDefinition)
	query(ctx, `CREATE ROLE t28_caller NOLOGIN;
GRANT USAGE ON SCHEMA public TO t28_caller;
GRANT SELECT, INSERT ON sm_inputs, sm_processes, sm_jobs TO t28_caller;
GRANT SELECT ON sm_actions TO t28_caller;
REVOKE EXECUTE ON FUNCTION public.sm_create_action(text,integer,jsonb) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.sm_create_action(text,integer,jsonb) TO t28_caller;`)
	const actionACL = `SELECT proacl FROM pg_proc WHERE oid='public.sm_create_action(text,integer,jsonb)'::regprocedure;`
	oldACL := query(ctx, actionACL)
	for _, path := range up[7:] {
		apply(ctx, path)
	}
	if !reflect.DeepEqual(snapshot(ctx), before) {
		t.Fatal("migration changed historical rows, audit, catalog or spool bytes")
	}
	t.Log("baseline fixtures validated; history and spool byte snapshot match")
	const retired = "SELECT to_regprocedure('public.sm_create_input(text,jsonb)') IS NULL;"
	expect(ctx, retired, "t")
	expect(ctx, actionACL, oldACL)
	const verifyChain = `
SELECT count(*) FROM sm_inputs i JOIN sm_processes p ON p.input_id=i.id
JOIN sm_jobs j ON j.process_id=p.id WHERE i.source='%s'
AND i.status='new' AND p.state='init' AND j.status='pending' AND j.action_version=1
AND j.tool_name=i.payload->>'tool' AND j.payload=i.payload AND i.payload->>'message'='%s';`
	expect(ctx, `SET ROLE t28_caller;
SELECT count(*) FROM public.sm_create_action('echo-test',1,'{"message":"after-up"}');`, "1")
	expect(ctx, fmt.Sprintf(verifyChain, "api", "after-up"), "1")

	// Each refusal must be 22023 and leave all tables (including audit) unchanged.
	query(ctx, `UPDATE public.sm_actions SET enabled=false WHERE action='fabric' AND version=1;`)
	withDisabled := snapshot(ctx)
	query(ctx, `DO $$
DECLARE stmt text;
BEGIN
    FOREACH stmt IN ARRAY ARRAY[
        $call$SELECT public.sm_create_action('echo-test',1,'null')$call$,
        $call$SELECT public.sm_create_action('echo-test',1,'{"tool":"fabric"}')$call$,
        $call$SELECT public.sm_create_action('echo-test',1,'{"message":false}')$call$,
        $call$SELECT public.sm_create_action('unknown',1,'{}')$call$,
        $call$SELECT public.sm_create_action('echo-test',2,'{}')$call$,
        $call$SELECT public.sm_create_action('fabric',1,'{"text":"valid but disabled"}')$call$
    ] LOOP
        BEGIN
            EXECUTE stmt;
            RAISE EXCEPTION 'invalid action accepted: %', stmt;
        EXCEPTION WHEN SQLSTATE '22023' THEN
            NULL;
        END;
    END LOOP;
END $$;`)
	if !reflect.DeepEqual(snapshot(ctx), withDisabled) {
		t.Fatal("refused actions changed the snapshot")
	}
	afterUp := snapshot(ctx)
	const migration = "000008_retire_legacy_input"
	apply(ctx, filepath.Join(migrations, migration+".down.sql"))
	expect(ctx, retired, "f")
	expect(ctx, actionDefinition, oldAction)
	expect(ctx, legacyDefinition, oldLegacy)
	expect(ctx, actionACL, oldACL)
	if !reflect.DeepEqual(snapshot(ctx), afterUp) {
		t.Fatal("down changed historical or newly enqueued data")
	}
	expect(ctx, `SELECT count(*) FROM public.sm_create_input('restored','{"tool":"echo-test","message":"after-down"}');`, "1")
	expect(ctx, fmt.Sprintf(verifyChain, "restored", "after-down"), "1")
	expect(ctx, `SET ROLE t28_caller;
SELECT count(*) FROM public.sm_create_action('echo-test',1,'{"message":"action-after-down"}');`, "1")
	expect(ctx, fmt.Sprintf(verifyChain, "api", "action-after-down"), "1")
	afterDown := snapshot(ctx)
	apply(ctx, filepath.Join(migrations, migration+".up.sql"))
	expect(ctx, retired, "t")
	expect(ctx, actionACL, oldACL)
	if !reflect.DeepEqual(snapshot(ctx), afterDown) {
		t.Fatal("second up changed historical or newly enqueued data")
	}
	expect(ctx, counts, "8/8/8/2/10")
	t.Log("up/down/up preserved data and existing action ACL; both down entry points worked")
}
