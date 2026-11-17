package db_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"petar/internal/db"
)

// readFixture is committed before read methods use their own pool connections.
// Both processes belong to env.source, so the existing cleanup owns every row.
type readFixture struct {
	process db.ProcessDetails
	other   db.ProcessDetails
}

func readFixtureTime() time.Time {
	return time.Date(2025, time.January, 2, 3, 4, 5, 123456000, time.UTC)
}

func newReadFixture(ctx context.Context, t *testing.T, env *queueTestEnv, count int, prepare ...func(pgx.Tx, *readFixture)) readFixture {
	t.Helper()
	tx, err := env.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin read fixture: %v", err)
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tx.Rollback(cleanupCtx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Errorf("roll back read fixture: %v", err)
		}
	}()

	build := func(count int) db.ProcessDetails {
		p := db.ProcessDetails{
			State: "running", Context: json.RawMessage(`{"nested":{"enabled":true},"items":[1,"two",null]}`),
			CreatedAt: readFixtureTime(), UpdatedAt: readFixtureTime().Add(time.Hour),
			Jobs: make([]db.JobDetails, 0, count), Outputs: make([]db.OutputDetails, 0),
		}
		if err := tx.QueryRow(ctx, `INSERT INTO sm_inputs (source, payload)
			VALUES ($1, '{"tool":"echo-test"}'::jsonb) RETURNING id`, env.source).Scan(&p.InputID); err != nil {
			t.Fatalf("insert read input: %v", err)
		}
		if err := tx.QueryRow(ctx, `SELECT id FROM sm_processes WHERE input_id=$1`, p.InputID).Scan(&p.ID); err != nil {
			t.Fatalf("query read process: %v", err)
		}
		var initial uuid.UUID
		if err := tx.QueryRow(ctx, `UPDATE sm_jobs SET status='dispatched'
			WHERE process_id=$1 RETURNING id`, p.ID).Scan(&initial); err != nil {
			t.Fatalf("dispatch read fixture before commit: %v", err)
		}
		for i := 0; i < count; i++ {
			job := db.JobDetails{
				ProcessID: p.ID, ToolName: "echo-test",
				Payload: json.RawMessage(`{"nested":{"message":"read"},"values":[true,2,null]}`),
				Status:  db.StatusDispatched, Priority: i - 3,
				CreatedAt: readFixtureTime().Add(time.Duration(i%3) * time.Minute),
				UpdatedAt: readFixtureTime().Add(time.Duration(i+1) * time.Hour),
			}
			switch i % 3 {
			case 1:
				job.Status = db.StatusCompleted
				path, message := "/spool/out/read/result.json", "diagnostic"
				job.ResultPath, job.ErrorMsg = &path, &message
			case 2:
				job.Status = db.StatusFailed
				path, message := "", ""
				job.ResultPath, job.ErrorMsg = &path, &message
			}
			if i == 0 {
				job.ID = initial
				// Do not include status here: the real status trigger has already run.
				_, err = tx.Exec(ctx, `UPDATE sm_jobs SET tool_name=$2, payload=$3,
					priority=$4, result_path=$5, error_msg=$6, created_at=$7, updated_at=$8
					WHERE id=$1`, job.ID, job.ToolName, job.Payload, job.Priority,
					job.ResultPath, job.ErrorMsg, job.CreatedAt, job.UpdatedAt)
			} else {
				err = tx.QueryRow(ctx, `INSERT INTO sm_jobs
					(process_id, tool_name, payload, status, priority, result_path, error_msg, created_at, updated_at)
					VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING id`,
					p.ID, job.ToolName, job.Payload, job.Status, job.Priority,
					job.ResultPath, job.ErrorMsg, job.CreatedAt, job.UpdatedAt).Scan(&job.ID)
			}
			if err != nil {
				t.Fatalf("prepare read job %d: %v", i, err)
			}
			p.Jobs = append(p.Jobs, job)
		}
		for i := 0; i < 3; i++ {
			output := db.OutputDetails{
				ProcessID: p.ID, ArtifactType: "text",
				Metadata:  json.RawMessage(`{"nested":{"source":"fixture"},"tags":["a",null,3]}`),
				CreatedAt: readFixtureTime(),
			}
			if i == 0 {
				output.CreatedAt = output.CreatedAt.Add(time.Minute)
			}
			if i == 1 {
				content := "read artifact"
				output.Content = &content
			} else if i == 2 {
				content := ""
				output.Content = &content
			}
			if err := tx.QueryRow(ctx, `INSERT INTO sm_outputs
				(process_id, artifact_type, content, metadata, created_at)
				VALUES ($1,$2,$3,$4,$5) RETURNING id`, p.ID, output.ArtifactType,
				output.Content, output.Metadata, output.CreatedAt).Scan(&output.ID); err != nil {
				t.Fatalf("insert read output: %v", err)
			}
			p.Outputs = append(p.Outputs, output)
		}
		if _, err := tx.Exec(ctx, `UPDATE sm_processes
			SET state=$2, context=$3, created_at=$4, updated_at=$5 WHERE id=$1`,
			p.ID, p.State, p.Context, p.CreatedAt, p.UpdatedAt); err != nil {
			t.Fatalf("set read process fields after status trigger: %v", err)
		}
		return p
	}
	fixture := readFixture{process: build(count), other: build(2)}
	for _, apply := range prepare {
		apply(tx, &fixture)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit read fixture: %v", err)
	}
	return fixture
}

func requireReadJob(t *testing.T, got, want db.JobDetails) {
	t.Helper()
	if got.ID != want.ID || got.ProcessID != want.ProcessID ||
		got.ToolName != want.ToolName || got.Status != want.Status || got.Priority != want.Priority ||
		!reflect.DeepEqual(got.ResultPath, want.ResultPath) || !reflect.DeepEqual(got.ErrorMsg, want.ErrorMsg) ||
		!got.CreatedAt.Equal(want.CreatedAt) || !got.UpdatedAt.Equal(want.UpdatedAt) {
		t.Fatalf("job fields = %+v, want %+v", got, want)
	}
	requireJSONEqual(t, got.Payload, want.Payload)
}

func readJobIDs(jobs []db.JobDetails) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(jobs))
	for _, job := range jobs {
		ids = append(ids, job.ID)
	}
	return ids
}

func sortedReadJobs(jobs []db.JobDetails, descending bool) []db.JobDetails {
	result := append([]db.JobDetails{}, jobs...)
	sort.Slice(result, func(i, j int) bool {
		if result[i].CreatedAt.Equal(result[j].CreatedAt) {
			if descending {
				return result[i].ID.String() > result[j].ID.String()
			}
			return result[i].ID.String() < result[j].ID.String()
		}
		if descending {
			return result[i].CreatedAt.After(result[j].CreatedAt)
		}
		return result[i].CreatedAt.Before(result[j].CreatedAt)
	})
	return result
}

func requireReadJobs(t *testing.T, got, want []db.JobDetails) {
	t.Helper()
	if got == nil {
		t.Fatal("jobs is nil, want nonnil slice")
	}
	if !reflect.DeepEqual(readJobIDs(got), readJobIDs(want)) {
		t.Fatalf("job IDs = %v, want %v", readJobIDs(got), readJobIDs(want))
	}
	for i := range want {
		requireReadJob(t, got[i], want[i])
	}
}

func readOracleIDs(ctx context.Context, t *testing.T, pool *pgxpool.Pool, query string, args ...any) []uuid.UUID {
	t.Helper()
	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		t.Fatalf("query independent read oracle: %v", err)
	}
	defer rows.Close()
	ids := make([]uuid.UUID, 0)
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan read oracle: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate read oracle: %v", err)
	}
	return ids
}

func TestGetProcessDetails(t *testing.T) {
	env := newQueueTestEnv(t, "step13-"+t.Name()+"-"+uuid.NewString())
	ctx := queueTestContext(t)
	f := newReadFixture(ctx, t, env, 7)
	want := f.process
	got, err := env.database.GetProcess(ctx, want.ID)
	if err != nil {
		t.Fatalf("get process: %v", err)
	}
	if got.ID != want.ID || got.InputID != want.InputID || got.State != want.State ||
		!got.CreatedAt.Equal(want.CreatedAt) || !got.UpdatedAt.Equal(want.UpdatedAt) {
		t.Fatalf("process fields = %+v, want %+v", got, want)
	}
	requireJSONEqual(t, got.Context, want.Context)
	requireReadJobs(t, got.Jobs, sortedReadJobs(want.Jobs, false))
	sort.Slice(want.Outputs, func(i, j int) bool {
		if want.Outputs[i].CreatedAt.Equal(want.Outputs[j].CreatedAt) {
			return want.Outputs[i].ID.String() < want.Outputs[j].ID.String()
		}
		return want.Outputs[i].CreatedAt.Before(want.Outputs[j].CreatedAt)
	})
	if len(got.Outputs) != len(want.Outputs) {
		t.Fatalf("outputs count = %d, want %d (no JOIN duplicates)", len(got.Outputs), len(want.Outputs))
	}
	for i, expected := range want.Outputs {
		actual := got.Outputs[i]
		if actual.ID != expected.ID || actual.ProcessID != expected.ProcessID ||
			actual.ArtifactType != expected.ArtifactType || !reflect.DeepEqual(actual.Content, expected.Content) ||
			!actual.CreatedAt.Equal(expected.CreatedAt) {
			t.Fatalf("output %d = %+v, want %+v", i, actual, expected)
		}
		requireJSONEqual(t, actual.Metadata, expected.Metadata)
	}
}

func TestGetProcessNotFound(t *testing.T) {
	env := newQueueTestEnv(t, "step13-"+t.Name()+"-"+uuid.NewString())
	got, err := env.database.GetProcess(queueTestContext(t), uuid.New())
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("missing process error = %v, want wrapped pgx.ErrNoRows", err)
	}
	if !reflect.DeepEqual(got, db.ProcessDetails{}) {
		t.Fatalf("missing process returned partial result: %+v", got)
	}
}

func TestGetProcessEmptyChildren(t *testing.T) {
	env := newQueueTestEnv(t, "step13-"+t.Name()+"-"+uuid.NewString())
	ctx := queueTestContext(t)
	// Prepare an input/process with no outputs and delete only its own jobs before commit.
	tx, err := env.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin empty fixture: %v", err)
	}
	defer tx.Rollback(ctx)
	var inputID, processID uuid.UUID
	if err := tx.QueryRow(ctx, `INSERT INTO sm_inputs(source,payload)
		VALUES($1,'{"tool":"echo-test"}'::jsonb) RETURNING id`, env.source).Scan(&inputID); err != nil {
		t.Fatalf("insert empty fixture input: %v", err)
	}
	if err := tx.QueryRow(ctx, "SELECT id FROM sm_processes WHERE input_id=$1", inputID).Scan(&processID); err != nil {
		t.Fatalf("query empty fixture process: %v", err)
	}
	if _, err := tx.Exec(ctx, "UPDATE sm_jobs SET status='dispatched' WHERE process_id=$1", processID); err != nil {
		t.Fatalf("dispatch empty fixture jobs: %v", err)
	}
	if _, err := tx.Exec(ctx, "DELETE FROM sm_jobs WHERE process_id=$1", processID); err != nil {
		t.Fatalf("delete own fixture jobs: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit empty fixture: %v", err)
	}
	got, err := env.database.GetProcess(ctx, processID)
	if err != nil {
		t.Fatalf("get empty process: %v", err)
	}
	if got.ID != processID || got.InputID != inputID ||
		got.Jobs == nil || len(got.Jobs) != 0 || got.Outputs == nil || len(got.Outputs) != 0 {
		t.Fatalf("empty process = %+v, want existing process with nonnil empty children", got)
	}
}

func TestListJobsFilters(t *testing.T) {
	env := newQueueTestEnv(t, "step13-"+t.Name()+"-"+uuid.NewString())
	ctx := queueTestContext(t)
	f := newReadFixture(ctx, t, env, 7)
	a := f.process.ID
	missing, nilID := uuid.New(), uuid.Nil
	dispatched, completed, unknown := db.StatusDispatched, db.StatusCompleted, db.Status("unknown")
	all := sortedReadJobs(f.process.Jobs, true)
	selectStatus := func(status db.Status) []db.JobDetails {
		jobs := make([]db.JobDetails, 0)
		for _, job := range all {
			if job.Status == status {
				jobs = append(jobs, job)
			}
		}
		return jobs
	}
	for _, tc := range []struct {
		name   string
		filter db.JobFilter
		want   []db.JobDetails
	}{
		{"process A", db.JobFilter{ProcessID: &a}, all},
		{"A dispatched", db.JobFilter{ProcessID: &a, Status: &dispatched}, selectStatus(dispatched)},
		{"A completed", db.JobFilter{ProcessID: &a, Status: &completed}, selectStatus(completed)},
		{"missing process", db.JobFilter{ProcessID: &missing}, nil},
		{"unknown status", db.JobFilter{ProcessID: &a, Status: &unknown}, nil},
		{"nil UUID is a filter", db.JobFilter{ProcessID: &nilID}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := env.database.ListJobs(ctx, tc.filter)
			if err != nil {
				t.Fatalf("list filtered jobs: %v", err)
			}
			requireReadJobs(t, got, tc.want)
		})
	}
	// Global rows may change between queries in other packages. Check properties
	// of a single result; exact fixture membership is checked by process above.
	for _, tc := range []struct {
		name   string
		filter db.JobFilter
		status *db.Status
	}{
		{"status only", db.JobFilter{Status: &completed}, &completed},
		{"empty filter", db.JobFilter{}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := env.database.ListJobs(ctx, tc.filter)
			if err != nil {
				t.Fatalf("list global jobs: %v", err)
			}
			if got == nil || len(got) > 20 {
				t.Fatalf("global jobs count = %d, nil=%v, want nonnil and at most 20", len(got), got == nil)
			}
			if !reflect.DeepEqual(readJobIDs(got), readJobIDs(sortedReadJobs(got, true))) {
				t.Fatal("global jobs not ordered by created_at DESC,id DESC")
			}
			for _, job := range got {
				if tc.status != nil && job.Status != *tc.status {
					t.Fatalf("status-only query returned %q", job.Status)
				}
				for _, own := range append(append([]db.JobDetails{}, f.process.Jobs...), f.other.Jobs...) {
					if own.ID == job.ID {
						requireReadJob(t, job, own)
					}
				}
			}
		})
	}
}

func TestQualityGateSerializesPackages(t *testing.T) {
	script, err := os.ReadFile("../../scripts/quality-gate.sh")
	if err != nil {
		t.Fatalf("read quality gate: %v", err)
	}
	bridge, err := os.ReadFile("test_lifecycle_test.go")
	if err != nil {
		t.Fatalf("read quality gate bridge: %v", err)
	}
	// Gate now owns the lock in its test-only bridge, which launches the suite.
	// Keep the serialization, no-cache, race and finite-timeout contract explicit.
	if !strings.Contains(string(script), "PETAR_TEST_DB_COMMAND=gate") ||
		!strings.Contains(string(bridge), `{"test", "-p=1", "-count=1", "-v", "-race", "-timeout", "30s", "./..."}`) {
		t.Fatal("quality gate bridge must run uncached race tests with package parallelism -p=1")
	}
}

func TestListJobsLimits(t *testing.T) {
	env := newQueueTestEnv(t, "step13-"+t.Name()+"-"+uuid.NewString())
	ctx := queueTestContext(t)
	f := newReadFixture(ctx, t, env, 1, func(tx pgx.Tx, f *readFixture) {
		// Add 104 tasks in one round trip, before committing the initial task.
		if _, err := tx.Exec(ctx, `INSERT INTO sm_jobs
			(process_id,tool_name,payload,status,priority,created_at,updated_at)
			SELECT $1,'echo-test',jsonb_build_object('index',n),'dispatched',n,
				$2::timestamptz + n * interval '1 microsecond',$2::timestamptz
			FROM generate_series(1,104) AS n`, f.process.ID, readFixtureTime()); err != nil {
			t.Fatalf("batch insert limit fixture: %v", err)
		}
	})
	want := readOracleIDs(ctx, t, env.pool, `SELECT id FROM sm_jobs
		WHERE process_id=$1 ORDER BY created_at DESC,id DESC`, f.process.ID)
	if len(want) != 105 {
		t.Fatalf("limit fixture count = %d, want 105", len(want))
	}
	for _, tc := range []struct{ limit, count int }{{-1, 20}, {0, 20}, {1, 1}, {20, 20}, {100, 100}, {101, 100}} {
		t.Run(strconv.Itoa(tc.limit), func(t *testing.T) {
			got, err := env.database.ListJobs(ctx, db.JobFilter{ProcessID: &f.process.ID, Limit: tc.limit})
			if err != nil {
				t.Fatalf("list limit %d: %v", tc.limit, err)
			}
			if got == nil || !reflect.DeepEqual(readJobIDs(got), want[:tc.count]) {
				t.Fatalf("limit %d IDs = %v, want prefix %v", tc.limit, readJobIDs(got), want[:tc.count])
			}
		})
	}
}

func TestListJobsKeysetPagination(t *testing.T) {
	env := newQueueTestEnv(t, "step13-"+t.Name()+"-"+uuid.NewString())
	ctx := queueTestContext(t)
	f := newReadFixture(ctx, t, env, 7, func(tx pgx.Tx, f *readFixture) {
		ids := readJobIDs(f.process.Jobs)
		sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
		// Generate UUIDs first, then deliberately invert time vs UUID order.
		// Three tied timestamps cross a size-2 page boundary.
		for i, offset := range []int{6, 3, 3, 3, 5, 1, 0} {
			if _, err := tx.Exec(ctx, "UPDATE sm_jobs SET created_at=$2 WHERE id=$1",
				ids[i], readFixtureTime().Add(time.Duration(offset)*time.Minute)); err != nil {
				t.Fatalf("set adversarial keyset time: %v", err)
			}
		}
	})
	completed := db.StatusCompleted
	for _, tc := range []struct {
		name   string
		status *db.Status
	}{{"all statuses", nil}, {"completed only", &completed}} {
		t.Run(tc.name, func(t *testing.T) {
			want := readOracleIDs(ctx, t, env.pool, `SELECT id FROM sm_jobs
				WHERE process_id=$1 AND ($2::text IS NULL OR status=$2::text)
				ORDER BY created_at DESC,id DESC`, f.process.ID, tc.status)
			filter := db.JobFilter{ProcessID: &f.process.ID, Status: tc.status, Limit: 2}
			gotIDs := make([]uuid.UUID, 0)
			seen := make(map[uuid.UUID]bool)
			ended := false
			for page := 0; page <= len(want)+1; page++ {
				got, err := env.database.ListJobs(ctx, filter)
				if err != nil {
					t.Fatalf("read page %d: %v", page, err)
				}
				if got == nil || len(got) > 2 {
					t.Fatalf("page %d is nil or exceeds limit: %+v", page, got)
				}
				if len(got) == 0 {
					ended = true
					break
				}
				for _, job := range got {
					if job.ProcessID != f.process.ID || (tc.status != nil && job.Status != *tc.status) {
						t.Fatalf("page leaked past filter: %+v", job)
					}
					if seen[job.ID] {
						t.Fatalf("duplicate paginated ID %s", job.ID)
					}
					seen[job.ID] = true
					gotIDs = append(gotIDs, job.ID)
				}
				lastID := got[len(got)-1].ID
				filter.CursorID = &lastID
			}
			if !ended || !reflect.DeepEqual(gotIDs, want) {
				t.Fatalf("pagination ended=%v, IDs=%v, want %v", ended, gotIDs, want)
			}
			last := want[len(want)-1]
			filter.CursorID = &last
			got, err := env.database.ListJobs(ctx, filter)
			if err != nil || got == nil || len(got) != 0 {
				t.Fatalf("last cursor page = %+v, err=%v, want nonnil empty", got, err)
			}
		})
	}
}

func TestListJobsCursorBoundaries(t *testing.T) {
	env := newQueueTestEnv(t, "step13-"+t.Name()+"-"+uuid.NewString())
	ctx := queueTestContext(t)
	f := newReadFixture(ctx, t, env, 7)
	missing := uuid.New()
	completed := db.StatusCompleted
	// This dispatched cursor is older than some completed rows and newer than others.
	cursor := f.process.Jobs[0].ID
	if _, err := env.pool.Exec(ctx, "UPDATE sm_jobs SET created_at=$2 WHERE id=$1",
		cursor, readFixtureTime().Add(90*time.Second)); err != nil {
		t.Fatalf("prepare status-mismatching cursor: %v", err)
	}
	for _, tc := range []struct {
		name   string
		cursor uuid.UUID
		status *db.Status
	}{
		{"missing", missing, nil},
		{"other process", f.other.Jobs[1].ID, nil},
		{"cursor status differs", cursor, &completed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := readOracleIDs(ctx, t, env.pool, `SELECT j.id FROM sm_jobs j
				WHERE j.process_id=$1 AND ($2::text IS NULL OR j.status=$2::text)
				AND (j.created_at,j.id) < (SELECT created_at,id FROM sm_jobs WHERE id=$3)
				ORDER BY j.created_at DESC,j.id DESC`, f.process.ID, tc.status, tc.cursor)
			got, err := env.database.ListJobs(ctx, db.JobFilter{
				ProcessID: &f.process.ID, Status: tc.status, CursorID: &tc.cursor, Limit: 100,
			})
			if err != nil {
				t.Fatalf("list cursor boundary: %v", err)
			}
			if got == nil || !reflect.DeepEqual(readJobIDs(got), want) {
				t.Fatalf("boundary IDs = %v (nil=%v), want %v", readJobIDs(got), got == nil, want)
			}
			if tc.name != "missing" && len(want) == 0 {
				t.Fatal("boundary fixture must include rows below its cursor")
			}
			for _, job := range got {
				if job.ID == tc.cursor || job.ProcessID != f.process.ID {
					t.Fatalf("cursor included or process filter leaked: %+v", job)
				}
			}
		})
	}
}

func TestReadQueriesCanceledContext(t *testing.T) {
	env := newQueueTestEnv(t, "step13-"+t.Name()+"-"+uuid.NewString())
	liveCtx := queueTestContext(t)
	f := newReadFixture(liveCtx, t, env, 7)
	ctx, cancel := context.WithCancel(liveCtx)
	cancel()
	process, err := env.database.GetProcess(ctx, f.process.ID)
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(process, db.ProcessDetails{}) {
		t.Fatalf("canceled GetProcess = %+v, err=%v, want zero result and wrapped context.Canceled", process, err)
	}
	jobs, err := env.database.ListJobs(ctx, db.JobFilter{ProcessID: &f.process.ID})
	if !errors.Is(err, context.Canceled) || jobs != nil {
		t.Fatalf("canceled ListJobs = %+v, err=%v, want nil and wrapped context.Canceled", jobs, err)
	}
	if err := env.pool.Ping(liveCtx); err != nil {
		t.Fatalf("database unavailable after cancellation: %v", err)
	}
	if _, err := env.database.GetProcess(liveCtx, f.process.ID); err != nil {
		t.Fatalf("GetProcess unavailable after cancellation: %v", err)
	}
	if _, err := env.database.ListJobs(liveCtx, db.JobFilter{ProcessID: &f.process.ID}); err != nil {
		t.Fatalf("ListJobs unavailable after cancellation: %v", err)
	}
}

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

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dsn := requireTestDSN(ctx, t)

	database, err := db.New(ctx, dsn)
	if err != nil {
		t.Fatalf("create database: %v", err)
	}

	pool, err := pgxpool.New(ctx, dsn)
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
