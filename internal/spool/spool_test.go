package spool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"petar/internal/config"
	"petar/internal/db"
)

type fakeNotification struct {
	payload string
	err     error
}

type fakeQueue struct {
	mu   sync.Mutex
	jobs []*db.Job

	notifications   chan fakeNotification
	waitStarted     chan struct{}
	reconnectCalled chan struct{}
}

func newFakeQueue() *fakeQueue {
	return &fakeQueue{
		notifications:   make(chan fakeNotification),
		waitStarted:     make(chan struct{}, 10),
		reconnectCalled: make(chan struct{}, 10),
	}
}

func (q *fakeQueue) PickNextJob(ctx context.Context) (*db.Job, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if len(q.jobs) == 0 {
		return nil, nil
	}

	job := q.jobs[0]
	q.jobs = q.jobs[1:]
	return job, nil
}

func (q *fakeQueue) WaitForNotification(ctx context.Context) (string, error) {
	select {
	case q.waitStarted <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}

	select {
	case notification := <-q.notifications:
		return notification.payload, notification.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (q *fakeQueue) ReconnectListener(ctx context.Context) error {
	select {
	case q.reconnectCalled <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

func (q *fakeQueue) addJob(job *db.Job) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.jobs = append(q.jobs, job)
}

func testJob() *db.Job {
	return &db.Job{
		ID:        uuid.New(),
		ProcessID: uuid.New(),
		ToolName:  "echo-test",
		Payload:   json.RawMessage(`{"tool":"echo-test","message":"hello"}`),
		OldStatus: db.StatusPending,
		Status:    db.StatusDispatched,
	}
}

func newTestWriter(t *testing.T) (*SpoolWriter, string) {
	t.Helper()

	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "in"), 0o755); err != nil {
		t.Fatalf("create spool input directory: %v", err)
	}
	return NewWriter(root), root
}

func waitForFile(t *testing.T, ctx context.Context, path string) []byte {
	t.Helper()

	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()

	for {
		content, err := os.ReadFile(path)
		if err == nil {
			return content
		}
		if !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("read spool file: %v", err)
		}

		select {
		case <-ctx.Done():
			t.Fatalf("file %s was not created: %v", path, ctx.Err())
		case <-ticker.C:
		}
	}
}

func waitForSignal(t *testing.T, ctx context.Context, signal <-chan struct{}, name string) {
	t.Helper()

	select {
	case <-signal:
	case <-ctx.Done():
		t.Fatalf("wait for %s: %v", name, ctx.Err())
	}
}

func startTestWorker(
	t *testing.T,
	worker *SpoolWorker,
) (context.CancelFunc, <-chan error) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- worker.Run(ctx)
	}()

	t.Cleanup(func() {
		cancel()
		select {
		case err := <-result:
			if err != nil {
				t.Errorf("stop worker: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("worker did not stop after cancellation")
		}
	})

	return cancel, result
}

func TestWriteJobCreatesJSON(t *testing.T) {
	writer, root := newTestWriter(t)
	job := testJob()

	if err := writer.WriteJob(context.Background(), job); err != nil {
		t.Fatalf("write job: %v", err)
	}

	jsonPath := filepath.Join(root, "in", job.ID.String()+".json")
	tmpPath := filepath.Join(root, "in", job.ID.String()+".tmp")
	content, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatalf("read job JSON: %v", err)
	}
	if _, err := os.Stat(tmpPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("temporary file still exists: %v", err)
	}

	var document jobDocument
	if err := json.Unmarshal(content, &document); err != nil {
		t.Fatalf("decode job JSON: %v", err)
	}
	if document.ID != job.ID.String() {
		t.Fatalf("id = %q, want %q", document.ID, job.ID)
	}
	if document.ProcessID != job.ProcessID.String() {
		t.Fatalf("process_id = %q, want %q", document.ProcessID, job.ProcessID)
	}
	if document.ToolName != job.ToolName {
		t.Fatalf("tool_name = %q, want %q", document.ToolName, job.ToolName)
	}
	if document.OldStatus != job.OldStatus || document.Status != job.Status {
		t.Fatalf("statuses = %q/%q, want %q/%q",
			document.OldStatus,
			document.Status,
			job.OldStatus,
			job.Status,
		)
	}

	var payload struct {
		Tool    string `json:"tool"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(document.Payload, &payload); err != nil {
		t.Fatalf("payload is not a nested JSON object: %v", err)
	}
	if payload.Tool != "echo-test" || payload.Message != "hello" {
		t.Fatalf("unexpected payload: %+v", payload)
	}
}

func TestWriteJobIsAtomic(t *testing.T) {
	writer, root := newTestWriter(t)
	job := testJob()
	jsonPath := filepath.Join(root, "in", job.ID.String()+".json")
	tmpPath := filepath.Join(root, "in", job.ID.String()+".tmp")

	if err := writer.WriteJob(context.Background(), job); err != nil {
		t.Fatalf("write initial job: %v", err)
	}

	readerCtx, stopReader := context.WithCancel(context.Background())
	readerResult := make(chan error, 1)
	go func() {
		for {
			select {
			case <-readerCtx.Done():
				readerResult <- nil
				return
			default:
			}

			content, err := os.ReadFile(jsonPath)
			if err != nil {
				readerResult <- fmt.Errorf("read JSON: %w", err)
				return
			}
			var document jobDocument
			if err := json.Unmarshal(content, &document); err != nil {
				readerResult <- fmt.Errorf("partial JSON %q: %w", content, err)
				return
			}
			if document.ID != job.ID.String() || document.ToolName != job.ToolName {
				readerResult <- fmt.Errorf("inconsistent document: %+v", document)
				return
			}
			runtime.Gosched()
		}
	}()

	for index := range 20 {
		job.Payload = json.RawMessage(
			fmt.Sprintf(`{"tool":"echo-test","version":%d}`, index),
		)
		if err := writer.WriteJob(context.Background(), job); err != nil {
			stopReader()
			<-readerResult
			t.Fatalf("replace job JSON: %v", err)
		}
	}

	stopReader()
	if err := <-readerResult; err != nil {
		t.Fatalf("concurrent reader: %v", err)
	}
	if _, err := os.Stat(tmpPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("temporary file still exists: %v", err)
	}
}

func TestWriteJobCleansTempOnError(t *testing.T) {
	writer, root := newTestWriter(t)
	job := testJob()
	jsonPath := filepath.Join(root, "in", job.ID.String()+".json")
	tmpPath := filepath.Join(root, "in", job.ID.String()+".tmp")

	if err := os.Mkdir(jsonPath, 0o755); err != nil {
		t.Fatalf("create rename obstacle: %v", err)
	}

	err := writer.WriteJob(context.Background(), job)
	if err == nil {
		t.Fatal("WriteJob succeeded despite rename obstacle")
	}
	if !strings.Contains(err.Error(), job.ID.String()) {
		t.Fatalf("error %q does not contain job ID", err)
	}
	if _, err := os.Stat(tmpPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("temporary file was not removed: %v", err)
	}
	if info, err := os.Stat(jsonPath); err != nil || !info.IsDir() {
		t.Fatalf("rename obstacle was damaged: info=%v, err=%v", info, err)
	}
}

func TestWriteJobRejectsInvalidPayload(t *testing.T) {
	writer, root := newTestWriter(t)
	job := testJob()
	job.Payload = json.RawMessage(`{invalid`)

	if err := writer.WriteJob(context.Background(), job); err == nil {
		t.Fatal("WriteJob accepted invalid JSON payload")
	}

	for _, extension := range []string{".tmp", ".json"} {
		path := filepath.Join(root, "in", job.ID.String()+extension)
		if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("unexpected file %s: %v", path, err)
		}
	}
}

func TestWorkerDispatchesOnNotification(t *testing.T) {
	queue := newFakeQueue()
	writer, root := newTestWriter(t)
	worker := newWorker(queue, writer, time.Hour, testLogger())
	_, _ = startTestWorker(t, worker)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	waitForSignal(t, ctx, queue.waitStarted, "initial LISTEN wait")

	job := testJob()
	queue.addJob(job)
	queue.notifications <- fakeNotification{payload: job.ID.String()}

	path := filepath.Join(root, "in", job.ID.String()+".json")
	content := waitForFile(t, ctx, path)
	var document jobDocument
	if err := json.Unmarshal(content, &document); err != nil {
		t.Fatalf("decode dispatched job: %v", err)
	}
	if document.ID != job.ID.String() {
		t.Fatalf("dispatched ID = %q, want %q", document.ID, job.ID)
	}
}

func TestWorkerDispatchesOnTicker(t *testing.T) {
	queue := newFakeQueue()
	writer, root := newTestWriter(t)
	worker := newWorker(
		queue,
		writer,
		20*time.Millisecond,
		testLogger(),
	)
	_, _ = startTestWorker(t, worker)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	waitForSignal(t, ctx, queue.waitStarted, "initial LISTEN wait")

	job := testJob()
	queue.addJob(job)
	path := filepath.Join(root, "in", job.ID.String()+".json")
	waitForFile(t, ctx, path)
}

func TestWorkerReconnectsListener(t *testing.T) {
	queue := newFakeQueue()
	writer, root := newTestWriter(t)
	worker := newWorker(queue, writer, time.Hour, testLogger())
	_, _ = startTestWorker(t, worker)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	waitForSignal(t, ctx, queue.waitStarted, "initial LISTEN wait")

	job := testJob()
	queue.addJob(job)
	queue.notifications <- fakeNotification{err: errors.New("listener connection lost")}

	waitForSignal(t, ctx, queue.reconnectCalled, "listener reconnect")
	path := filepath.Join(root, "in", job.ID.String()+".json")
	waitForFile(t, ctx, path)
	waitForSignal(t, ctx, queue.waitStarted, "LISTEN wait after reconnect")
}

func TestWorkerStopsOnContextCancellation(t *testing.T) {
	queue := newFakeQueue()
	writer, _ := newTestWriter(t)
	worker := newWorker(queue, writer, time.Hour, testLogger())

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- worker.Run(ctx)
	}()

	waitCtx, waitCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer waitCancel()
	waitForSignal(t, waitCtx, queue.waitStarted, "LISTEN wait")
	cancel()

	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Run returned an error on cancellation: %v", err)
		}
	case <-waitCtx.Done():
		t.Fatalf("worker did not stop: %v", waitCtx.Err())
	}
}

func TestSpoolWorkerIntegration(t *testing.T) {
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	setupCtx, setupCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer setupCancel()
	database, err := db.New(setupCtx, cfg.DBDSN)
	if err != nil {
		t.Fatalf("create database: %v", err)
	}
	verification, err := pgxpool.New(setupCtx, cfg.DBDSN)
	if err != nil {
		database.Close()
		t.Fatalf("create verification pool: %v", err)
	}

	source := "step4-test-" + uuid.NewString()
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(
			context.Background(),
			5*time.Second,
		)
		defer cleanupCancel()
		if _, err := verification.Exec(
			cleanupCtx,
			"DELETE FROM sm_inputs WHERE source = $1",
			source,
		); err != nil {
			t.Errorf("clean integration input: %v", err)
		}
		database.Close()
		verification.Close()
	})

	writer, root := newTestWriter(t)
	worker := NewWorker(database, writer, time.Hour, testLogger())
	_, _ = startTestWorker(t, worker)

	created, err := database.CreateInput(
		setupCtx,
		source,
		json.RawMessage(`{"tool":"echo-test","message":"integration"}`),
	)
	if err != nil {
		t.Fatalf("create integration input: %v", err)
	}

	var jobID uuid.UUID
	if err := verification.QueryRow(
		setupCtx,
		`SELECT j.id
		 FROM sm_jobs j
		 JOIN sm_processes p ON p.id = j.process_id
		 WHERE p.input_id = $1`,
		created.InputID,
	).Scan(&jobID); err != nil {
		t.Fatalf("query integration job: %v", err)
	}

	path := filepath.Join(root, "in", jobID.String()+".json")
	content := waitForFile(t, setupCtx, path)
	var document jobDocument
	if err := json.Unmarshal(content, &document); err != nil {
		t.Fatalf("decode integration JSON: %v", err)
	}
	if document.ID != jobID.String() || document.ToolName != "echo-test" {
		t.Fatalf("unexpected integration document: %+v", document)
	}
	if document.Status != db.StatusDispatched {
		t.Fatalf("integration status = %q, want dispatched", document.Status)
	}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelError,
	}))
}
