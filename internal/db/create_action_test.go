package db_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestCreateActionContract(t *testing.T) {
	env := newQueueTestEnv(t, "t24-db-"+uuid.NewString())
	// These cases share one guarded setup; each SQL contract transaction
	// rolls back before the next case starts.
	t.Run("enqueue", func(t *testing.T) { testActionEnqueue(t, env) })
	t.Run("dispatch from older snapshot", func(t *testing.T) { testActionDispatchVersion(t, env) })
	t.Run("version column", func(t *testing.T) { testActionVersionColumn(t, env) })
}

func testActionEnqueue(t *testing.T, env *queueTestEnv) {
	t.Helper()
	ctx := queueTestContext(t)
	tx, err := env.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())

	counts := func(t *testing.T) (result [3]int) {
		t.Helper()
		if err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM sm_inputs),
			(SELECT count(*) FROM sm_processes), (SELECT count(*) FROM sm_jobs)`).
			Scan(&result[0], &result[1], &result[2]); err != nil {
			t.Fatal(err)
		}
		return result
	}
	const create = `SELECT input_id, process_id, status FROM public.sm_create_action($1,$2,$3)`
	for _, tc := range []struct{ name, action, data, payload string }{
		{"echo-test", "echo-test", `{}`, `{"tool":"echo-test"}`},
		{"yt-dlp", "yt-dlp", `{"url":"https://example.org/video"}`, `{"tool":"yt-dlp","url":"https://example.org/video"}`},
		{"fabric", "fabric", `{"text":" body ","extra":{"n":9007199254740993}}`, `{"tool":"fabric","text":" body ","extra":{"n":9007199254740993}}`},
		{"word-count", "word-count", `{"text":"один два три"}`, `{"tool":"word-count","text":"один два три"}`},
		{"word-count preserved", "word-count", `{"text":"  padded text  ","extra":true}`, `{"tool":"word-count","text":"  padded text  ","extra":true}`},
		{"word-count empty", "word-count", `{"text":""}`, `{"tool":"word-count","text":""}`},
		{"word-count separators", "word-count", `{"text":" \n\t\u00a0\u2003","extra":{"n":9007199254740993}}`, `{"tool":"word-count","text":" \n\t\u00a0\u2003","extra":{"n":9007199254740993}}`},
	} {
		if !t.Run(tc.name, func(t *testing.T) {
			before := counts(t)
			var inputID, processID uuid.UUID
			var status string
			if err := tx.QueryRow(ctx, create, tc.action, 1, tc.data).Scan(&inputID, &processID, &status); err != nil ||
				inputID == uuid.Nil || processID == uuid.Nil || status != "new" {
				t.Fatalf("input=%s process=%s status=%q error=%v", inputID, processID, status, err)
			}
			var matches int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM sm_inputs i JOIN sm_processes p ON p.input_id=i.id
				JOIN sm_jobs j ON j.process_id=p.id WHERE i.id=$1 AND p.id=$2 AND i.source='api'
				AND i.payload=$3::jsonb AND j.payload=i.payload AND j.tool_name=$4 AND j.status='pending' AND j.action_version=1`,
				inputID, processID, tc.payload, tc.action).Scan(&matches); err != nil || matches != 1 {
				t.Fatalf("matching input/process/job chains=%d error=%v", matches, err)
			}
			if got := counts(t); got != [3]int{before[0] + 1, before[1] + 1, before[2] + 1} {
				t.Fatalf("expected exactly one new chain: %v → %v", before, got)
			}
		}) {
			t.FailNow()
		}
	}
	for _, tc := range []struct {
		name, action string
		version      int
		data         any
		setup        string
	}{
		{"disabled v1", "echo-test", 1, `{}`, `UPDATE sm_actions SET enabled=false WHERE action='echo-test' AND version=1`},
		{"unknown", "unknown", 1, `{}`, ""},
		{"case sensitive", "Echo-test", 1, `{}`, ""},
		{"padded name", " echo-test ", 1, `{}`, ""},
		{"unknown version", "echo-test", 2, `{}`, ""},
		{"enabled v2", "echo-test", 2, `{}`, `INSERT INTO sm_actions VALUES ('echo-test',2,'Echo v2',true)`},
		{"SQL null data", "echo-test", 1, nil, ""},
		{"JSON null data", "echo-test", 1, `null`, ""},
		{"array", "echo-test", 1, `[]`, ""},
		{"string", "echo-test", 1, `"message"`, ""},
		{"number", "echo-test", 1, `1`, ""},
		{"tool collision", "echo-test", 1, `{"tool":"echo-test"}`, ""},
		{"null tool collision", "echo-test", 1, `{"tool":null}`, ""},
		{"bad echo", "echo-test", 1, `{"message":null}`, ""},
		{"bad fabric", "fabric", 1, `{}`, ""},
		{"bad URL", "yt-dlp", 1, `{"url":"/relative"}`, ""},
		{"word-count missing text", "word-count", 1, `{}`, ""},
		{"word-count null text", "word-count", 1, `{"text":null}`, ""},
		{"word-count numeric text", "word-count", 1, `{"text":42}`, ""},
		{"word-count array text", "word-count", 1, `{"text":[]}`, ""},
		{"word-count object text", "word-count", 1, `{"text":{}}`, ""},
		{"word-count boolean text", "word-count", 1, `{"text":true}`, ""},
		{"word-count disabled v1", "word-count", 1, `{"text":"hello"}`, `UPDATE sm_actions SET enabled=false WHERE action='word-count' AND version=1`},
		{"word-count enabled v2", "word-count", 2, `{"text":"hello"}`, `INSERT INTO sm_actions VALUES ('word-count',2,'Word count v2',true)`},
		{"word-count tool collision", "word-count", 1, `{"text":"hello","tool":"echo-test"}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture, err := tx.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer fixture.Rollback(ctx)
			if tc.setup != "" {
				if tag, err := fixture.Exec(ctx, tc.setup); err != nil || tag.RowsAffected() != 1 {
					t.Fatalf("set up action fixture: %v, error=%v", tag, err)
				}
			}
			before := counts(t)
			attempt, err := fixture.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			_, err = attempt.Exec(ctx, create, tc.action, tc.version, tc.data)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "22023" {
				t.Errorf("error=%v, want SQLSTATE 22023", err)
			}
			if err == nil {
				// Check before rolling back so rollback cannot hide a write.
				if got := counts(t); got != before {
					t.Errorf("accepted invalid action changed counts: %v → %v", before, got)
				}
			}
			if err := attempt.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			if got := counts(t); got != before {
				t.Fatalf("rejected action changed counts: %v → %v", before, got)
			}
		})
	}
}

func testActionVersionColumn(t *testing.T, env *queueTestEnv) {
	t.Helper()
	ctx := queueTestContext(t)
	var dataType, nullable, defaultValue string
	err := env.pool.QueryRow(ctx, `SELECT data_type,is_nullable,column_default FROM information_schema.columns
		WHERE table_schema='public' AND table_name='sm_jobs' AND column_name='action_version'`).
		Scan(&dataType, &nullable, &defaultValue)
	if err != nil || dataType != "integer" || nullable != "NO" || defaultValue != "1" {
		t.Fatalf("action_version type=%q nullable=%q default=%q error=%v", dataType, nullable, defaultValue, err)
	}
	ids := env.createJob(t, ctx, []byte(`{"tool":"echo-test"}`))
	var version int
	if err := env.pool.QueryRow(ctx, `SELECT action_version FROM sm_jobs WHERE id=$1`, ids.jobID).
		Scan(&version); err != nil || version != 1 {
		t.Fatalf("default action_version=%d error=%v", version, err)
	}
	_, err = env.pool.Exec(ctx, `UPDATE sm_jobs SET action_version=0 WHERE id=$1`, ids.jobID)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != "sm_jobs_action_version_positive" {
		t.Fatalf("zero version error=%v, want positivity check violation", err)
	}
}

func testActionDispatchVersion(t *testing.T, env *queueTestEnv) {
	t.Helper()
	ctx := queueTestContext(t)
	tx, err := env.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())

	// Enqueue inside the outer statement: the dispatch function can see the
	// new job, but a join to sm_jobs using the outer snapshot cannot see it.
	var picked bool
	err = tx.QueryRow(ctx, `
		WITH created AS MATERIALIZED (
			SELECT * FROM public.sm_create_action('echo-test', 1, '{}'::jsonb)
		)
		SELECT (
			SELECT j.process_id=created.process_id AND j.action_version=1 AND j.status='dispatched'
			FROM public.sm_pick_next_job_with_version() AS j
		) FROM created`).Scan(&picked)
	if err != nil || !picked {
		t.Fatalf("pick newly visible job: correct job/version/status=%t error=%v", picked, err)
	}
}
