package db_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestActionCatalogInitialEntries(t *testing.T) {
	ctx := queueTestContext(t)
	conn, err := pgx.Connect(ctx, requireTestDSN(ctx, t))
	if err != nil {
		t.Fatalf("connect to catalog test database: %v", err)
	}
	defer conn.Close(context.Background())
	type entry struct {
		action  string
		version int
	}
	want := []entry{{"echo-test", 1}, {"fabric", 1}, {"word-count", 1}, {"yt-dlp", 1}}
	rows, err := conn.Query(ctx, `
		SELECT action, version FROM public.sm_actions
		WHERE enabled ORDER BY action COLLATE "C", version`)
	if err != nil {
		t.Fatalf("query active catalog entries: %v", err)
	}
	defer rows.Close()
	var got []entry
	for rows.Next() {
		var item entry
		if err := rows.Scan(&item.action, &item.version); err != nil {
			t.Fatalf("scan catalog entry: %v", err)
		}
		got = append(got, item)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate catalog entries: %v", err)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("active catalog entries = %v, want %v", got, want)
	}
}

func TestActionCatalogDisabledRejectsInput(t *testing.T) {
	ctx := queueTestContext(t)
	conn, err := pgx.Connect(ctx, requireTestDSN(ctx, t))
	if err != nil {
		t.Fatalf("connect to catalog test database: %v", err)
	}
	defer conn.Close(context.Background())
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin catalog test: %v", err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tx.Rollback(cleanup); err != nil {
			t.Errorf("roll back catalog test: %v", err)
		}
	}()

	const createInput = `SELECT input_id, process_id, status FROM public.sm_create_input($1, '{"tool":"echo-test"}')`
	source := "t22-catalog-" + uuid.NewString()
	var inputID, processID, jobID uuid.UUID
	var status string
	if err := tx.QueryRow(ctx, createInput, source).Scan(&inputID, &processID, &status); err != nil || status != "new" {
		t.Fatalf("create control input: status %q, error %v", status, err)
	}
	var processes, jobs int
	if err := tx.QueryRow(ctx, `
		SELECT j.id,
		    (SELECT count(*) FROM public.sm_processes WHERE input_id=$1),
		    (SELECT count(*) FROM public.sm_jobs WHERE process_id=$2)
		FROM public.sm_processes p JOIN public.sm_jobs j ON j.process_id=p.id
		WHERE p.id=$2 AND p.input_id=$1 AND j.tool_name='echo-test' AND j.status='pending'`,
		inputID, processID).Scan(&jobID, &processes, &jobs); err != nil || processes != 1 || jobs != 1 {
		t.Fatalf("control chain: processes=%d jobs=%d, error %v", processes, jobs, err)
	}
	tag, err := tx.Exec(ctx, `UPDATE public.sm_jobs SET status='dispatched' WHERE id=$1 AND status='pending'`, jobID)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("dispatch control job: %v, error %v", tag, err)
	}
	const resultPath = "/spool/out/t22-catalog/result.json"
	if _, err := tx.Exec(ctx, `SELECT public.sm_update_job_status($1,'completed',$2,NULL)`, jobID, resultPath); err != nil {
		t.Fatalf("complete control job: %v", err)
	}
	var hasAudit, hasOutput bool
	if err := tx.QueryRow(ctx, `SELECT
		EXISTS (SELECT FROM public.sm_job_logs WHERE job_id=$1 AND old_status='dispatched' AND new_status='completed'),
		EXISTS (SELECT FROM public.sm_outputs WHERE process_id=$2 AND artifact_type='result' AND metadata->>'result_path'=$3)`,
		jobID, processID, resultPath).Scan(&hasAudit, &hasOutput); err != nil || !hasAudit || !hasOutput {
		t.Fatalf("control history: audit=%t output=%t, error %v", hasAudit, hasOutput, err)
	}

	snapshot := func() (history string, counts [5]int) {
		t.Helper()
		err := tx.QueryRow(ctx, `SELECT jsonb_build_array(
			(SELECT to_jsonb(i) FROM public.sm_inputs i WHERE i.id=$1),
			(SELECT to_jsonb(p) FROM public.sm_processes p WHERE p.id=$2),
			(SELECT to_jsonb(j) FROM public.sm_jobs j WHERE j.id=$3),
			(SELECT jsonb_agg(to_jsonb(l) ORDER BY l.id) FROM public.sm_job_logs l WHERE l.job_id=$3),
			(SELECT jsonb_agg(to_jsonb(o) ORDER BY o.id) FROM public.sm_outputs o WHERE o.process_id=$2))::text,
			(SELECT count(*) FROM public.sm_inputs), (SELECT count(*) FROM public.sm_processes),
			(SELECT count(*) FROM public.sm_jobs), (SELECT count(*) FROM public.sm_job_logs),
			(SELECT count(*) FROM public.sm_outputs)`, inputID, processID, jobID).
			Scan(&history, &counts[0], &counts[1], &counts[2], &counts[3], &counts[4])
		if err != nil {
			t.Fatalf("snapshot control chain and table counts: %v", err)
		}
		return history, counts
	}
	before, beforeCounts := snapshot()

	tag, err = tx.Exec(ctx, `UPDATE public.sm_actions SET enabled=false WHERE action='echo-test' AND version=1 AND enabled`)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("disable echo-test v1: %v, error %v", tag, err)
	}
	if _, err := tx.Exec(ctx, `SAVEPOINT attempt`); err != nil {
		t.Fatalf("create attempt savepoint: %v", err)
	}
	attempt := source + "-rejected"
	var rejectedInput, rejectedProcess uuid.UUID
	var rejectedStatus string
	err = tx.QueryRow(ctx, createInput, attempt).Scan(&rejectedInput, &rejectedProcess, &rejectedStatus)
	if err == nil {
		t.Fatalf("sm_create_input accepted a valid payload for disabled echo-test v1 (input %s)", rejectedInput)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "22023" {
		t.Fatalf("disabled action error = %v, want SQLSTATE 22023", err)
	}
	if _, err := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT attempt`); err != nil {
		t.Fatalf("roll back attempt: %v", err)
	}
	var enabled, attemptExists bool
	if err := tx.QueryRow(ctx, `SELECT enabled,
		EXISTS (SELECT FROM public.sm_inputs WHERE source=$1)
		FROM public.sm_actions WHERE action='echo-test' AND version=1`, attempt).
		Scan(&enabled, &attemptExists); err != nil || enabled || attemptExists {
		t.Fatalf("after rejected attempt: enabled=%t input exists=%t, error %v", enabled, attemptExists, err)
	}
	after, afterCounts := snapshot()
	if afterCounts != beforeCounts {
		t.Errorf("table counts changed: before=%v after=%v", beforeCounts, afterCounts)
	}
	if after != before {
		t.Errorf("control history changed:\nbefore=%s\nafter=%s", before, after)
	}
}
