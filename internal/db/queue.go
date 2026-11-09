package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	listenJobsQuery  = "LISTEN sm_jobs_channel"
	createInputQuery = `
INSERT INTO sm_inputs (source, payload)
VALUES ($1, $2)
RETURNING id, status`
	findCreatedProcessQuery = `
SELECT id
FROM sm_processes
WHERE input_id = $1
ORDER BY created_at ASC
LIMIT 1`
	pickNextJobQuery = `
UPDATE sm_jobs
SET
    status = 'dispatched',
    updated_at = NOW()
WHERE id = (
    SELECT id
    FROM sm_jobs
    WHERE status = 'pending'
    ORDER BY priority DESC, created_at ASC
    FOR UPDATE SKIP LOCKED
    LIMIT 1
)
RETURNING
    id,
    process_id,
    tool_name,
    payload,
    OLD.status,
    NEW.status`
	updateJobStatusQuery = `
UPDATE sm_jobs
SET
    status = $2,
    result_path = NULLIF($3, ''),
    error_msg = NULLIF($4, ''),
    updated_at = NOW()
WHERE id = $1
  AND status IN ('dispatched', 'in_progress')
RETURNING
    process_id,
    OLD.status,
    NEW.status`
	insertOutputQuery = `
INSERT INTO sm_outputs (
    process_id,
    artifact_type,
    metadata
)
VALUES (
    $1,
    'result',
    jsonb_build_object('result_path', NULLIF($2, ''))
)`
)

type CreatedInput struct {
	InputID   uuid.UUID
	ProcessID uuid.UUID
	Status    string
}

func (d *DB) CreateInput(
	ctx context.Context,
	source string,
	payload json.RawMessage,
) (CreatedInput, error) {
	tx, err := d.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return CreatedInput{}, fmt.Errorf(
			"begin create input from source %q: %w",
			source,
			err,
		)
	}
	defer func() {
		rollbackCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(rollbackCtx)
	}()

	var created CreatedInput
	if err := tx.QueryRow(
		ctx,
		createInputQuery,
		source,
		payload,
	).Scan(&created.InputID, &created.Status); err != nil {
		return CreatedInput{}, fmt.Errorf("create input from source %q: %w", source, err)
	}

	if err := tx.QueryRow(
		ctx,
		findCreatedProcessQuery,
		created.InputID,
	).Scan(&created.ProcessID); err != nil {
		return CreatedInput{}, fmt.Errorf(
			"find created process for input %s: %w",
			created.InputID,
			err,
		)
	}

	if err := tx.Commit(ctx); err != nil {
		return CreatedInput{}, fmt.Errorf("commit create input %s: %w", created.InputID, err)
	}

	return created, nil
}

func (d *DB) PickNextJob(ctx context.Context) (*Job, error) {
	var job Job
	err := d.pool.QueryRow(ctx, pickNextJobQuery).Scan(
		&job.ID,
		&job.ProcessID,
		&job.ToolName,
		&job.Payload,
		&job.OldStatus,
		&job.Status,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("pick next pending job: %w", err)
	}

	return &job, nil
}

func (d *DB) UpdateJobStatus(
	ctx context.Context,
	jobID uuid.UUID,
	status Status,
	resultPath string,
	errorMsg string,
) error {
	switch status {
	case StatusInProgress, StatusCompleted, StatusFailed:
	default:
		return fmt.Errorf("update job %s: invalid callback status %q", jobID, status)
	}

	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin status update for job %s: %w", jobID, err)
	}
	defer tx.Rollback(ctx)

	var processID uuid.UUID
	var oldStatus Status
	var newStatus Status
	if err := tx.QueryRow(
		ctx,
		updateJobStatusQuery,
		jobID,
		status,
		resultPath,
		errorMsg,
	).Scan(&processID, &oldStatus, &newStatus); err != nil {
		return fmt.Errorf("update job %s status to %q: %w", jobID, status, err)
	}

	if status == StatusCompleted {
		if _, err := tx.Exec(
			ctx,
			insertOutputQuery,
			processID,
			resultPath,
		); err != nil {
			return fmt.Errorf("create output for completed job %s: %w", jobID, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit status update for job %s: %w", jobID, err)
	}

	return nil
}

func (d *DB) WaitForNotification(ctx context.Context) (string, error) {
	d.listenerMu.Lock()
	defer d.listenerMu.Unlock()

	if d.listener == nil {
		return "", fmt.Errorf("wait for notification on sm_jobs_channel: listener is closed")
	}

	notification, err := d.listener.Conn().WaitForNotification(ctx)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", fmt.Errorf("wait for notification on sm_jobs_channel: %w", ctxErr)
		}
		return "", fmt.Errorf("listener connection lost on sm_jobs_channel: %w", err)
	}

	return notification.Payload, nil
}
