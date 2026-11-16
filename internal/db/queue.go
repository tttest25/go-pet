package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	listenJobsQuery      = "LISTEN sm_jobs_channel"
	createInputQuery     = `SELECT * FROM sm_create_input($1, $2)`
	pickNextJobQuery     = `SELECT * FROM sm_pick_next_job()`
	updateJobStatusQuery = `SELECT sm_update_job_status($1, $2, $3, $4)`
	getProcessQuery      = `SELECT id, input_id, state, context, created_at, updated_at
		FROM sm_processes WHERE id = $1`
	getProcessJobsQuery = `SELECT id, process_id, tool_name, payload, status, priority,
		result_path, error_msg, created_at, updated_at
		FROM sm_jobs WHERE process_id = $1 ORDER BY created_at ASC, id ASC`
	getProcessOutputsQuery = `SELECT id, process_id, artifact_type, content, metadata, created_at
		FROM sm_outputs WHERE process_id = $1 ORDER BY created_at ASC, id ASC`
	listJobsQuery = `SELECT j.id, j.process_id, j.tool_name, j.payload, j.status, j.priority,
		j.result_path, j.error_msg, j.created_at, j.updated_at
		FROM sm_jobs AS j
		WHERE ($1::uuid IS NULL OR j.process_id = $1::uuid)
		  AND ($2::text IS NULL OR j.status = $2::text)
		  AND ($3::uuid IS NULL OR (j.created_at, j.id) < (
		      SELECT c.created_at, c.id FROM sm_jobs AS c WHERE c.id = $3::uuid
		  ))
		ORDER BY j.created_at DESC, j.id DESC LIMIT $4`
)

func (d *DB) GetProcess(ctx context.Context, processID uuid.UUID) (ProcessDetails, error) {
	result := ProcessDetails{Jobs: make([]JobDetails, 0), Outputs: make([]OutputDetails, 0)}
	if err := d.pool.QueryRow(ctx, getProcessQuery, processID).Scan(
		&result.ID, &result.InputID, &result.State, &result.Context,
		&result.CreatedAt, &result.UpdatedAt,
	); err != nil {
		return ProcessDetails{}, fmt.Errorf("get process %s: %w", processID, err)
	}

	jobs, err := d.pool.Query(ctx, getProcessJobsQuery, processID)
	if err != nil {
		return ProcessDetails{}, fmt.Errorf("get process %s jobs: %w", processID, err)
	}
	for jobs.Next() {
		var job JobDetails
		if err := jobs.Scan(&job.ID, &job.ProcessID, &job.ToolName, &job.Payload,
			&job.Status, &job.Priority, &job.ResultPath, &job.ErrorMsg,
			&job.CreatedAt, &job.UpdatedAt); err != nil {
			jobs.Close()
			return ProcessDetails{}, fmt.Errorf("scan process %s job: %w", processID, err)
		}
		result.Jobs = append(result.Jobs, job)
	}
	err = jobs.Err()
	jobs.Close()
	if err != nil {
		return ProcessDetails{}, fmt.Errorf("iterate process %s jobs: %w", processID, err)
	}

	outputs, err := d.pool.Query(ctx, getProcessOutputsQuery, processID)
	if err != nil {
		return ProcessDetails{}, fmt.Errorf("get process %s outputs: %w", processID, err)
	}
	defer outputs.Close()
	for outputs.Next() {
		var output OutputDetails
		if err := outputs.Scan(&output.ID, &output.ProcessID, &output.ArtifactType,
			&output.Content, &output.Metadata, &output.CreatedAt); err != nil {
			return ProcessDetails{}, fmt.Errorf("scan process %s output: %w", processID, err)
		}
		result.Outputs = append(result.Outputs, output)
	}
	if err := outputs.Err(); err != nil {
		return ProcessDetails{}, fmt.Errorf("iterate process %s outputs: %w", processID, err)
	}
	return result, nil
}

func (d *DB) ListJobs(ctx context.Context, filter JobFilter) ([]JobDetails, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = 20
	} else if limit > 100 {
		limit = 100
	}
	rows, err := d.pool.Query(ctx, listJobsQuery, filter.ProcessID, filter.Status, filter.CursorID, limit)
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	defer rows.Close()
	jobs := make([]JobDetails, 0)
	for rows.Next() {
		var job JobDetails
		if err := rows.Scan(&job.ID, &job.ProcessID, &job.ToolName, &job.Payload,
			&job.Status, &job.Priority, &job.ResultPath, &job.ErrorMsg,
			&job.CreatedAt, &job.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan listed job: %w", err)
		}
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate listed jobs: %w", err)
	}
	return jobs, nil
}

// ErrInvalidCallbackStatus indicates an invalid callback status.
var ErrInvalidCallbackStatus = errors.New("invalid callback status")

func mapQueueError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "P0002":
			return fmt.Errorf("%w: %w", pgx.ErrNoRows, err)
		case "22023":
			return fmt.Errorf("%w: %w", ErrInvalidCallbackStatus, err)
		}
	}
	return err
}

type CreatedInput struct {
	InputID   uuid.UUID
	ProcessID uuid.UUID
	Status    string
}

func (d *DB) CreateInput(ctx context.Context, source string, payload json.RawMessage) (CreatedInput, error) {
	var created CreatedInput
	err := d.pool.QueryRow(ctx, createInputQuery, source, payload).Scan(
		&created.InputID, &created.ProcessID, &created.Status,
	)
	if err != nil {
		return CreatedInput{}, fmt.Errorf("create input from source %q: %w", source, mapQueueError(err))
	}
	return created, nil
}

func (d *DB) PickNextJob(ctx context.Context) (*Job, error) {
	var job Job
	err := d.pool.QueryRow(ctx, pickNextJobQuery).Scan(
		&job.ID, &job.ProcessID, &job.ToolName, &job.Payload, &job.OldStatus, &job.Status,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("pick next pending job: %w", mapQueueError(err))
	}
	return &job, nil
}

func (d *DB) UpdateJobStatus(ctx context.Context, jobID uuid.UUID, status Status, resultPath string, errorMsg string) error {
	_, err := d.pool.Exec(ctx, updateJobStatusQuery, jobID, status, resultPath, errorMsg)
	if err != nil {
		return fmt.Errorf("update job %s status to %q: %w", jobID, status, mapQueueError(err))
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
