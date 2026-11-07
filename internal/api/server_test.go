package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"petar/internal/config"
	"petar/internal/db"
)

type fakeDatabase struct {
	pingErr        error
	createInputErr error
	updateErr      error

	inputID uuid.UUID

	pingCalls        int
	createInputCalls int
	updateCalls      int

	source     string
	payload    json.RawMessage
	jobID      uuid.UUID
	status     db.Status
	resultPath string
	errorMsg   string
}

func newFakeDatabase() *fakeDatabase {
	return &fakeDatabase{inputID: uuid.New()}
}

func (f *fakeDatabase) Ping(context.Context) error {
	f.pingCalls++
	return f.pingErr
}

func (f *fakeDatabase) CreateInput(
	_ context.Context,
	source string,
	payload json.RawMessage,
) (uuid.UUID, error) {
	f.createInputCalls++
	f.source = source
	f.payload = payload
	if f.createInputErr != nil {
		return uuid.Nil, f.createInputErr
	}
	return f.inputID, nil
}

func (f *fakeDatabase) UpdateJobStatus(
	_ context.Context,
	jobID uuid.UUID,
	status db.Status,
	resultPath string,
	errorMsg string,
) error {
	f.updateCalls++
	f.jobID = jobID
	f.status = status
	f.resultPath = resultPath
	f.errorMsg = errorMsg
	return f.updateErr
}

func apiTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func performRequest(
	server *Server,
	method string,
	target string,
	body string,
) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	return response
}

func requireJSONContentType(t *testing.T, response *httptest.ResponseRecorder) {
	t.Helper()

	contentType := response.Header().Get("Content-Type")
	if contentType != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want application/json", contentType)
	}
}

func TestHealthz(t *testing.T) {
	database := newFakeDatabase()
	server := newServer(database, apiTestLogger())

	response := performRequest(server, http.MethodGet, "/healthz", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	requireJSONContentType(t, response)

	var body statusResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode health response: %v", err)
	}
	if body.Status != "ok" {
		t.Fatalf("health status = %q, want ok", body.Status)
	}
	if database.pingCalls != 0 {
		t.Fatal("health probe called Ping")
	}
}

func TestReadyz(t *testing.T) {
	t.Run("ready", func(t *testing.T) {
		database := newFakeDatabase()
		server := newServer(database, apiTestLogger())
		response := performRequest(server, http.MethodGet, "/readyz", "")

		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", response.Code)
		}
		requireJSONContentType(t, response)
		var body statusResponse
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode readiness response: %v", err)
		}
		if body.Status != "ready" {
			t.Fatalf("readiness status = %q, want ready", body.Status)
		}
		if database.pingCalls != 1 {
			t.Fatalf("Ping calls = %d, want 1", database.pingCalls)
		}
	})

	t.Run("database unavailable", func(t *testing.T) {
		database := newFakeDatabase()
		database.pingErr = errors.New("sensitive ping failure")
		server := newServer(database, apiTestLogger())
		response := performRequest(server, http.MethodGet, "/readyz", "")

		if response.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", response.Code)
		}
		requireJSONContentType(t, response)
		if strings.Contains(response.Body.String(), "sensitive") {
			t.Fatalf("response leaked internal error: %s", response.Body)
		}
	})
}

func TestCreateInput(t *testing.T) {
	database := newFakeDatabase()
	server := newServer(database, apiTestLogger())
	body := `{"source":"manual","payload":{"tool":"echo-test","message":"hello"}}`

	response := performRequest(server, http.MethodPost, "/api/v1/inputs", body)
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", response.Code, response.Body)
	}
	requireJSONContentType(t, response)

	var responseBody createInputResponse
	if err := json.Unmarshal(response.Body.Bytes(), &responseBody); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if responseBody.ID != database.inputID {
		t.Fatalf("response ID = %s, want %s", responseBody.ID, database.inputID)
	}
	if database.createInputCalls != 1 {
		t.Fatalf("CreateInput calls = %d, want 1", database.createInputCalls)
	}
	if database.source != "manual" {
		t.Fatalf("source = %q, want manual", database.source)
	}

	var payload map[string]any
	if err := json.Unmarshal(database.payload, &payload); err != nil {
		t.Fatalf("database received invalid payload: %v", err)
	}
	if payload["tool"] != "echo-test" || payload["message"] != "hello" {
		t.Fatalf("database payload = %#v", payload)
	}
}

func TestCreateInputValidation(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "empty body", body: ""},
		{name: "malformed JSON", body: `{"`},
		{
			name: "multiple documents",
			body: `{"source":"manual","payload":{"tool":"echo-test"}} {}`,
		},
		{
			name: "unknown field",
			body: `{"source":"manual","payload":{"tool":"echo-test"},"extra":true}`,
		},
		{
			name: "missing source",
			body: `{"payload":{"tool":"echo-test"}}`,
		},
		{
			name: "blank source",
			body: `{"source":"  ","payload":{"tool":"echo-test"}}`,
		},
		{name: "missing payload", body: `{"source":"manual"}`},
		{
			name: "null payload",
			body: `{"source":"manual","payload":null}`,
		},
		{
			name: "payload is string",
			body: `{"source":"manual","payload":"echo-test"}`,
		},
		{
			name: "missing tool",
			body: `{"source":"manual","payload":{"message":"hello"}}`,
		},
		{
			name: "unsupported tool",
			body: `{"source":"manual","payload":{"tool":"unsupported"}}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			database := newFakeDatabase()
			server := newServer(database, apiTestLogger())
			response := performRequest(
				server,
				http.MethodPost,
				"/api/v1/inputs",
				test.body,
			)

			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", response.Code, response.Body)
			}
			requireJSONContentType(t, response)
			if database.createInputCalls != 0 {
				t.Fatal("invalid input reached CreateInput")
			}
		})
	}
}

func TestCreateInputDatabaseError(t *testing.T) {
	database := newFakeDatabase()
	database.createInputErr = errors.New("sensitive insert failure")
	server := newServer(database, apiTestLogger())

	response := performRequest(
		server,
		http.MethodPost,
		"/api/v1/inputs",
		`{"source":"manual","payload":{"tool":"echo-test"}}`,
	)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", response.Code)
	}
	if strings.Contains(response.Body.String(), "sensitive") {
		t.Fatalf("response leaked internal error: %s", response.Body)
	}
}

func TestUpdateJobStatus(t *testing.T) {
	jobID := uuid.New()
	tests := []struct {
		name       string
		body       string
		status     db.Status
		resultPath string
		errorMsg   string
	}{
		{
			name:   "in progress",
			body:   `{"status":"in_progress"}`,
			status: db.StatusInProgress,
		},
		{
			name:       "completed",
			body:       `{"status":"completed","result_path":"/spool/out/job/result.json"}`,
			status:     db.StatusCompleted,
			resultPath: "/spool/out/job/result.json",
		},
		{
			name:     "failed",
			body:     `{"status":"failed","error_msg":"runner failed"}`,
			status:   db.StatusFailed,
			errorMsg: "runner failed",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			database := newFakeDatabase()
			server := newServer(database, apiTestLogger())
			target := "/api/v1/jobs/" + jobID.String() + "/status"
			response := performRequest(server, http.MethodPost, target, test.body)

			if response.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want 204: %s", response.Code, response.Body)
			}
			if response.Body.Len() != 0 {
				t.Fatalf("204 response has body: %q", response.Body)
			}
			if database.updateCalls != 1 {
				t.Fatalf("UpdateJobStatus calls = %d, want 1", database.updateCalls)
			}
			if database.jobID != jobID || database.status != test.status {
				t.Fatalf(
					"update arguments = %s/%q, want %s/%q",
					database.jobID,
					database.status,
					jobID,
					test.status,
				)
			}
			if database.resultPath != test.resultPath || database.errorMsg != test.errorMsg {
				t.Fatalf(
					"update path/error = %q/%q, want %q/%q",
					database.resultPath,
					database.errorMsg,
					test.resultPath,
					test.errorMsg,
				)
			}
		})
	}
}

func TestUpdateJobStatusValidation(t *testing.T) {
	jobID := uuid.NewString()
	tests := []struct {
		name  string
		jobID string
		body  string
	}{
		{name: "invalid UUID", jobID: "not-a-uuid", body: `{"status":"in_progress"}`},
		{name: "malformed JSON", jobID: jobID, body: `{"`},
		{
			name:  "unknown field",
			jobID: jobID,
			body:  `{"status":"in_progress","extra":true}`,
		},
		{name: "pending", jobID: jobID, body: `{"status":"pending"}`},
		{name: "dispatched", jobID: jobID, body: `{"status":"dispatched"}`},
		{name: "unknown status", jobID: jobID, body: `{"status":"unknown"}`},
		{name: "completed without path", jobID: jobID, body: `{"status":"completed"}`},
		{name: "failed without message", jobID: jobID, body: `{"status":"failed"}`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			database := newFakeDatabase()
			server := newServer(database, apiTestLogger())
			response := performRequest(
				server,
				http.MethodPost,
				"/api/v1/jobs/"+test.jobID+"/status",
				test.body,
			)

			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", response.Code, response.Body)
			}
			if database.updateCalls != 0 {
				t.Fatal("invalid callback reached UpdateJobStatus")
			}
		})
	}
}

func TestUpdateJobStatusDatabaseErrors(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
	}{
		{
			name:       "not found or not updatable",
			err:        fmt.Errorf("update failed: %w", pgx.ErrNoRows),
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "internal error",
			err:        errors.New("sensitive update failure"),
			wantStatus: http.StatusInternalServerError,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			database := newFakeDatabase()
			database.updateErr = test.err
			server := newServer(database, apiTestLogger())
			response := performRequest(
				server,
				http.MethodPost,
				"/api/v1/jobs/"+uuid.NewString()+"/status",
				`{"status":"in_progress"}`,
			)

			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, test.wantStatus)
			}
			if strings.Contains(response.Body.String(), "sensitive") {
				t.Fatalf("response leaked internal error: %s", response.Body)
			}
		})
	}
}

func TestRouting(t *testing.T) {
	server := newServer(newFakeDatabase(), apiTestLogger())
	tests := []struct {
		method string
		target string
		want   int
	}{
		{http.MethodGet, "/api/v1/inputs", http.StatusMethodNotAllowed},
		{http.MethodPost, "/healthz", http.StatusMethodNotAllowed},
		{http.MethodGet, "/unknown", http.StatusNotFound},
	}

	for _, test := range tests {
		response := performRequest(server, test.method, test.target, "")
		if response.Code != test.want {
			t.Fatalf("%s %s status = %d, want %d",
				test.method,
				test.target,
				response.Code,
				test.want,
			)
		}
	}
}

func TestServerIntegration(t *testing.T) {
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	database, err := db.New(ctx, cfg.DBDSN)
	if err != nil {
		t.Fatalf("create database: %v", err)
	}
	verification, err := pgxpool.New(ctx, cfg.DBDSN)
	if err != nil {
		database.Close()
		t.Fatalf("create verification pool: %v", err)
	}

	source := "step5-test-" + uuid.NewString()
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

	httpServer := httptest.NewServer(NewServer(database, apiTestLogger()).Handler())
	t.Cleanup(httpServer.Close)

	requestBody, err := json.Marshal(createInputRequest{
		Source: source,
		Payload: json.RawMessage(
			`{"tool":"echo-test","message":"integration"}`,
		),
	})
	if err != nil {
		t.Fatalf("marshal integration input: %v", err)
	}
	response, err := postJSON(ctx, httpServer.Client(), httpServer.URL+"/api/v1/inputs", requestBody)
	if err != nil {
		t.Fatalf("post integration input: %v", err)
	}
	if response.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("create input status = %d: %s", response.StatusCode, body)
	}
	var created createInputResponse
	if err := json.NewDecoder(response.Body).Decode(&created); err != nil {
		response.Body.Close()
		t.Fatalf("decode integration input ID: %v", err)
	}
	response.Body.Close()

	var jobID uuid.UUID
	if err := verification.QueryRow(
		ctx,
		`SELECT j.id
		 FROM sm_jobs j
		 JOIN sm_processes p ON p.id = j.process_id
		 WHERE p.input_id = $1`,
		created.ID,
	).Scan(&jobID); err != nil {
		t.Fatalf("query integration job: %v", err)
	}
	if _, err := verification.Exec(
		ctx,
		`UPDATE sm_jobs
		 SET status = 'dispatched', updated_at = NOW()
		 WHERE id = $1`,
		jobID,
	); err != nil {
		t.Fatalf("dispatch integration job: %v", err)
	}

	callbackURL := httpServer.URL + "/api/v1/jobs/" + jobID.String() + "/status"
	for _, body := range [][]byte{
		[]byte(`{"status":"in_progress"}`),
		[]byte(`{"status":"completed","result_path":"/spool/out/step5/result.json"}`),
	} {
		response, err := postJSON(ctx, httpServer.Client(), callbackURL, body)
		if err != nil {
			t.Fatalf("post callback %s: %v", body, err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusNoContent {
			t.Fatalf("callback %s status = %d", body, response.StatusCode)
		}
	}

	var jobStatus db.Status
	var processState string
	if err := verification.QueryRow(
		ctx,
		`SELECT j.status, p.state
		 FROM sm_jobs j
		 JOIN sm_processes p ON p.id = j.process_id
		 WHERE j.id = $1`,
		jobID,
	).Scan(&jobStatus, &processState); err != nil {
		t.Fatalf("query final integration state: %v", err)
	}
	if jobStatus != db.StatusCompleted || processState != "completed" {
		t.Fatalf("final state = %q/%q, want completed/completed", jobStatus, processState)
	}

	var resultPath string
	if err := verification.QueryRow(
		ctx,
		`SELECT metadata->>'result_path'
		 FROM sm_outputs o
		 JOIN sm_jobs j ON j.process_id = o.process_id
		 WHERE j.id = $1`,
		jobID,
	).Scan(&resultPath); err != nil {
		t.Fatalf("query integration output: %v", err)
	}
	if resultPath != "/spool/out/step5/result.json" {
		t.Fatalf("output result path = %q", resultPath)
	}
}

func postJSON(
	ctx context.Context,
	client *http.Client,
	url string,
	body []byte,
) (*http.Response, error) {
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		url,
		bytes.NewReader(body),
	)
	if err != nil {
		return nil, fmt.Errorf("create HTTP request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("execute HTTP request: %w", err)
	}
	return response, nil
}
