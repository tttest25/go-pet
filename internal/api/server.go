package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"petar/internal/db"
)

const maxRequestBodyBytes = 1 << 20

type databaseAPI interface {
	Ping(ctx context.Context) error

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
	database databaseAPI
	handler  http.Handler
	logger   *slog.Logger
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

func NewServer(database *db.DB, logger *slog.Logger) *Server {
	return newServer(database, logger)
}

func newServer(database databaseAPI, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}

	server := &Server{
		database: database,
		logger:   logger,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", server.handleHealthz)
	mux.HandleFunc("GET /readyz", server.handleReadyz)
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

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if err := s.database.Ping(r.Context()); err != nil {
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
	if _, err := validateInputPayload(request.Payload); err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	created, err := s.database.CreateInput(r.Context(), source, request.Payload)
	if err != nil {
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

	if err := s.database.UpdateJobStatus(
		r.Context(),
		jobID,
		request.Status,
		request.ResultPath,
		request.ErrorMsg,
	); err != nil {
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

func validateInputPayload(payload json.RawMessage) (string, error) {
	if len(payload) == 0 {
		return "", errors.New("payload is required")
	}

	var object map[string]json.RawMessage
	if err := json.Unmarshal(payload, &object); err != nil {
		return "", fmt.Errorf("payload must be a JSON object: %w", err)
	}
	if object == nil {
		return "", errors.New("payload must be a JSON object")
	}

	rawTool, ok := object["tool"]
	if !ok {
		return "", errors.New("payload.tool is required")
	}
	var tool string
	if err := json.Unmarshal(rawTool, &tool); err != nil {
		return "", fmt.Errorf("payload.tool must be a string: %w", err)
	}

	tool = strings.TrimSpace(tool)
	switch tool {
	case "echo-test", "yt-dlp", "fabric":
		return tool, nil
	default:
		return "", fmt.Errorf("unsupported payload.tool %q", tool)
	}
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
