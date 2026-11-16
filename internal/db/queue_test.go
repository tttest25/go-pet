package db_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"petar/internal/config"
	"petar/internal/db"
)

type queueTestEnv struct {
	database *db.DB
	pool     *pgxpool.Pool
	source   string
}

type testJobIDs struct {
	inputID   uuid.UUID
	processID uuid.UUID
	jobID     uuid.UUID
}

type statusTransition struct {
	oldStatus db.Status
	newStatus db.Status
}

func newQueueTestEnv(t *testing.T, source string) *queueTestEnv {
	t.Helper()

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	database, err := db.New(ctx, cfg.DBDSN)
	if err != nil {
		t.Fatalf("create database: %v", err)
	}

	pool, err := pgxpool.New(ctx, cfg.DBDSN)
	if err != nil {
		database.Close()
		t.Fatalf("create verification pool: %v", err)
	}

	env := &queueTestEnv{
		database: database,
		pool:     pool,
		source:   source,
	}

	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(
			context.Background(),
			5*time.Second,
		)
		defer cleanupCancel()

		if _, err := pool.Exec(
			cleanupCtx,
			"DELETE FROM sm_inputs WHERE source = $1",
			source,
		); err != nil {
			t.Errorf("clean up test input: %v", err)
		}

		database.Close()
		pool.Close()
	})

	return env
}

func queueTestContext(t *testing.T) context.Context {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func (e *queueTestEnv) createJob(
	t *testing.T,
	ctx context.Context,
	payload json.RawMessage,
) testJobIDs {
	t.Helper()

	created, err := e.database.CreateInput(ctx, e.source, payload)
	if err != nil {
		t.Fatalf("create input: %v", err)
	}
	if created.InputID == uuid.Nil || created.ProcessID == uuid.Nil {
		t.Fatalf("CreateInput returned a nil UUID: %+v", created)
	}

	ids := testJobIDs{
		inputID:   created.InputID,
		processID: created.ProcessID,
	}
	if err := e.pool.QueryRow(
		ctx,
		"SELECT id FROM sm_jobs WHERE process_id = $1",
		created.ProcessID,
	).Scan(&ids.jobID); err != nil {
		t.Fatalf("query generated job: %v", err)
	}

	return ids
}

func requireProcessState(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	processID uuid.UUID,
	want string,
) {
	t.Helper()

	var got string
	if err := pool.QueryRow(
		ctx,
		"SELECT state FROM sm_processes WHERE id = $1",
		processID,
	).Scan(&got); err != nil {
		t.Fatalf("query process state: %v", err)
	}
	if got != want {
		t.Fatalf("process state = %q, want %q", got, want)
	}
}

func requireJobStatus(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	jobID uuid.UUID,
	want db.Status,
) {
	t.Helper()

	var got db.Status
	if err := pool.QueryRow(
		ctx,
		"SELECT status FROM sm_jobs WHERE id = $1",
		jobID,
	).Scan(&got); err != nil {
		t.Fatalf("query job status: %v", err)
	}
	if got != want {
		t.Fatalf("job status = %q, want %q", got, want)
	}
}

func requireJSONEqual(t *testing.T, got, want json.RawMessage) {
	t.Helper()

	var gotValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatalf("decode actual JSON: %v", err)
	}

	var wantValue any
	if err := json.Unmarshal(want, &wantValue); err != nil {
		t.Fatalf("decode expected JSON: %v", err)
	}

	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("payload = %s, want %s", got, want)
	}
}

func TestCreateInputReturnsCreatedProcess(t *testing.T) {
	env := newQueueTestEnv(t, "step10-test-create-input")
	ctx := queueTestContext(t)

	created, err := env.database.CreateInput(
		ctx,
		env.source,
		json.RawMessage(`{"tool":"echo-test","message":"step10"}`),
	)
	if err != nil {
		t.Fatalf("create input: %v", err)
	}
	if created.InputID == uuid.Nil {
		t.Fatal("CreateInput returned a nil input UUID")
	}
	if created.ProcessID == uuid.Nil {
		t.Fatal("CreateInput returned a nil process UUID")
	}
	if created.Status != "new" {
		t.Fatalf("input status = %q, want new", created.Status)
	}

	var storedStatus string
	if err := env.pool.QueryRow(
		ctx,
		"SELECT status FROM sm_inputs WHERE id = $1",
		created.InputID,
	).Scan(&storedStatus); err != nil {
		t.Fatalf("query stored input: %v", err)
	}
	if storedStatus != created.Status {
		t.Fatalf("stored status = %q, want %q", storedStatus, created.Status)
	}

	var processID uuid.UUID
	if err := env.pool.QueryRow(
		ctx,
		"SELECT id FROM sm_processes WHERE input_id = $1",
		created.InputID,
	).Scan(&processID); err != nil {
		t.Fatalf("query created process: %v", err)
	}
	if processID != created.ProcessID {
		t.Fatalf("process ID = %s, want %s", processID, created.ProcessID)
	}
}

func TestCreateInputRollsBackWhenProcessIsMissing(t *testing.T) {
	env := newQueueTestEnv(t, "step10-test-create-input-without-process")
	ctx := queueTestContext(t)

	if _, err := env.pool.Exec(
		ctx,
		"ALTER TABLE sm_inputs DISABLE TRIGGER trg_sm_inputs_after_insert",
	); err != nil {
		t.Fatalf("disable input trigger: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		if _, err := env.pool.Exec(
			cleanupCtx,
			"ALTER TABLE sm_inputs ENABLE TRIGGER trg_sm_inputs_after_insert",
		); err != nil {
			t.Errorf("enable input trigger: %v", err)
		}
	})

	created, err := env.database.CreateInput(
		ctx,
		env.source,
		json.RawMessage(`{"tool":"echo-test"}`),
	)
	if err == nil {
		t.Fatal("CreateInput succeeded without a generated process")
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("CreateInput error = %v, want wrapped pgx.ErrNoRows", err)
	}
	if created != (db.CreatedInput{}) {
		t.Fatalf("CreateInput returned data after rollback: %+v", created)
	}

	var inputCount int
	if err := env.pool.QueryRow(
		ctx,
		"SELECT COUNT(*) FROM sm_inputs WHERE source = $1",
		env.source,
	).Scan(&inputCount); err != nil {
		t.Fatalf("count rolled back inputs: %v", err)
	}
	if inputCount != 0 {
		t.Fatalf("rolled back input count = %d, want 0", inputCount)
	}
}

func TestCreateInputAndPickNextJob(t *testing.T) {
	env := newQueueTestEnv(t, "step3-test-create-pick")
	ctx := queueTestContext(t)
	payload := json.RawMessage(`{"tool":"echo-test","message":"hello"}`)
	ids := env.createJob(t, ctx, payload)

	job, err := env.database.PickNextJob(ctx)
	if err != nil {
		t.Fatalf("pick next job: %v", err)
	}
	if job == nil {
		t.Fatal("PickNextJob returned no job")
	}

	if job.ID != ids.jobID {
		t.Fatalf("job ID = %s, want %s", job.ID, ids.jobID)
	}
	if job.ProcessID != ids.processID {
		t.Fatalf("process ID = %s, want %s", job.ProcessID, ids.processID)
	}
	if job.ToolName != "echo-test" {
		t.Fatalf("tool name = %q, want echo-test", job.ToolName)
	}
	requireJSONEqual(t, job.Payload, payload)
	if job.OldStatus != db.StatusPending {
		t.Fatalf("old status = %q, want %q", job.OldStatus, db.StatusPending)
	}
	if job.Status != db.StatusDispatched {
		t.Fatalf("status = %q, want %q", job.Status, db.StatusDispatched)
	}

	requireProcessState(t, ctx, env.pool, ids.processID, "running")

	var transition statusTransition
	if err := env.pool.QueryRow(
		ctx,
		`SELECT old_status, new_status
		 FROM sm_job_logs
		 WHERE job_id = $1`,
		ids.jobID,
	).Scan(&transition.oldStatus, &transition.newStatus); err != nil {
		t.Fatalf("query dispatch audit log: %v", err)
	}
	if transition.oldStatus != db.StatusPending ||
		transition.newStatus != db.StatusDispatched {
		t.Fatalf("audit transition = %+v, want pending -> dispatched", transition)
	}

	job, err = env.database.PickNextJob(ctx)
	if err != nil {
		t.Fatalf("pick from empty queue: %v", err)
	}
	if job != nil {
		t.Fatalf("empty queue returned job %s", job.ID)
	}
}

func TestPickNextJobPriorityAndFIFO(t *testing.T) {
	env := newQueueTestEnv(t, "step3-test-priority-fifo")
	ctx := queueTestContext(t)
	payload := json.RawMessage(`{"tool":"echo-test"}`)

	lowPriority := env.createJob(t, ctx, payload)
	newerHighPriority := env.createJob(t, ctx, payload)
	olderHighPriority := env.createJob(t, ctx, payload)
	baseTime := time.Now().Add(-time.Hour)

	jobs := []struct {
		id        uuid.UUID
		priority  int
		createdAt time.Time
	}{
		{lowPriority.jobID, 0, baseTime.Add(-time.Minute)},
		{newerHighPriority.jobID, 10, baseTime.Add(time.Minute)},
		{olderHighPriority.jobID, 10, baseTime},
	}
	for _, job := range jobs {
		if _, err := env.pool.Exec(
			ctx,
			`UPDATE sm_jobs
			 SET priority = $2, created_at = $3
			 WHERE id = $1`,
			job.id,
			job.priority,
			job.createdAt,
		); err != nil {
			t.Fatalf("prepare priority for job %s: %v", job.id, err)
		}
	}

	wantOrder := []uuid.UUID{
		olderHighPriority.jobID,
		newerHighPriority.jobID,
		lowPriority.jobID,
	}
	for index, wantID := range wantOrder {
		job, err := env.database.PickNextJob(ctx)
		if err != nil {
			t.Fatalf("pick job %d: %v", index, err)
		}
		if job == nil {
			t.Fatalf("pick job %d returned nil", index)
		}
		if job.ID != wantID {
			t.Fatalf("pick job %d = %s, want %s", index, job.ID, wantID)
		}
	}
}

func TestUpdateJobStatusCompleted(t *testing.T) {
	env := newQueueTestEnv(t, "step3-test-completed")
	ctx := queueTestContext(t)
	ids := env.createJob(
		t,
		ctx,
		json.RawMessage(`{"tool":"echo-test"}`),
	)

	job, err := env.database.PickNextJob(ctx)
	if err != nil || job == nil {
		t.Fatalf("pick job: job=%v, err=%v", job, err)
	}
	if err := env.database.UpdateJobStatus(
		ctx,
		ids.jobID,
		db.StatusInProgress,
		"",
		"",
	); err != nil {
		t.Fatalf("mark job in progress: %v", err)
	}
	requireProcessState(t, ctx, env.pool, ids.processID, "running")

	const resultPath = "/spool/out/step3-test/result.json"
	if err := env.database.UpdateJobStatus(
		ctx,
		ids.jobID,
		db.StatusCompleted,
		resultPath,
		"",
	); err != nil {
		t.Fatalf("complete job: %v", err)
	}

	var resultPathFromDB string
	var errorMsg *string
	requireJobStatus(t, ctx, env.pool, ids.jobID, db.StatusCompleted)
	if err := env.pool.QueryRow(
		ctx,
		`SELECT result_path, error_msg
		 FROM sm_jobs
		 WHERE id = $1`,
		ids.jobID,
	).Scan(&resultPathFromDB, &errorMsg); err != nil {
		t.Fatalf("query completed job fields: %v", err)
	}
	if resultPathFromDB != resultPath {
		t.Fatalf("result path = %q, want %q", resultPathFromDB, resultPath)
	}
	if errorMsg != nil {
		t.Fatalf("error message = %q, want NULL", *errorMsg)
	}
	requireProcessState(t, ctx, env.pool, ids.processID, "completed")

	rows, err := env.pool.Query(
		ctx,
		`SELECT old_status, new_status
		 FROM sm_job_logs
		 WHERE job_id = $1
		 ORDER BY id`,
		ids.jobID,
	)
	if err != nil {
		t.Fatalf("query audit transitions: %v", err)
	}
	defer rows.Close()

	var transitions []statusTransition
	for rows.Next() {
		var transition statusTransition
		if err := rows.Scan(
			&transition.oldStatus,
			&transition.newStatus,
		); err != nil {
			t.Fatalf("scan audit transition: %v", err)
		}
		transitions = append(transitions, transition)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate audit transitions: %v", err)
	}

	wantTransitions := []statusTransition{
		{db.StatusPending, db.StatusDispatched},
		{db.StatusDispatched, db.StatusInProgress},
		{db.StatusInProgress, db.StatusCompleted},
	}
	if !reflect.DeepEqual(transitions, wantTransitions) {
		t.Fatalf("transitions = %+v, want %+v", transitions, wantTransitions)
	}

	var artifactType string
	var outputResultPath string
	var contentIsNull bool
	if err := env.pool.QueryRow(
		ctx,
		`SELECT
			artifact_type,
			metadata->>'result_path',
			content IS NULL
		 FROM sm_outputs
		 WHERE process_id = $1`,
		ids.processID,
	).Scan(&artifactType, &outputResultPath, &contentIsNull); err != nil {
		t.Fatalf("query completed output: %v", err)
	}
	if artifactType != "result" {
		t.Fatalf("artifact type = %q, want result", artifactType)
	}
	if outputResultPath != resultPath {
		t.Fatalf("output result path = %q, want %q", outputResultPath, resultPath)
	}
	if !contentIsNull {
		t.Fatal("output content must be NULL")
	}
}

func TestUpdateJobStatusFailed(t *testing.T) {
	env := newQueueTestEnv(t, "step3-test-failed")
	ctx := queueTestContext(t)
	ids := env.createJob(
		t,
		ctx,
		json.RawMessage(`{"tool":"echo-test"}`),
	)

	job, err := env.database.PickNextJob(ctx)
	if err != nil || job == nil {
		t.Fatalf("pick job: job=%v, err=%v", job, err)
	}
	if err := env.database.UpdateJobStatus(
		ctx,
		ids.jobID,
		db.StatusFailed,
		"",
		"test error",
	); err != nil {
		t.Fatalf("fail job: %v", err)
	}

	requireJobStatus(t, ctx, env.pool, ids.jobID, db.StatusFailed)
	requireProcessState(t, ctx, env.pool, ids.processID, "failed")

	var errorMsg string
	if err := env.pool.QueryRow(
		ctx,
		"SELECT error_msg FROM sm_jobs WHERE id = $1",
		ids.jobID,
	).Scan(&errorMsg); err != nil {
		t.Fatalf("query failure message: %v", err)
	}
	if errorMsg != "test error" {
		t.Fatalf("error message = %q, want test error", errorMsg)
	}

	var transition statusTransition
	var logMessage *string
	if err := env.pool.QueryRow(
		ctx,
		`SELECT old_status, new_status, message
		 FROM sm_job_logs
		 WHERE job_id = $1
		 ORDER BY id DESC
		 LIMIT 1`,
		ids.jobID,
	).Scan(
		&transition.oldStatus,
		&transition.newStatus,
		&logMessage,
	); err != nil {
		t.Fatalf("query failure audit: %v", err)
	}
	if transition.oldStatus != db.StatusDispatched ||
		transition.newStatus != db.StatusFailed {
		t.Fatalf("failure transition = %+v, want dispatched -> failed", transition)
	}
	if logMessage == nil || *logMessage != "test error" {
		t.Fatalf("failure audit message = %v, want test error", logMessage)
	}

	var outputCount int
	if err := env.pool.QueryRow(
		ctx,
		"SELECT COUNT(*) FROM sm_outputs WHERE process_id = $1",
		ids.processID,
	).Scan(&outputCount); err != nil {
		t.Fatalf("count failure outputs: %v", err)
	}
	if outputCount != 0 {
		t.Fatalf("failed job created %d outputs", outputCount)
	}
}

func TestUpdateJobStatusRejectsInvalidTransitions(t *testing.T) {
	env := newQueueTestEnv(t, "step3-test-invalid-transitions")
	ctx := queueTestContext(t)
	payload := json.RawMessage(`{"tool":"echo-test"}`)

	t.Run("pending to completed", func(t *testing.T) {
		ids := env.createJob(t, ctx, payload)
		if err := env.database.UpdateJobStatus(
			ctx,
			ids.jobID,
			db.StatusCompleted,
			"/tmp/result.json",
			"",
		); err == nil {
			t.Fatal("completing a pending job succeeded")
		}
		requireJobStatus(t, ctx, env.pool, ids.jobID, db.StatusPending)
		if _, err := env.pool.Exec(
			ctx,
			"DELETE FROM sm_inputs WHERE id = $1",
			ids.inputID,
		); err != nil {
			t.Fatalf("clean pending job before next case: %v", err)
		}
	})

	t.Run("missing job", func(t *testing.T) {
		if err := env.database.UpdateJobStatus(
			ctx,
			uuid.New(),
			db.StatusInProgress,
			"",
			"",
		); err == nil {
			t.Fatal("updating a missing job succeeded")
		}
	})

	t.Run("unknown status", func(t *testing.T) {
		ids := env.createJob(t, ctx, payload)
		job, err := env.database.PickNextJob(ctx)
		if err != nil || job == nil {
			t.Fatalf("pick job: job=%v, err=%v", job, err)
		}
		if job.ID != ids.jobID {
			t.Fatalf("picked job = %s, want %s", job.ID, ids.jobID)
		}

		if err := env.database.UpdateJobStatus(
			ctx,
			ids.jobID,
			db.Status("unknown"),
			"",
			"",
		); err == nil {
			t.Fatal("unknown status was accepted")
		}
		requireJobStatus(t, ctx, env.pool, ids.jobID, db.StatusDispatched)
	})

	t.Run("repeated completed", func(t *testing.T) {
		ids := env.createJob(t, ctx, payload)
		job, err := env.database.PickNextJob(ctx)
		if err != nil || job == nil {
			t.Fatalf("pick job: job=%v, err=%v", job, err)
		}
		if job.ID != ids.jobID {
			t.Fatalf("picked job = %s, want %s", job.ID, ids.jobID)
		}

		if err := env.database.UpdateJobStatus(
			ctx,
			ids.jobID,
			db.StatusCompleted,
			"/tmp/result.json",
			"",
		); err != nil {
			t.Fatalf("complete job: %v", err)
		}
		if err := env.database.UpdateJobStatus(
			ctx,
			ids.jobID,
			db.StatusCompleted,
			"/tmp/result.json",
			"",
		); err == nil {
			t.Fatal("repeated completion succeeded")
		}

		var outputCount int
		if err := env.pool.QueryRow(
			ctx,
			"SELECT COUNT(*) FROM sm_outputs WHERE process_id = $1",
			ids.processID,
		).Scan(&outputCount); err != nil {
			t.Fatalf("count outputs: %v", err)
		}
		if outputCount != 1 {
			t.Fatalf("output count = %d, want 1", outputCount)
		}
	})
}

func TestWaitForNotification(t *testing.T) {
	env := newQueueTestEnv(t, "step3-test-notify")
	ctx := queueTestContext(t)
	ids := env.createJob(
		t,
		ctx,
		json.RawMessage(`{"tool":"echo-test"}`),
	)

	payload, err := env.database.WaitForNotification(ctx)
	if err != nil {
		t.Fatalf("wait for notification: %v", err)
	}
	if payload != ids.jobID.String() {
		t.Fatalf("notification payload = %q, want %q", payload, ids.jobID)
	}
}

func TestWaitForNotificationConnectionLost(t *testing.T) {
	env := newQueueTestEnv(t, "step3-test-notify-disconnect")
	ctx := queueTestContext(t)

	var listenerPID int
	if err := env.pool.QueryRow(
		ctx,
		`SELECT pid
		 FROM pg_stat_activity
		 WHERE datname = current_database()
		   AND pid <> pg_backend_pid()
		   AND query = 'LISTEN sm_jobs_channel'
		 ORDER BY backend_start DESC
		 LIMIT 1`,
	).Scan(&listenerPID); err != nil {
		t.Fatalf("find listener backend: %v", err)
	}

	if _, err := env.pool.Exec(
		ctx,
		"SELECT pg_terminate_backend($1)",
		listenerPID,
	); err != nil {
		t.Fatalf("terminate listener backend: %v", err)
	}

	_, err := env.database.WaitForNotification(ctx)
	if err == nil {
		t.Fatal("WaitForNotification succeeded after listener disconnect")
	}
	if !strings.Contains(err.Error(), "listener connection lost") {
		t.Fatalf("disconnect error = %q, want listener connection lost", err)
	}
	if ctx.Err() != nil {
		t.Fatalf("test context ended before disconnect was reported: %v", ctx.Err())
	}
}

func TestPickNextJobSkipsLockedRow(t *testing.T) {
	env := newQueueTestEnv(t, "step3-test-skip-locked")
	ctx := queueTestContext(t)
	payload := json.RawMessage(`{"tool":"echo-test"}`)
	locked := env.createJob(t, ctx, payload)
	available := env.createJob(t, ctx, payload)
	baseTime := time.Now().Add(-time.Hour)

	if _, err := env.pool.Exec(
		ctx,
		"UPDATE sm_jobs SET created_at = $2 WHERE id = $1",
		locked.jobID,
		baseTime,
	); err != nil {
		t.Fatalf("set locked job creation time: %v", err)
	}
	if _, err := env.pool.Exec(
		ctx,
		"UPDATE sm_jobs SET created_at = $2 WHERE id = $1",
		available.jobID,
		baseTime.Add(time.Minute),
	); err != nil {
		t.Fatalf("set available job creation time: %v", err)
	}

	tx, err := env.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin locking transaction: %v", err)
	}
	defer tx.Rollback(context.Background())

	var lockedID uuid.UUID
	if err := tx.QueryRow(
		ctx,
		"SELECT id FROM sm_jobs WHERE id = $1 FOR UPDATE",
		locked.jobID,
	).Scan(&lockedID); err != nil {
		t.Fatalf("lock first job: %v", err)
	}

	job, err := env.database.PickNextJob(ctx)
	if err != nil {
		t.Fatalf("pick with locked row: %v", err)
	}
	if job == nil {
		t.Fatal("PickNextJob returned nil while an unlocked job existed")
	}
	if job.ID != available.jobID {
		t.Fatalf("picked job = %s, want unlocked job %s", job.ID, available.jobID)
	}
	requireJobStatus(t, ctx, env.pool, locked.jobID, db.StatusPending)
}

func TestPickNextJobConcurrent(t *testing.T) {
	env := newQueueTestEnv(t, "step3-test-concurrent")
	ctx := queueTestContext(t)
	payload := json.RawMessage(`{"tool":"echo-test"}`)
	first := env.createJob(t, ctx, payload)
	second := env.createJob(t, ctx, payload)

	type pickResult struct {
		job *db.Job
		err error
	}

	start := make(chan struct{})
	results := make(chan pickResult, 2)
	for range 2 {
		go func() {
			<-start
			job, err := env.database.PickNextJob(ctx)
			results <- pickResult{job: job, err: err}
		}()
	}
	close(start)

	gotIDs := make(map[uuid.UUID]struct{}, 2)
	for range 2 {
		select {
		case result := <-results:
			if result.err != nil {
				t.Fatalf("concurrent pick: %v", result.err)
			}
			if result.job == nil {
				t.Fatal("concurrent pick returned nil")
			}
			gotIDs[result.job.ID] = struct{}{}
		case <-ctx.Done():
			t.Fatalf("concurrent pick timed out: %v", ctx.Err())
		}
	}

	if len(gotIDs) != 2 {
		t.Fatalf("concurrent picks returned duplicate IDs: %v", gotIDs)
	}
	for _, wantID := range []uuid.UUID{first.jobID, second.jobID} {
		if _, ok := gotIDs[wantID]; !ok {
			t.Fatalf("concurrent picks did not return job %s: %v", wantID, gotIDs)
		}
		requireJobStatus(t, ctx, env.pool, wantID, db.StatusDispatched)
	}
}

func TestDBStateFunctions(t *testing.T) {
	env := newQueueTestEnv(t, "step11-test-state-functions-"+uuid.NewString())
	ctx := queueTestContext(t)
	payload := json.RawMessage(`{"tool":"echo-test","message":"state functions"}`)

	tx, err := env.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin queue isolation: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tx.Rollback(cleanupCtx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Errorf("release queue isolation: %v", err)
		}
	})
	if _, err := tx.Exec(ctx, "LOCK TABLE sm_jobs IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatalf("lock queue: %v", err)
	}
	// Remove unrelated pending jobs only within this transaction; rollback restores them.
	if _, err := tx.Exec(ctx, "DELETE FROM sm_jobs WHERE status = 'pending'"); err != nil {
		t.Fatalf("isolate pending queue: %v", err)
	}

	requireSQLState := func(t *testing.T, err error, code string) {
		t.Helper()
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != code {
			t.Fatalf("error = %v, want SQLSTATE %s", err, code)
		}
	}

	create := func(t *testing.T) testJobIDs {
		t.Helper()
		var ids testJobIDs
		var status string
		if err := tx.QueryRow(ctx, "SELECT * FROM sm_create_input($1, $2)",
			env.source, payload).Scan(&ids.inputID, &ids.processID, &status); err != nil {
			t.Fatalf("call sm_create_input: %v", err)
		}
		if ids.inputID == uuid.Nil || ids.processID == uuid.Nil || status != "new" {
			t.Fatalf("created input = %+v, status = %q", ids, status)
		}
		if err := tx.QueryRow(ctx,
			"SELECT id FROM sm_jobs WHERE process_id = $1 AND process_id IN (SELECT id FROM sm_processes WHERE input_id = $2)",
			ids.processID, ids.inputID).Scan(&ids.jobID); err != nil {
			t.Fatalf("query created process and job: %v", err)
		}
		return ids
	}

	requireStatus := func(id uuid.UUID, want db.Status) {
		t.Helper()
		var got db.Status
		if err := tx.QueryRow(ctx, "SELECT status FROM sm_jobs WHERE id = $1", id).Scan(&got); err != nil {
			t.Fatalf("query job status: %v", err)
		}
		if got != want {
			t.Fatalf("job status = %q, want %q", got, want)
		}
	}
	requireState := func(id uuid.UUID, want string) {
		t.Helper()
		var got string
		if err := tx.QueryRow(ctx, "SELECT state FROM sm_processes WHERE id = $1", id).Scan(&got); err != nil {
			t.Fatalf("query process state: %v", err)
		}
		if got != want {
			t.Fatalf("process state = %q, want %q", got, want)
		}
	}

	low := create(t)
	newer := create(t)
	older := create(t)
	base := time.Now().Add(-time.Hour)
	for _, item := range []struct {
		id       uuid.UUID
		priority int
		created  time.Time
	}{
		{low.jobID, 0, base.Add(-time.Minute)},
		{newer.jobID, 10, base.Add(time.Minute)},
		{older.jobID, 10, base},
	} {
		if _, err := tx.Exec(ctx, "UPDATE sm_jobs SET priority = $2, created_at = $3 WHERE id = $1",
			item.id, item.priority, item.created); err != nil {
			t.Fatalf("prepare job ordering: %v", err)
		}
	}

	update := func(id uuid.UUID, status any, result, message string) error {
		if _, err := tx.Exec(ctx, "SAVEPOINT before_update"); err != nil {
			t.Fatalf("create update savepoint: %v", err)
		}
		_, err := tx.Exec(ctx, "SELECT sm_update_job_status($1, $2, $3, $4)", id, status, result, message)
		if err != nil {
			if _, rollbackErr := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT before_update"); rollbackErr != nil {
				t.Fatalf("roll back failed update: %v", rollbackErr)
			}
		}
		if _, releaseErr := tx.Exec(ctx, "RELEASE SAVEPOINT before_update"); releaseErr != nil {
			t.Fatalf("release update savepoint: %v", releaseErr)
		}
		return err
	}
	requireSQLState(t, update(low.jobID, "completed", "/result", ""), "P0002")
	requireSQLState(t, update(uuid.New(), "in_progress", "", ""), "P0002")
	requireSQLState(t, update(low.jobID, "unknown", "", ""), "22023")
	requireSQLState(t, update(low.jobID, nil, "", ""), "22023")
	requireStatus(low.jobID, db.StatusPending)

	for _, want := range []testJobIDs{older, newer, low} {
		var job db.Job
		err := tx.QueryRow(ctx, "SELECT * FROM sm_pick_next_job()").Scan(
			&job.ID, &job.ProcessID, &job.ToolName, &job.Payload, &job.OldStatus, &job.Status)
		if err != nil {
			t.Fatalf("call sm_pick_next_job: %v", err)
		}
		if job.ID != want.jobID || job.ProcessID != want.processID || job.ToolName != "echo-test" ||
			job.OldStatus != db.StatusPending || job.Status != db.StatusDispatched {
			t.Fatalf("picked job = %+v, want job %s pending -> dispatched", job, want.jobID)
		}
		requireJSONEqual(t, job.Payload, payload)
		requireState(want.processID, "running")
	}
	var unused uuid.UUID
	if err := tx.QueryRow(ctx, "SELECT id FROM sm_pick_next_job()").Scan(&unused); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("pick from empty queue: %v, want no rows", err)
	}

	if err := update(older.jobID, "in_progress", "", ""); err != nil {
		t.Fatalf("mark in progress: %v", err)
	}
	const resultPath = "/spool/out/state-functions/result.json"
	if err := update(older.jobID, "completed", resultPath, ""); err != nil {
		t.Fatalf("complete job: %v", err)
	}
	requireStatus(older.jobID, db.StatusCompleted)
	requireState(older.processID, "completed")
	requireSQLState(t, update(older.jobID, "completed", resultPath, ""), "P0002")
	var outputCount int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM sm_outputs
		WHERE process_id = $1 AND artifact_type = 'result' AND metadata->>'result_path' = $2`,
		older.processID, resultPath).Scan(&outputCount); err != nil || outputCount != 1 {
		t.Fatalf("completed output count = %d, err = %v; want 1", outputCount, err)
	}
	if err := update(newer.jobID, "failed", "", "test failure"); err != nil {
		t.Fatalf("fail job: %v", err)
	}
	requireStatus(newer.jobID, db.StatusFailed)
	requireState(newer.processID, "failed")
	var message string
	if err := tx.QueryRow(ctx, "SELECT error_msg FROM sm_jobs WHERE id = $1", newer.jobID).Scan(&message); err != nil || message != "test failure" {
		t.Fatalf("failure message = %q, err = %v", message, err)
	}

	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("roll back queue isolation: %v", err)
	}

	t.Run("missing process rolls back input", func(t *testing.T) {
		rollbackSource := env.source + "-rollback"
		// Register restoration before disabling the trigger, including on ambiguous errors.
		t.Cleanup(func() {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := env.pool.Exec(cleanupCtx, "ALTER TABLE sm_inputs ENABLE TRIGGER trg_sm_inputs_after_insert"); err != nil {
				t.Errorf("enable input trigger: %v", err)
			}
			if _, err := env.pool.Exec(cleanupCtx, "DELETE FROM sm_inputs WHERE source = $1", rollbackSource); err != nil {
				t.Errorf("clean up trigger test input: %v", err)
			}
		})
		if _, err := env.pool.Exec(ctx, "ALTER TABLE sm_inputs DISABLE TRIGGER trg_sm_inputs_after_insert"); err != nil {
			t.Fatalf("disable input trigger: %v", err)
		}
		var inputID, processID uuid.UUID
		var status string
		err := env.pool.QueryRow(ctx, "SELECT * FROM sm_create_input($1, $2)", rollbackSource, payload).
			Scan(&inputID, &processID, &status)
		requireSQLState(t, err, "P0002")
		var count int
		if err := env.pool.QueryRow(ctx, "SELECT count(*) FROM sm_inputs WHERE source = $1", rollbackSource).Scan(&count); err != nil || count != 0 {
			t.Fatalf("rolled back input count = %d, err = %v; want 0", count, err)
		}
	})
}
