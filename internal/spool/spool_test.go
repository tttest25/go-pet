package spool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

	"petar/internal/db"
)

type fakeNotification struct {
	payload string
	err     error
}

type fakeReconnectAttempt struct {
	started time.Time
	reply   chan error
	failed  chan time.Time
}

type fakeQueue struct {
	mu   sync.Mutex
	jobs []*db.Job

	notifications chan fakeNotification
	waitStarted   chan struct{}
	attempts      chan fakeReconnectAttempt // nil: reconnect succeeds immediately

	active, maxActive, successes int
	reconnects                   int
}

func newFakeQueue() *fakeQueue {
	return &fakeQueue{
		notifications: make(chan fakeNotification),
		waitStarted:   make(chan struct{}, 10),
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
	defer q.trackListenerCall()()
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
	defer q.trackListenerCall()()
	q.mu.Lock()
	q.reconnects++
	q.mu.Unlock()
	if q.attempts != nil {
		attempt := fakeReconnectAttempt{
			started: time.Now(),
			reply:   make(chan error),
			failed:  make(chan time.Time, 1),
		}
		select {
		case q.attempts <- attempt:
		case <-ctx.Done():
			return ctx.Err()
		}
		select {
		case err := <-attempt.reply:
			if err != nil {
				attempt.failed <- time.Now()
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	q.mu.Lock()
	q.successes++
	q.mu.Unlock()
	return nil
}

func (q *fakeQueue) trackListenerCall() func() {
	q.mu.Lock()
	q.active++
	q.maxActive = max(q.maxActive, q.active)
	q.mu.Unlock()
	return func() {
		q.mu.Lock()
		q.active--
		q.mu.Unlock()
	}
}

func (q *fakeQueue) listenerStats() (active, maxActive, successes int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.active, q.maxActive, q.successes
}

func (q *fakeQueue) addJob(job *db.Job) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.jobs = append(q.jobs, job)
}

func testJob() *db.Job {
	return &db.Job{
		ID:            uuid.New(),
		ProcessID:     uuid.New(),
		ToolName:      "echo-test",
		ActionVersion: 1,
		Payload:       json.RawMessage(`{"tool":"echo-test","message":"hello"}`),
		OldStatus:     db.StatusPending,
		Status:        db.StatusDispatched,
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

func receiveWorkerEvent[T any](t *testing.T, ctx context.Context, signal <-chan T, name string) T {
	t.Helper()

	select {
	case event := <-signal:
		return event
	case <-ctx.Done():
		t.Fatalf("wait for %s: %v", name, ctx.Err())
		var zero T
		return zero
	}
}

func sendWorkerEvent[T any](t *testing.T, ctx context.Context, signal chan<- T, event T) {
	t.Helper()
	select {
	case signal <- event:
	case <-ctx.Done():
		t.Fatalf("send worker event: %v", ctx.Err())
	}
}

func startTestWorker(
	t *testing.T,
	worker *SpoolWorker,
) (context.CancelFunc, func(context.Context) error) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var result error
	go func() {
		result = worker.Run(ctx)
		close(done)
	}()

	checked := false
	await := func(ctx context.Context) error {
		t.Helper()
		receiveWorkerEvent(t, ctx, done, "worker exit")
		checked = true
		return result
	}
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
			if !checked && result != nil {
				t.Errorf("stop worker: %v", result)
			}
		case <-time.After(5 * time.Second):
			t.Error("worker did not stop after cancellation")
		}
	})

	return cancel, await
}

func TestWriteJobCreatesJSON(t *testing.T) {
	writer, root := newTestWriter(t)
	job := testJob()
	job.ActionVersion = 2

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
	requireSpoolActionVersion(t, content, 2)
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
	receiveWorkerEvent(t, ctx, queue.waitStarted, "initial LISTEN wait")

	job := testJob()
	queue.addJob(job)
	sendWorkerEvent(t, ctx, queue.notifications, fakeNotification{payload: job.ID.String()})

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
	receiveWorkerEvent(t, ctx, queue.waitStarted, "initial LISTEN wait")

	job := testJob()
	queue.addJob(job)
	path := filepath.Join(root, "in", job.ID.String()+".json")
	waitForFile(t, ctx, path)
}

func controlledListener() *fakeQueue {
	queue := newFakeQueue()
	queue.attempts = make(chan fakeReconnectAttempt)
	return queue
}

func workerTestDeadline(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	t.Cleanup(cancel)
	return ctx
}

const listenerLostMessage = "spool listener connection lost; reconnecting"
const reconnectFailedMessage = "reconnect spool listener failed"

type workerLogEntry struct {
	Level   string `json:"level"`
	Message string `json:"msg"`
	Channel string `json:"channel"`
	Error   string `json:"error"`
}

type workerLog struct {
	mu      sync.Mutex
	data    bytes.Buffer
	written chan struct{}
}

func newWorkerLog() (*workerLog, *slog.Logger) {
	log := &workerLog{written: make(chan struct{}, 1)}
	return log, slog.New(slog.NewJSONHandler(log, &slog.HandlerOptions{Level: slog.LevelError}))
}

func (l *workerLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	n, err := l.data.Write(p)
	select {
	case l.written <- struct{}{}:
	default:
	}
	return n, err
}

func (l *workerLog) entries(t *testing.T) []workerLogEntry {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	var entries []workerLogEntry
	decoder := json.NewDecoder(bytes.NewReader(l.data.Bytes()))
	for {
		var entry workerLogEntry
		if err := decoder.Decode(&entry); err != nil {
			if errors.Is(err, io.EOF) {
				return entries
			}
			t.Fatalf("decode worker log: %v", err)
		}
		entries = append(entries, entry)
	}
}

func (l *workerLog) checkErrors(t *testing.T, losses []string, retries int) {
	t.Helper()
	entries := l.entries(t)
	if len(entries) != len(losses)+retries {
		t.Fatalf("unexpected ERROR records: %+v (want %d losses and %d retry errors)", entries, len(losses), retries)
	}
	for _, entry := range entries {
		if entry.Level != "ERROR" {
			t.Fatalf("unexpected log level: %+v", entry)
		}
		switch entry.Message {
		case listenerLostMessage:
			if len(losses) == 0 || entry.Channel != "sm_jobs_channel" || entry.Error != losses[0] {
				t.Fatalf("unexpected LISTEN loss record: %+v (remaining causes %v)", entry, losses)
			}
			losses = losses[1:]
		case reconnectFailedMessage:
			retries--
		default:
			t.Fatalf("unexpected ERROR record: %+v", entry)
		}
	}
	if len(losses) != 0 || retries != 0 {
		t.Fatalf("wrong ERROR records: %+v", entries)
	}
}

func loseListener(t *testing.T, ctx context.Context, queue *fakeQueue, cause string) fakeReconnectAttempt {
	t.Helper()
	sendWorkerEvent(t, ctx, queue.notifications, fakeNotification{err: errors.New(cause)})
	return receiveWorkerEvent(t, ctx, queue.attempts, "first reconnect attempt")
}

func checkPublishedJob(t *testing.T, ctx context.Context, root string, job *db.Job) {
	t.Helper()
	content := waitForFile(t, ctx, filepath.Join(root, "in", job.ID.String()+".json"))
	var document jobDocument
	if err := json.Unmarshal(content, &document); err != nil {
		t.Fatalf("decode dispatched job: %v", err)
	}
	if document.ID != job.ID.String() || document.Status != db.StatusDispatched {
		t.Fatalf("published ID/status = %q/%q, want %q/dispatched", document.ID, document.Status, job.ID)
	}
}

func checkStoppedListener(t *testing.T, queue *fakeQueue, successes int) {
	t.Helper()
	if active, peak, recovered := queue.listenerStats(); active != 0 || peak != 1 || recovered != successes {
		t.Fatalf("listener calls active/peak/successful reconnects = %d/%d/%d, want 0/1/%d", active, peak, recovered, successes)
	}
}

func TestWorkerPollsDuringListenerRecovery(t *testing.T) {
	ctx := workerTestDeadline(t)
	queue := controlledListener()
	writer, root := newTestWriter(t)
	log, logger := newWorkerLog()
	stop, await := startTestWorker(t, newWorker(queue, writer, 20*time.Millisecond, logger))

	const cause = "initial listener socket lost"
	receiveWorkerEvent(t, ctx, queue.waitStarted, "LISTEN wait after startup drain")
	attempt := loseListener(t, ctx, queue, cause)
	for retry := 1; retry <= 2; retry++ {
		sendWorkerEvent(t, ctx, attempt.reply, errors.New("subscription still unavailable"))
		failed := receiveWorkerEvent(t, ctx, attempt.failed, "reconnect failure")
		attempt = receiveWorkerEvent(t, ctx, queue.attempts, "next reconnect attempt")
		if elapsed := attempt.started.Sub(failed); elapsed < time.Second {
			t.Fatalf("retry %d started %s after the previous failure; want at least 1s", retry, elapsed)
		}
	}

	// Startup drain is over; the third attempt is held, and no NOTIFY is sent.
	job := testJob()
	queue.addJob(job)
	checkPublishedJob(t, ctx, root, job)
	if _, _, successes := queue.listenerStats(); successes != 0 {
		t.Fatalf("file was read after %d successful reconnects, want zero", successes)
	}
	log.checkErrors(t, []string{cause}, 2)

	sendWorkerEvent(t, ctx, attempt.reply, error(nil))
	receiveWorkerEvent(t, ctx, queue.waitStarted, "LISTEN wait after recovery")
	stop()
	if err := await(ctx); err != nil {
		t.Fatalf("stop recovered worker: %v", err)
	}
	checkStoppedListener(t, queue, 1)
}

func TestWorkerPollsDuringFirstReconnectAttempt(t *testing.T) {
	ctx := workerTestDeadline(t)
	queue := controlledListener()
	writer, root := newTestWriter(t)
	stop, await := startTestWorker(t, newWorker(queue, writer, 20*time.Millisecond, testLogger()))
	receiveWorkerEvent(t, ctx, queue.waitStarted, "initial LISTEN wait")
	loseListener(t, ctx, queue, "listener disconnected")

	job := testJob()
	queue.addJob(job)
	checkPublishedJob(t, ctx, root, job)
	stop()
	if err := await(ctx); err != nil {
		t.Fatalf("stop worker during first reconnect: %v", err)
	}
	checkStoppedListener(t, queue, 0)
}

func TestWorkerReconnectsListener(t *testing.T) {
	ctx := workerTestDeadline(t)
	queue := controlledListener()
	writer, root := newTestWriter(t)
	log, logger := newWorkerLog()
	stop, await := startTestWorker(t, newWorker(queue, writer, time.Hour, logger))

	const first, second = "first LISTEN loss", "second LISTEN loss"
	receiveWorkerEvent(t, ctx, queue.waitStarted, "initial LISTEN wait")
	attempt := loseListener(t, ctx, queue, first)
	log.checkErrors(t, []string{first}, 0)

	missed := testJob()
	queue.addJob(missed)
	sendWorkerEvent(t, ctx, attempt.reply, error(nil))
	checkPublishedJob(t, ctx, root, missed)
	receiveWorkerEvent(t, ctx, queue.waitStarted, "LISTEN wait after reconnect")

	notified := testJob()
	queue.addJob(notified)
	sendWorkerEvent(t, ctx, queue.notifications, fakeNotification{payload: notified.ID.String()})
	checkPublishedJob(t, ctx, root, notified)
	receiveWorkerEvent(t, ctx, queue.waitStarted, "LISTEN wait after notification")

	loseListener(t, ctx, queue, second)
	log.checkErrors(t, []string{first, second}, 0)
	stop() // The second attempt can return only by cancellation.
	if err := await(ctx); err != nil {
		t.Fatalf("stop worker during reconnect: %v", err)
	}
	checkStoppedListener(t, queue, 1)
	log.checkErrors(t, []string{first, second}, 0)
}

func TestWorkerStopsOnContextCancellation(t *testing.T) {
	ctx := workerTestDeadline(t)
	queue := controlledListener()
	writer, _ := newTestWriter(t)
	log, logger := newWorkerLog()
	stop, await := startTestWorker(t, newWorker(queue, writer, time.Hour, logger))
	receiveWorkerEvent(t, ctx, queue.waitStarted, "LISTEN wait")
	stop()
	if err := await(ctx); err != nil {
		t.Fatalf("stop worker during LISTEN wait: %v", err)
	}
	checkStoppedListener(t, queue, 0)
	log.checkErrors(t, nil, 0)
}

func TestWorkerStopsDuringReconnectPause(t *testing.T) {
	ctx := workerTestDeadline(t)
	queue := controlledListener()
	writer, _ := newTestWriter(t)
	log, logger := newWorkerLog()
	stop, await := startTestWorker(t, newWorker(queue, writer, time.Hour, logger))
	const cause = "LISTEN lost before retry pause"
	receiveWorkerEvent(t, ctx, queue.waitStarted, "initial LISTEN wait")
	attempt := loseListener(t, ctx, queue, cause)
	receiveWorkerEvent(t, ctx, log.written, "initial loss log write")
	sendWorkerEvent(t, ctx, attempt.reply, errors.New("reconnect unavailable"))
	receiveWorkerEvent(t, ctx, log.written, "failed reconnect log write before pause")
	stop()
	stopCtx, cancel := context.WithTimeout(ctx, 900*time.Millisecond)
	defer cancel()
	if err := await(stopCtx); err != nil {
		t.Fatalf("stop worker during reconnect pause: %v", err)
	}
	checkStoppedListener(t, queue, 0)
	if queue.reconnects != 1 { // Run has already joined every listener call.
		t.Fatalf("reconnect attempted %d times, want only the initial attempt", queue.reconnects)
	}
	log.checkErrors(t, []string{cause}, 1)
}

func TestWorkerWriterErrorStopsListenerRecovery(t *testing.T) {
	ctx := workerTestDeadline(t)
	queue := controlledListener()
	writer, root := newTestWriter(t)
	log, logger := newWorkerLog()
	_, await := startTestWorker(t, newWorker(queue, writer, 20*time.Millisecond, logger))
	const cause = "LISTEN lost before writer error"
	receiveWorkerEvent(t, ctx, queue.waitStarted, "initial LISTEN wait")
	loseListener(t, ctx, queue, cause)
	if err := os.Remove(filepath.Join(root, "in")); err != nil {
		t.Fatalf("remove temporary spool input directory: %v", err)
	}
	job := testJob()
	queue.addJob(job)
	err := await(ctx) // No external cancellation before Run returns.
	if !errors.Is(err, fs.ErrNotExist) || !strings.HasPrefix(err.Error(), "write spool job "+job.ID.String()+":") {
		t.Fatalf("Run error = %v, want wrapped writer failure for %s", err, job.ID)
	}
	checkStoppedListener(t, queue, 0)
	log.checkErrors(t, []string{cause}, 0)
}

func requireSpoolActionVersion(t *testing.T, content []byte, want int) {
	t.Helper()
	var doc struct {
		Version int `json:"action_version"`
	}
	if err := json.Unmarshal(content, &doc); err != nil || doc.Version != want {
		t.Fatalf("spool action_version=%d, want %d (decode error: %v)", doc.Version, want, err)
	}
}

func TestSpoolWorkerIntegration(t *testing.T) {
	setupCtx, setupCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer setupCancel()
	dsn := requireTestDSN(setupCtx, t)
	database, err := db.New(setupCtx, dsn)
	if err != nil {
		t.Fatalf("create database: %v", err)
	}
	verification, err := pgxpool.New(setupCtx, dsn)
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

	// Publish the NOTIFY only after the test version is stored. This checks
	// propagation from the DB, not support for launching v2 through the API.
	tx, err := verification.Begin(setupCtx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(setupCtx)
	var processID uuid.UUID
	if err := tx.QueryRow(setupCtx, `SELECT process_id FROM sm_create_input($1,$2)`, source,
		`{"tool":"echo-test","message":"integration"}`).Scan(&processID); err != nil {
		t.Fatal(err)
	}

	var jobID uuid.UUID
	var version int
	if err := tx.QueryRow(setupCtx, `UPDATE sm_jobs SET action_version=2 WHERE process_id=$1
		RETURNING id, action_version`, processID).Scan(&jobID, &version); err != nil {
		t.Fatalf("set integration job version: %v", err)
	}
	if err := tx.Commit(setupCtx); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(root, "in", jobID.String()+".json")
	content := waitForFile(t, setupCtx, path)
	requireSpoolActionVersion(t, content, version)
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
