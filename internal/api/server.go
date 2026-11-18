package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"petar/internal/db"
)

const maxRequestBodyBytes = 1 << 20

var errDatabaseDeadline = errors.New("HTTP database deadline exceeded")

type databaseAPI interface {
	Ping(ctx context.Context) error
	GetProcess(ctx context.Context, processID uuid.UUID) (db.ProcessDetails, error)
	ListJobs(ctx context.Context, filter db.JobFilter) ([]db.JobDetails, error)
	ListActions(ctx context.Context) ([]db.ActionVersion, error)

	CreateInput(
		ctx context.Context,
		source string,
		payload json.RawMessage,
	) (db.CreatedInput, error)

	UpdateJobStatus(
		ctx context.Context,
		jobID uuid.UUID,
		status db.Status,
		resultPath string,
		errorMsg string,
	) error
}

type Server struct {
	databaseTimeout time.Duration
	database        databaseAPI
	handler         http.Handler
	logger          *slog.Logger
}

type createInputRequest struct {
	Source  string          `json:"source"`
	Payload json.RawMessage `json:"payload"`
}

type createInputResponse struct {
	InputID   uuid.UUID `json:"input_id"`
	ProcessID uuid.UUID `json:"process_id"`
	Status    string    `json:"status"`
	ID        uuid.UUID `json:"id"`
}

type updateJobStatusRequest struct {
	Status     db.Status `json:"status"`
	ResultPath string    `json:"result_path,omitempty"`
	ErrorMsg   string    `json:"error_msg,omitempty"`
}

type statusResponse struct {
	Status string `json:"status"`
}

type errorResponse struct {
	Error string `json:"error"`
}

type actionVersionResponse struct {
	Action  string `json:"action"`
	Version int    `json:"version"`
	Title   string `json:"title"`
}

type processDetailsResponse struct {
	ID        uuid.UUID               `json:"id"`
	InputID   uuid.UUID               `json:"input_id"`
	State     string                  `json:"state"`
	Context   json.RawMessage         `json:"context"`
	CreatedAt time.Time               `json:"created_at"`
	UpdatedAt time.Time               `json:"updated_at"`
	Jobs      []jobDetailsResponse    `json:"jobs"`
	Outputs   []outputDetailsResponse `json:"outputs"`
}

type jobDetailsResponse struct {
	ID         uuid.UUID       `json:"id"`
	ProcessID  uuid.UUID       `json:"process_id"`
	ToolName   string          `json:"tool_name"`
	Payload    json.RawMessage `json:"payload"`
	Status     db.Status       `json:"status"`
	Priority   int             `json:"priority"`
	ResultPath *string         `json:"result_path"`
	ErrorMsg   *string         `json:"error_msg"`
	CreatedAt  time.Time       `json:"created_at"`
	UpdatedAt  time.Time       `json:"updated_at"`
}

type outputDetailsResponse struct {
	ID           uuid.UUID       `json:"id"`
	ProcessID    uuid.UUID       `json:"process_id"`
	ArtifactType string          `json:"artifact_type"`
	Content      *string         `json:"content"`
	Metadata     json.RawMessage `json:"metadata"`
	CreatedAt    time.Time       `json:"created_at"`
}

func jobsResponse(jobs []db.JobDetails) []jobDetailsResponse {
	result := make([]jobDetailsResponse, len(jobs))
	for i, job := range jobs {
		result[i] = jobDetailsResponse{
			ID: job.ID, ProcessID: job.ProcessID, ToolName: job.ToolName,
			Payload: job.Payload, Status: job.Status, Priority: job.Priority,
			ResultPath: job.ResultPath, ErrorMsg: job.ErrorMsg,
			CreatedAt: job.CreatedAt, UpdatedAt: job.UpdatedAt,
		}
	}
	return result
}

func NewServer(database *db.DB, logger *slog.Logger) *Server {
	return newServer(database, logger)
}

func newServer(database databaseAPI, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}

	server := &Server{
		databaseTimeout: 5 * time.Second,
		database:        database,
		logger:          logger,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", server.handleHealthz)
	mux.HandleFunc("GET /readyz", server.handleReadyz)
	mux.HandleFunc("GET /api/v1/processes/{id}", server.handleGetProcess)
	mux.HandleFunc("GET /api/v1/jobs", server.handleListJobs)
	mux.HandleFunc("GET /api/v1/actions", server.handleListActions)
	mux.HandleFunc("POST /api/v1/inputs", server.handleCreateInput)
	mux.HandleFunc(
		"POST /api/v1/jobs/{id}/status",
		server.handleUpdateJobStatus,
	)
	server.handler = loggingMiddleware(logger)(mux)

	return server
}

func (s *Server) Handler() http.Handler {
	return s.handler
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, statusResponse{Status: "ok"})
}

func (s *Server) handleGetProcess(w http.ResponseWriter, r *http.Request) {
	processID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid process ID")
		return
	}
	ctx, cancel := context.WithTimeoutCause(r.Context(), s.databaseTimeout, errDatabaseDeadline)
	defer cancel()
	process, err := s.database.GetProcess(ctx, processID)
	if err != nil {
		if s.writeDatabaseTimeout(ctx, w) {
			return
		}
		if errors.Is(err, pgx.ErrNoRows) {
			s.writeError(w, http.StatusNotFound, "process not found")
			return
		}
		s.logger.Error("get process through HTTP API failed",
			"method", r.Method, "path", r.URL.Path, "process_id", processID, "error", err)
		s.writeError(w, http.StatusInternalServerError, "failed to get process")
		return
	}
	outputs := make([]outputDetailsResponse, len(process.Outputs))
	for i, output := range process.Outputs {
		outputs[i] = outputDetailsResponse{
			ID: output.ID, ProcessID: output.ProcessID, ArtifactType: output.ArtifactType,
			Content: output.Content, Metadata: output.Metadata, CreatedAt: output.CreatedAt,
		}
	}
	s.writeJSON(w, http.StatusOK, processDetailsResponse{
		ID: process.ID, InputID: process.InputID, State: process.State, Context: process.Context,
		CreatedAt: process.CreatedAt, UpdatedAt: process.UpdatedAt,
		Jobs: jobsResponse(process.Jobs), Outputs: outputs,
	})
}

func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid query parameters")
		return
	}
	filter := db.JobFilter{Limit: 20}
	for _, name := range []string{"process_id", "status", "limit", "cursor"} {
		values, present := query[name]
		if !present {
			continue
		}
		if len(values) != 1 || values[0] == "" {
			s.writeError(w, http.StatusBadRequest, "invalid "+name)
			return
		}
		value := values[0]
		switch name {
		case "process_id", "cursor":
			id, err := uuid.Parse(value)
			if err != nil {
				s.writeError(w, http.StatusBadRequest, "invalid "+name)
				return
			}
			if name == "process_id" {
				filter.ProcessID = &id
			} else {
				filter.CursorID = &id
			}
		case "status":
			status := db.Status(value)
			switch status {
			case db.StatusPending, db.StatusDispatched, db.StatusInProgress, db.StatusCompleted, db.StatusFailed:
				filter.Status = &status
			default:
				s.writeError(w, http.StatusBadRequest, "invalid status")
				return
			}
		case "limit":
			limit, err := strconv.Atoi(value)
			if err != nil || limit <= 0 {
				s.writeError(w, http.StatusBadRequest, "invalid limit")
				return
			}
			filter.Limit = min(limit, 100)
		}
	}
	ctx, cancel := context.WithTimeoutCause(r.Context(), s.databaseTimeout, errDatabaseDeadline)
	defer cancel()
	jobs, err := s.database.ListJobs(ctx, filter)
	if err != nil {
		if s.writeDatabaseTimeout(ctx, w) {
			return
		}
		s.logger.Error("list jobs through HTTP API failed",
			"method", r.Method, "path", r.URL.Path, "error", err)
		s.writeError(w, http.StatusInternalServerError, "failed to list jobs")
		return
	}
	s.writeJSON(w, http.StatusOK, jobsResponse(jobs))
}

func (s *Server) handleListActions(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeoutCause(r.Context(), s.databaseTimeout, errDatabaseDeadline)
	defer cancel()
	actions, err := s.database.ListActions(ctx)
	if err != nil {
		if s.writeDatabaseTimeout(ctx, w) {
			return
		}
		s.logger.Error("list actions through HTTP API failed",
			"method", r.Method, "path", r.URL.Path, "error", err)
		s.writeError(w, http.StatusInternalServerError, "failed to list actions")
		return
	}
	result := make([]actionVersionResponse, len(actions))
	for i, action := range actions {
		result[i] = actionVersionResponse{
			Action: action.Action, Version: action.Version, Title: action.Title,
		}
	}
	s.writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeoutCause(r.Context(), s.databaseTimeout, errDatabaseDeadline)
	defer cancel()
	if err := s.database.Ping(ctx); err != nil {
		s.logger.Error(
			"database readiness probe failed",
			"method", r.Method,
			"path", r.URL.Path,
			"error", err,
		)
		s.writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}

	s.writeJSON(w, http.StatusOK, statusResponse{Status: "ready"})
}

func (s *Server) handleCreateInput(w http.ResponseWriter, r *http.Request) {
	var request createInputRequest
	if err := decodeJSON(w, r, &request); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid JSON request body")
		return
	}

	source := strings.TrimSpace(request.Source)
	if source == "" {
		s.writeError(w, http.StatusBadRequest, "source is required")
		return
	}
	if len(request.Payload) == 0 || request.Payload[0] != '{' {
		s.writeError(w, http.StatusBadRequest, "payload must be a JSON object")
		return
	}

	ctx, cancel := context.WithTimeoutCause(r.Context(), s.databaseTimeout, errDatabaseDeadline)
	defer cancel()
	created, err := s.database.CreateInput(ctx, source, request.Payload)
	if err != nil {
		if s.writeDatabaseTimeout(ctx, w) {
			return
		}
		if errors.Is(err, db.ErrInvalidInput) {
			s.writeError(w, http.StatusBadRequest, "invalid payload")
			return
		}
		s.logger.Error(
			"create input through HTTP API failed",
			"method", r.Method,
			"path", r.URL.Path,
			"error", err,
		)
		s.writeError(w, http.StatusInternalServerError, "failed to create input")
		return
	}

	s.writeJSON(w, http.StatusCreated, createInputResponse{
		InputID:   created.InputID,
		ProcessID: created.ProcessID,
		Status:    created.Status,
		ID:        created.InputID,
	})
}

func (s *Server) handleUpdateJobStatus(w http.ResponseWriter, r *http.Request) {
	jobID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid job ID")
		return
	}

	var request updateJobStatusRequest
	if err := decodeJSON(w, r, &request); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid JSON request body")
		return
	}

	switch request.Status {
	case db.StatusInProgress:
	case db.StatusCompleted:
		if strings.TrimSpace(request.ResultPath) == "" {
			s.writeError(
				w,
				http.StatusBadRequest,
				"result_path is required for completed status",
			)
			return
		}
	case db.StatusFailed:
		if strings.TrimSpace(request.ErrorMsg) == "" {
			s.writeError(
				w,
				http.StatusBadRequest,
				"error_msg is required for failed status",
			)
			return
		}
	default:
		s.writeError(w, http.StatusBadRequest, "invalid job status")
		return
	}

	ctx, cancel := context.WithTimeoutCause(r.Context(), s.databaseTimeout, errDatabaseDeadline)
	defer cancel()
	if err := s.database.UpdateJobStatus(
		ctx,
		jobID,
		request.Status,
		request.ResultPath,
		request.ErrorMsg,
	); err != nil {
		if s.writeDatabaseTimeout(ctx, w) {
			return
		}
		if errors.Is(err, pgx.ErrNoRows) {
			s.writeError(
				w,
				http.StatusNotFound,
				"job not found or not updatable",
			)
			return
		}

		s.logger.Error(
			"update job status through HTTP API failed",
			"method", r.Method,
			"path", r.URL.Path,
			"job_id", jobID,
			"error", err,
		)
		s.writeError(w, http.StatusInternalServerError, "failed to update job status")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// Only called on database errors: a successful result wins over a late timer.
func (s *Server) writeDatabaseTimeout(ctx context.Context, w http.ResponseWriter) bool {
	if !errors.Is(context.Cause(ctx), errDatabaseDeadline) {
		return false
	}
	s.writeError(w, http.StatusGatewayTimeout, "database request timed out")
	return true
}

func decodeJSON(
	w http.ResponseWriter,
	r *http.Request,
	destination any,
) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("decode request body: %w", err)
	}

	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("request body must contain one JSON document")
		}
		return fmt.Errorf("decode trailing request body: %w", err)
	}

	return nil
}

func (s *Server) writeError(
	w http.ResponseWriter,
	status int,
	message string,
) {
	s.writeJSON(w, status, errorResponse{Error: message})
}

func (s *Server) writeJSON(
	w http.ResponseWriter,
	status int,
	body any,
) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		s.logger.Error("encode HTTP JSON response", "error", err)
	}
}
