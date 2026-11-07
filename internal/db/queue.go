package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	listenJobsQuery  = "LISTEN sm_jobs_channel"
	createInputQuery = `
INSERT INTO sm_inputs (source, payload)
VALUES ($1, $2)
RETURNING id`
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

func (d *DB) CreateInput(
	ctx context.Context,
	source string,
	payload json.RawMessage,
) (uuid.UUID, error) {
	var inputID uuid.UUID
	if err := d.pool.QueryRow(
		ctx,
		createInputQuery,
		source,
		payload,
	).Scan(&inputID); err != nil {
		return uuid.Nil, fmt.Errorf("create input from source %q: %w", source, err)
	}

	return inputID, nil
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
