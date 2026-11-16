package db_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"petar/internal/db"
)

func TestQueueCallbackErrorContract(t *testing.T) {
	env := newQueueTestEnv(t, "step12-callback-errors-"+uuid.NewString())
	ctx := queueTestContext(t)
	ids := env.createJob(t, ctx, json.RawMessage(`{"tool":"echo-test"}`))
	counts := func() (int, int) {
		t.Helper()
		var logs, outputs int
		err := env.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM sm_job_logs WHERE job_id=$1), (SELECT count(*) FROM sm_outputs WHERE process_id=$2)`, ids.jobID, ids.processID).Scan(&logs, &outputs)
		if err != nil {
			t.Fatal(err)
		}
		return logs, outputs
	}
	requireNoRows := func(err error) {
		t.Helper()
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("error = %v, want no rows", err)
		}
	}
	requireNoRows(env.database.UpdateJobStatus(ctx, uuid.New(), db.StatusInProgress, "", ""))
	requireNoRows(env.database.UpdateJobStatus(ctx, ids.jobID, db.StatusCompleted, "/result", ""))
	requireJobStatus(t, ctx, env.pool, ids.jobID, db.StatusPending)
	if _, outputs := counts(); outputs != 0 {
		t.Fatalf("pending outputs = %d", outputs)
	}
	job, err := env.database.PickNextJob(ctx)
	if err != nil || job == nil || job.ID != ids.jobID {
		t.Fatalf("pick = %v, %v", job, err)
	}
	logsBefore, outputsBefore := counts()
	for _, status := range []db.Status{"unknown", ""} {
		err := env.database.UpdateJobStatus(ctx, ids.jobID, status, "", "")
		var pgErr *pgconn.PgError
		if !errors.Is(err, db.ErrInvalidCallbackStatus) || errors.Is(err, pgx.ErrNoRows) || !errors.As(err, &pgErr) || pgErr.Code != "22023" {
			t.Fatalf("status %q: error = %v", status, err)
		}
		requireJobStatus(t, ctx, env.pool, ids.jobID, db.StatusDispatched)
		logs, outputs := counts()
		if logs != logsBefore || outputs != outputsBefore {
			t.Fatalf("status %q changed counts: %d/%d -> %d/%d", status, logsBefore, outputsBefore, logs, outputs)
		}
	}
	if err := env.database.UpdateJobStatus(ctx, ids.jobID, db.StatusCompleted, "/result", ""); err != nil {
		t.Fatal(err)
	}
	requireNoRows(env.database.UpdateJobStatus(ctx, ids.jobID, db.StatusCompleted, "/result", ""))
	requireJobStatus(t, ctx, env.pool, ids.jobID, db.StatusCompleted)
	if _, outputs := counts(); outputs != 1 {
		t.Fatalf("completed outputs = %d", outputs)
	}
}
