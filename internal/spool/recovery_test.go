package spool

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"petar/internal/db"
)

func TestRecoveryDispatchWithoutFile(t *testing.T) {
	ctx, f := newRecoveryFixture(t)
	first := f.pick(ctx, t)
	f.checkState(ctx, t, db.StatusDispatched, "running", "pending->dispatched")
	f.checkEmpty(t, "in", "processing")
	f.checkPending(ctx, t)
	if job, err := f.database.PickNextJob(ctx); err != nil || job != nil {
		t.Fatalf("dispatched job should not be picked again: job=%+v err=%v", job, err)
	}
	t.Logf("baseline job=%s: dispatched/running, no active files, second pick empty", f.jobID)

	var requeued uuid.UUID
	var oldStatus, newStatus db.Status
	err := f.pool.QueryRow(ctx, `UPDATE sm_jobs SET status='pending', result_path=NULL, error_msg=NULL, updated_at=NOW()
		WHERE id=$1 AND status IN ('dispatched','in_progress')
		RETURNING id, OLD.status, NEW.status`, f.jobID).Scan(&requeued, &oldStatus, &newStatus)
	if err != nil || requeued != f.jobID || oldStatus != db.StatusDispatched || newStatus != db.StatusPending {
		t.Fatalf("guarded requeue job=%s: got %s %s->%s err=%v", f.jobID, requeued, oldStatus, newStatus, err)
	}
	f.checkState(ctx, t, db.StatusPending, "init", "pending->dispatched", "dispatched->pending")
	job := f.pick(ctx, t)
	if job.ID != first.ID {
		t.Fatalf("recovery picked %s, want original job %s", job.ID, first.ID)
	}
	f.publish(ctx, t, job)
	run, checkCallbacks := f.runner(t, false)
	if out, err := run(ctx); err != nil {
		t.Fatalf("recovered runner: %v\n%s", err, out)
	}
	checkCallbacks()
	f.checkState(ctx, t, db.StatusCompleted, "completed", "pending->dispatched", "dispatched->pending",
		"pending->dispatched", "dispatched->in_progress", "in_progress->completed")
	f.checkEcho(t)
	f.checkEmpty(t, "in", "processing")
	t.Logf("recovered job=%s: addressed requeue, same ID, completed/completed, one output, no active files", f.jobID)
}

func TestRecoveryCommittedCallbackResponseLost(t *testing.T) {
	ctx, f := newRecoveryFixture(t)
	f.publish(ctx, t, f.pick(ctx, t))
	run, checkCallbacks := f.runner(t, true)
	out, err := run(ctx)
	if _, ok := err.(*exec.ExitError); !ok ||
		!strings.Contains(string(out), "job "+f.jobID.String()+" callback completed unconfirmed (curl exit ") {
		t.Fatalf("runner did not report the lost completed response: %v\n%s", err, out)
	}
	checkCallbacks()
	f.checkEmpty(t, "in")
	claimed := f.readJobFile(t, "processing")
	audit := []string{"pending->dispatched", "dispatched->in_progress", "in_progress->completed"}
	before := f.checkState(ctx, t, db.StatusCompleted, "completed", audit...)
	result := f.checkEcho(t)
	t.Logf("baseline job=%s: completed commit, response lost, processing and echo retained\n%s", f.jobID, strings.TrimSpace(string(out)))

	// SQL already confirmed a terminal commit. Archive only the processing
	// file; never requeue or retry the callback, and keep result_path valid.
	if err := os.Mkdir(filepath.Join(f.root, "archive"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(f.jobFile("processing"), f.jobFile("archive")); err != nil {
		t.Fatal(err)
	}
	if archived := f.readJobFile(t, "archive"); !slices.Equal(archived, claimed) {
		t.Fatal("archived processing file changed")
	}
	if out, err := run(ctx); err != nil {
		t.Fatalf("runner after archive: %v\n%s", err, out)
	}
	checkCallbacks() // Exactly the same two callback attempts, not four.
	after := f.checkState(ctx, t, db.StatusCompleted, "completed", audit...)
	if !reflect.DeepEqual(before, after) || !slices.Equal(result, f.checkEcho(t)) {
		t.Fatal("archive and empty runner pass changed committed state or result")
	}
	f.checkEmpty(t, "in", "processing")
	t.Logf("recovered job=%s: processing archived, no retry, audit/output/result unchanged", f.jobID)
}

type recoveryFixture struct {
	database         *db.DB
	pool             *pgxpool.Pool
	writer           *SpoolWriter
	root             string
	processID, jobID uuid.UUID
}

func newRecoveryFixture(t *testing.T) (context.Context, *recoveryFixture) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	t.Cleanup(cancel)
	dsn := requireTestDSN(ctx, t)
	database, err := db.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(database.Close)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	marker := "t21-recovery-" + uuid.NewString()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if _, err := pool.Exec(ctx, `DELETE FROM sm_inputs WHERE source='api' AND payload->>'_test_fixture'=$1`, marker); err != nil {
			t.Errorf("cleanup recovery input: %v", err)
		}
	})
	data := json.RawMessage(fmt.Sprintf(`{"message":"T21 recovery","_test_fixture":%q}`, marker))
	created, err := database.CreateAction(ctx, "echo-test", 1, data)
	if err != nil {
		t.Fatal(err)
	}
	f := &recoveryFixture{database: database, pool: pool, processID: created.ProcessID}
	if err := pool.QueryRow(ctx, `SELECT id FROM sm_jobs WHERE process_id=$1`, f.processID).Scan(&f.jobID); err != nil {
		t.Fatalf("locate recovery job: %v", err)
	}
	f.writer, f.root = newTestWriter(t)
	if err := os.Mkdir(filepath.Join(f.root, "processing"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Logf("drill job=%s process=%s", f.jobID, f.processID)
	return ctx, f
}

func (f *recoveryFixture) pick(ctx context.Context, t *testing.T) *db.Job {
	t.Helper()
	f.checkPending(ctx, t, f.jobID.String())
	job, err := f.database.PickNextJob(ctx)
	if err != nil || job == nil || job.ID != f.jobID || job.ProcessID != f.processID ||
		job.OldStatus != db.StatusPending || job.Status != db.StatusDispatched {
		t.Fatalf("expected pending->dispatched for job %s: got %+v err=%v", f.jobID, job, err)
	}
	return job
}

func (f *recoveryFixture) checkPending(ctx context.Context, t *testing.T, want ...string) {
	t.Helper()
	var pending []string
	if err := f.pool.QueryRow(ctx, `SELECT ARRAY(SELECT id::text FROM sm_jobs WHERE status='pending')`).Scan(&pending); err != nil {
		t.Fatalf("check pending queue: %v", err)
	}
	if !slices.Equal(pending, want) {
		t.Fatalf("refuse to pick: pending queue = %v, want %v", pending, want)
	}
}

func (f *recoveryFixture) jobFile(dir string) string {
	return filepath.Join(f.root, dir, f.jobID.String()+".json")
}

func (f *recoveryFixture) resultPath() string {
	return "/spool/out/" + f.jobID.String() + "/result.json"
}

func (f *recoveryFixture) publish(ctx context.Context, t *testing.T, job *db.Job) {
	t.Helper()
	if err := f.writer.WriteJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	f.readJobFile(t, "in")
}

func (f *recoveryFixture) readJobFile(t *testing.T, stage string) []byte {
	t.Helper()
	content, err := os.ReadFile(f.jobFile(stage))
	if err != nil {
		t.Fatal(err)
	}
	var doc jobDocument
	if err := json.Unmarshal(content, &doc); err != nil || doc.ID != f.jobID.String() {
		t.Fatalf("%s file does not contain job %s: id=%s err=%v", stage, f.jobID, doc.ID, err)
	}
	return content
}

func (f *recoveryFixture) checkEmpty(t *testing.T, dirs ...string) {
	t.Helper()
	for _, dir := range dirs {
		entries, err := os.ReadDir(filepath.Join(f.root, dir))
		if err != nil || len(entries) != 0 {
			t.Fatalf("%s should be empty: entries=%v err=%v", dir, entries, err)
		}
	}
}

func (f *recoveryFixture) checkEcho(t *testing.T) []byte {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(f.root, "out", f.jobID.String(), "result.json"))
	if err != nil {
		t.Fatal(err)
	}
	var echo map[string]string
	if err := json.Unmarshal(content, &echo); err != nil || echo["status"] != "ok" || echo["echo"] != "T21 recovery" {
		t.Fatalf("unexpected echo: %s err=%v", content, err)
	}
	return content
}

func (f *recoveryFixture) checkState(ctx context.Context, t *testing.T, status db.Status, state string, transitions ...string) db.ProcessDetails {
	t.Helper()
	process, err := f.database.GetProcess(ctx, f.processID)
	if err != nil {
		t.Fatal(err)
	}
	if process.State != state || len(process.Jobs) != 1 || process.Jobs[0].ID != f.jobID || process.Jobs[0].Status != status {
		t.Fatalf("job %s should be %s and process %s: state=%s jobs=%+v", f.jobID, status, state, process.State, process.Jobs)
	}
	var audit []string
	if err := f.pool.QueryRow(ctx, `SELECT ARRAY(SELECT old_status || '->' || new_status
		FROM sm_job_logs WHERE job_id=$1 ORDER BY id)`, f.jobID).Scan(&audit); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(audit, transitions) {
		t.Fatalf("job %s audit=%v want %v", f.jobID, audit, transitions)
	}
	path := process.Jobs[0].ResultPath
	if status != db.StatusCompleted {
		if path != nil || len(process.Outputs) != 0 {
			t.Fatalf("job %s has premature result path or output", f.jobID)
		}
		return process
	}
	if path == nil || *path != f.resultPath() || len(process.Outputs) != 1 {
		t.Fatalf("job %s should have result path %s and one output: job=%+v outputs=%+v", f.jobID, f.resultPath(), process.Jobs[0], process.Outputs)
	}
	output := process.Outputs[0]
	var meta map[string]string
	if err := json.Unmarshal(output.Metadata, &meta); err != nil ||
		output.ArtifactType != "result" || meta["result_path"] != f.resultPath() {
		t.Fatalf("job %s unexpected result output: %+v err=%v", f.jobID, output, err)
	}
	return process
}

func (f *recoveryFixture) runner(t *testing.T, loseCompletedResponse bool) (func(context.Context) ([]byte, error), func()) {
	t.Helper()
	var mu sync.Mutex
	var calls []db.Status
	var responseDropped bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		var body struct {
			Status     db.Status `json:"status"`
			ResultPath string    `json:"result_path"`
		}
		err := json.NewDecoder(r.Body).Decode(&body)
		calls = append(calls, body.Status)
		if err != nil || r.Method != http.MethodPost || r.Proto != "HTTP/1.1" ||
			r.URL.Path != "/api/v1/jobs/"+f.jobID.String()+"/status" ||
			!((body.Status == db.StatusInProgress && body.ResultPath == "") ||
				(body.Status == db.StatusCompleted && body.ResultPath == f.resultPath())) {
			t.Errorf("unexpected callback: %s %s %s body=%+v err=%v", r.Method, r.Proto, r.URL.Path, body, err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if err := f.database.UpdateJobStatus(ctx, f.jobID, body.Status, body.ResultPath, ""); err != nil {
			t.Errorf("callback DB update: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if loseCompletedResponse && body.Status == db.StatusCompleted {
			// UpdateJobStatus uses an autocommitted statement. Do not send
			// headers or close the connection until that statement succeeds.
			conn, _, err := http.NewResponseController(w).Hijack()
			if err != nil {
				t.Errorf("hijack committed callback: %v", err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			if err := conn.Close(); err != nil {
				t.Errorf("close committed callback: %v", err)
				return
			}
			responseDropped = true
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	return func(ctx context.Context) ([]byte, error) {
			ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "bash", "../../scripts/spool-runner.sh")
			cmd.Env = append(os.Environ(), "SPOOL_DIR="+f.root, "API_BASE_URL="+server.URL+"/api/v1",
				"NO_PROXY=127.0.0.1", "no_proxy=127.0.0.1", "RUNNER_TOOL_TIMEOUT=2")
			return cmd.CombinedOutput()
		}, func() {
			t.Helper()
			mu.Lock()
			defer mu.Unlock()
			if !slices.Equal(calls, []db.Status{db.StatusInProgress, db.StatusCompleted}) || responseDropped != loseCompletedResponse {
				t.Fatalf("callback attempts=%v, dropped completed response=%t", calls, responseDropped)
			}
		}
}
