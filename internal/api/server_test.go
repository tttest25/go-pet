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
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"petar/internal/db"
)

type fakeDatabase struct {
	getProcessResult                   db.ProcessDetails
	getProcessErr                      error
	listJobsResult                     []db.JobDetails
	listJobsErr                        error
	listActionsResult                  []db.ActionVersion
	listActionsErr                     error
	listActionsContext                 context.Context
	getProcessCalls, listJobsCalls     int
	getProcessID                       uuid.UUID
	listJobsFilter                     db.JobFilter
	getProcessContext, listJobsContext context.Context
	pingErr                            error
	createInputErr                     error
	updateErr                          error

	createdInput db.CreatedInput

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
	return &fakeDatabase{
		createdInput: db.CreatedInput{
			InputID:   uuid.New(),
			ProcessID: uuid.New(),
			Status:    "new",
		},
	}
}

func (f *fakeDatabase) GetProcess(ctx context.Context, id uuid.UUID) (db.ProcessDetails, error) {
	f.getProcessCalls++
	f.getProcessID, f.getProcessContext = id, ctx
	return f.getProcessResult, f.getProcessErr
}

func (f *fakeDatabase) ListJobs(ctx context.Context, filter db.JobFilter) ([]db.JobDetails, error) {
	f.listJobsCalls++
	f.listJobsFilter, f.listJobsContext = filter, ctx
	return f.listJobsResult, f.listJobsErr
}

func (f *fakeDatabase) Ping(context.Context) error {
	f.pingCalls++
	return f.pingErr
}

func (f *fakeDatabase) ListActions(ctx context.Context) ([]db.ActionVersion, error) {
	f.listActionsContext = ctx
	return f.listActionsResult, f.listActionsErr
}

func (f *fakeDatabase) CreateInput(
	_ context.Context,
	source string,
	payload json.RawMessage,
) (db.CreatedInput, error) {
	f.createInputCalls++
	f.source = source
	f.payload = payload
	if f.createInputErr != nil {
		return db.CreatedInput{}, f.createInputErr
	}
	return f.createdInput, nil
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

func TestListJobsHasDatabaseDeadline(t *testing.T) {
	f := newFakeDatabase()
	server := newServer(f, apiTestLogger())
	before := time.Now()
	response := performRequest(server, http.MethodGet, "/api/v1/jobs", "")
	after := time.Now()
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	deadline, ok := f.listJobsContext.Deadline()
	if !ok {
		t.Fatal("ListJobs context has no database deadline")
	}
	if deadline.Before(before.Add(5*time.Second)) || deadline.After(after.Add(5*time.Second)) {
		t.Fatalf("deadline = %v, want within call interval + 5s", deadline)
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
	if responseBody.InputID == uuid.Nil || responseBody.ProcessID == uuid.Nil {
		t.Fatalf("response contains nil UUID: %+v", responseBody)
	}
	if responseBody.InputID != database.createdInput.InputID {
		t.Fatalf(
			"response input ID = %s, want %s",
			responseBody.InputID,
			database.createdInput.InputID,
		)
	}
	if responseBody.ProcessID != database.createdInput.ProcessID {
		t.Fatalf(
			"response process ID = %s, want %s",
			responseBody.ProcessID,
			database.createdInput.ProcessID,
		)
	}
	if responseBody.Status != "new" {
		t.Fatalf("response status = %q, want new", responseBody.Status)
	}
	if responseBody.ID != responseBody.InputID {
		t.Fatalf("legacy response ID = %s, want input ID %s", responseBody.ID, responseBody.InputID)
	}

	var responseFields map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &responseFields); err != nil {
		t.Fatalf("decode create response fields: %v", err)
	}
	if len(responseFields) != 4 {
		t.Fatalf("response fields = %v, want exactly four fields", responseFields)
	}
	for _, field := range []string{"input_id", "process_id", "status", "id"} {
		if _, ok := responseFields[field]; !ok {
			t.Errorf("response field %q is missing", field)
		}
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
			name: "oversized body",
			body: `{"source":"manual","payload":{"tool":"echo-test","message":"` + strings.Repeat("x", 1<<20) + `"}}`,
		},
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
	tests := []struct {
		name string
		err  error
		want int
	}{
		{
			name: "insert failure",
			err:  errors.New("sensitive insert failure"),
			want: http.StatusInternalServerError,
		},
		{
			name: "created process is missing",
			err:  fmt.Errorf("sensitive process lookup failure: %w", pgx.ErrNoRows),
			want: http.StatusInternalServerError,
		},
		{
			name: "wrapped invalid input",
			err:  fmt.Errorf("sensitive validator details: %w", db.ErrInvalidInput),
			want: http.StatusBadRequest,
		},
		{
			name: "callback sentinel is not an input error",
			err:  fmt.Errorf("sensitive callback details: %w", db.ErrInvalidCallbackStatus),
			want: http.StatusInternalServerError,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			database := newFakeDatabase()
			database.createInputErr = test.err
			server := newServer(database, apiTestLogger())

			response := performRequest(
				server,
				http.MethodPost,
				"/api/v1/inputs",
				`{"source":"manual","payload":{"tool":"echo-test"}}`,
			)
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d", response.Code, test.want)
			}
			if strings.Contains(response.Body.String(), "sensitive") {
				t.Fatalf("response leaked internal error: %s", response.Body)
			}
			if database.createInputCalls != 1 {
				t.Fatalf("CreateInput calls = %d, want 1", database.createInputCalls)
			}
		})
	}
}

func TestCreateInputPayloadIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dsn := requireTestDSN(ctx, t)
	database, err := db.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	server := newServer(database, apiTestLogger())

	for _, tc := range []struct {
		name, payload string
		want          int
	}{
		{"default echo", `{"tool":"echo-test"}`, 201},
		{"web URL", `{"tool":"yt-dlp","url":"https://example.org/video"}`, 201},
		{"default pattern", `{"tool":"fabric","text":"body"}`, 201},
		{"missing tool", `{"message":"hello"}`, 400},
		{"unknown tool", `{"tool":"unsupported"}`, 400},
		{"padded tool", `{"tool":" echo-test "}`, 400},
		{"null message", `{"tool":"echo-test","message":null}`, 400},
		{"missing text", `{"tool":"fabric"}`, 400},
		{"invalid pattern", `{"tool":"fabric","text":"body","pattern":"../x"}`, 400},
		{"invalid URL", `{"tool":"yt-dlp","url":"https:///video"}`, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := "t17-http-" + uuid.NewString()
			t.Cleanup(func() {
				cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if _, err := pool.Exec(cleanup, `DELETE FROM sm_inputs WHERE source=$1`, source); err != nil {
					t.Error(err)
				}
			})
			response := performRequest(server, http.MethodPost, "/api/v1/inputs",
				fmt.Sprintf(`{"source":%q,"payload":%s}`, source, tc.payload))
			if response.Code != tc.want {
				t.Errorf("status = %d, want %d: %s", response.Code, tc.want, response.Body)
			}
			if tc.want == 201 {
				var created createInputResponse
				if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil ||
					created.InputID == uuid.Nil || created.ProcessID == uuid.Nil || created.ID != created.InputID || created.Status != "new" {
					t.Fatalf("invalid successful response: %s (%v)", response.Body, err)
				}
			} else {
				if response.Body.String() != "{\"error\":\"invalid payload\"}\n" {
					t.Errorf("unexpected rejection response: %s", response.Body)
				}
				var count int
				if err := pool.QueryRow(ctx, `SELECT count(*) FROM sm_inputs WHERE source=$1`, source).Scan(&count); err != nil || count != 0 {
					t.Errorf("input count after failure = %d, %v", count, err)
				}
			}
		})
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

// Comparing decoded JSON checks both values and the exact public key set.
func requireReadJSON(t *testing.T, response *httptest.ResponseRecorder, status int, want any) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status = %d, want %d: %s", response.Code, status, response.Body)
	}
	requireJSONContentType(t, response)
	expected, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var gotValue, wantValue any
	if err := json.Unmarshal(response.Body.Bytes(), &gotValue); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(expected, &wantValue); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("JSON = %s, want %s", response.Body, expected)
	}
}

func readFixture() (db.ProcessDetails, map[string]any, []map[string]any) {
	now := time.Date(2026, 11, 16, 12, 30, 0, 123456000, time.UTC)
	empty, path, failure := "", "/spool/out/result.json", "runner failed"
	p := db.ProcessDetails{ID: uuid.New(), InputID: uuid.New(), State: "running",
		Context: json.RawMessage(`{"nested":[1,true]}`), CreatedAt: now, UpdatedAt: now.Add(time.Second)}
	p.Jobs = []db.JobDetails{
		{ID: uuid.New(), ProcessID: p.ID, ToolName: "fabric", Payload: json.RawMessage(`{"n":2}`),
			Status: db.StatusFailed, Priority: 7, ResultPath: &empty, ErrorMsg: &failure, CreatedAt: now, UpdatedAt: now.Add(time.Second)},
		{ID: uuid.New(), ProcessID: p.ID, ToolName: "echo-test", Payload: json.RawMessage(`{"n":1}`),
			Status: db.StatusCompleted, Priority: -2, ResultPath: &path, CreatedAt: now.Add(time.Hour), UpdatedAt: now},
	}
	jobs := make([]map[string]any, len(p.Jobs))
	for i, j := range p.Jobs {
		jobs[i] = map[string]any{"id": j.ID, "process_id": j.ProcessID, "tool_name": j.ToolName,
			"payload": j.Payload, "status": j.Status, "priority": j.Priority, "result_path": j.ResultPath,
			"error_msg": j.ErrorMsg, "created_at": j.CreatedAt, "updated_at": j.UpdatedAt}
	}
	p.Outputs = []db.OutputDetails{
		{ID: uuid.New(), ProcessID: p.ID, ArtifactType: "text", Content: &empty, Metadata: json.RawMessage(`{"x":true}`), CreatedAt: now},
		{ID: uuid.New(), ProcessID: p.ID, ArtifactType: "json", Content: &path, Metadata: json.RawMessage(`{}`), CreatedAt: now},
		{ID: uuid.New(), ProcessID: p.ID, ArtifactType: "empty", Metadata: json.RawMessage(`{}`), CreatedAt: now},
	}
	outputs := make([]map[string]any, len(p.Outputs))
	for i, o := range p.Outputs {
		outputs[i] = map[string]any{"id": o.ID, "process_id": o.ProcessID, "artifact_type": o.ArtifactType,
			"content": o.Content, "metadata": o.Metadata, "created_at": o.CreatedAt}
	}
	want := map[string]any{"id": p.ID, "input_id": p.InputID, "state": p.State, "context": p.Context,
		"created_at": p.CreatedAt, "updated_at": p.UpdatedAt, "jobs": jobs, "outputs": outputs}
	return p, want, jobs
}

func requireReadOnly(t *testing.T, f *fakeDatabase) {
	t.Helper()
	if f.createInputCalls != 0 || f.updateCalls != 0 || f.pingCalls != 0 {
		t.Fatal("read endpoint called unrelated database method")
	}
}

func TestGetProcess(t *testing.T) {
	p, want, _ := readFixture()
	tests := []struct {
		name, id string
		err      error
		status   int
		message  string
	}{
		{"success", p.ID.String(), nil, 200, ""},
		{"invalid UUID", "bad", nil, 400, "invalid process ID"},
		{"nil UUID", uuid.Nil.String(), nil, 200, ""},
		{"not found", p.ID.String(), pgx.ErrNoRows, 404, "process not found"},
		{"wrapped not found", p.ID.String(), fmt.Errorf("sensitive: %w", pgx.ErrNoRows), 404, "process not found"},
		{"internal error", p.ID.String(), errors.New("sensitive secret"), 500, "failed to get process"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeDatabase()
			f.getProcessResult, f.getProcessErr = p, tt.err
			r := performRequest(newServer(f, apiTestLogger()), "GET", "/api/v1/processes/"+tt.id, "")
			if tt.message == "" {
				requireReadJSON(t, r, tt.status, want)
			} else {
				requireReadJSON(t, r, tt.status, map[string]string{"error": tt.message})
			}
			calls := 1
			if tt.status == 400 {
				calls = 0
			}
			if f.getProcessCalls != calls || f.listJobsCalls != 0 {
				t.Fatal("unexpected read calls")
			}
			if calls == 1 && f.getProcessID.String() != tt.id {
				t.Fatal("wrong process ID")
			}
			requireReadOnly(t, f)
		})
	}
	for _, empty := range []bool{false, true} {
		t.Run(fmt.Sprintf("empty slices %v", empty), func(t *testing.T) {
			f := newFakeDatabase()
			f.getProcessResult = p
			f.getProcessResult.Jobs, f.getProcessResult.Outputs = nil, nil
			if empty {
				f.getProcessResult.Jobs = []db.JobDetails{}
				f.getProcessResult.Outputs = []db.OutputDetails{}
			}
			expected := make(map[string]any)
			for k, v := range want {
				expected[k] = v
			}
			expected["jobs"], expected["outputs"] = []any{}, []any{}
			requireReadJSON(t, performRequest(newServer(f, apiTestLogger()), "GET", "/api/v1/processes/"+p.ID.String(), ""), 200, expected)
		})
	}
	t.Run("context", func(t *testing.T) { testReadContext(t, true) })
}

func TestListJobs(t *testing.T) {
	p, _, jobs := readFixture()
	id, cursor, status := p.ID, p.Jobs[0].ID, db.StatusPending
	tests := []struct {
		query  string
		filter db.JobFilter
	}{
		{"", db.JobFilter{Limit: 20}},
		{"unknown=ignored", db.JobFilter{Limit: 20}},
		{"process_id=" + id.String() + "&status=pending&limit=1&cursor=" + cursor.String(), db.JobFilter{ProcessID: &id, Status: &status, Limit: 1, CursorID: &cursor}},
		{"process_id=" + uuid.Nil.String() + "&cursor=" + uuid.Nil.String(), db.JobFilter{ProcessID: &uuid.Nil, CursorID: &uuid.Nil, Limit: 20}},
	}
	for _, s := range []db.Status{db.StatusPending, db.StatusDispatched, db.StatusInProgress, db.StatusCompleted, db.StatusFailed} {
		value := s
		tests = append(tests, struct {
			query  string
			filter db.JobFilter
		}{"status=" + string(s), db.JobFilter{Status: &value, Limit: 20}})
	}
	for _, limit := range []int{1, 20, 100, 101} {
		wantLimit := min(limit, 100)
		tests = append(tests, struct {
			query  string
			filter db.JobFilter
		}{fmt.Sprintf("limit=%d", limit), db.JobFilter{Limit: wantLimit}})
	}
	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			f := newFakeDatabase()
			f.listJobsResult = p.Jobs
			requireReadJSON(t, performRequest(newServer(f, apiTestLogger()), "GET", "/api/v1/jobs?"+tt.query, ""), 200, jobs)
			if f.listJobsCalls != 1 || f.getProcessCalls != 0 || !reflect.DeepEqual(f.listJobsFilter, tt.filter) {
				t.Fatalf("filter = %+v, want %+v", f.listJobsFilter, tt.filter)
			}
			requireReadOnly(t, f)
		})
	}
	invalid := map[string]string{
		"process_id=bad": "invalid process_id", "cursor=bad": "invalid cursor",
		"status=unknown": "invalid status", "status=Pending": "invalid status",
		"status=new": "invalid status", "status=processed": "invalid status", "status=error": "invalid status",
		"limit=0": "invalid limit", "limit=-1": "invalid limit", "limit=abc": "invalid limit",
		"limit=1.5": "invalid limit", "limit=%20": "invalid limit", "limit=9999999999999999999999999": "invalid limit",
		"bad=%zz": "invalid query parameters", "bad=a;b": "invalid query parameters",
		"process_id=bad&status=bad": "invalid process_id",
	}
	for _, name := range []string{"process_id", "status", "limit", "cursor"} {
		invalid[name+"="] = "invalid " + name
		invalid[name] = "invalid " + name
		value := map[string]string{"process_id": id.String(), "status": "pending", "limit": "1", "cursor": cursor.String()}[name]
		invalid[name+"="+value+"&"+name+"="+value] = "invalid " + name
	}
	for query, message := range invalid {
		t.Run(query, func(t *testing.T) {
			f := newFakeDatabase()
			request := httptest.NewRequest("GET", "/api/v1/jobs", nil)
			request.URL.RawQuery = query
			r := httptest.NewRecorder()
			newServer(f, apiTestLogger()).Handler().ServeHTTP(r, request)
			requireReadJSON(t, r, 400, map[string]string{"error": message})
			if f.listJobsCalls != 0 || f.getProcessCalls != 0 {
				t.Fatal("invalid query reached DB")
			}
			requireReadOnly(t, f)
		})
	}
	for _, result := range [][]db.JobDetails{nil, {}} {
		t.Run(fmt.Sprintf("empty %v", result == nil), func(t *testing.T) {
			f := newFakeDatabase()
			f.listJobsResult = result
			r := performRequest(newServer(f, apiTestLogger()), "GET", "/api/v1/jobs", "")
			requireReadJSON(t, r, 200, []any{})
			if strings.TrimSpace(r.Body.String()) != "[]" {
				t.Fatal("empty result is not []")
			}
		})
	}
	for _, err := range []error{errors.New("sensitive secret"), fmt.Errorf("sensitive: %w", pgx.ErrNoRows)} {
		t.Run(err.Error(), func(t *testing.T) {
			f := newFakeDatabase()
			f.listJobsResult, f.listJobsErr = p.Jobs, err
			requireReadJSON(t, performRequest(newServer(f, apiTestLogger()), "GET", "/api/v1/jobs", ""), 500, map[string]string{"error": "failed to list jobs"})
			requireReadOnly(t, f)
		})
	}
	t.Run("pagination passthrough", func(t *testing.T) {
		f := newFakeDatabase()
		f.listJobsResult = p.Jobs[:1]
		s := newServer(f, apiTestLogger())
		base := "/api/v1/jobs?process_id=" + id.String() + "&status=pending&limit=1"
		first := performRequest(s, "GET", base, "")
		requireReadJSON(t, first, 200, jobs[:1])
		var page []struct {
			ID uuid.UUID `json:"id"`
		}
		if err := json.Unmarshal(first.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		f.listJobsResult = p.Jobs[1:]
		requireReadJSON(t, performRequest(s, "GET", base+"&cursor="+page[0].ID.String(), ""), 200, jobs[1:])
		if f.listJobsCalls != 2 || !reflect.DeepEqual(f.listJobsFilter, db.JobFilter{ProcessID: &id, Status: &status, Limit: 1, CursorID: &cursor}) {
			t.Fatal("pagination filter lost")
		}
	})
	t.Run("context", func(t *testing.T) { testReadContext(t, false) })
}

func testReadContext(t *testing.T, process bool) {
	t.Helper()
	type key struct{}
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), key{}, "value"))
	cancel()
	f := newFakeDatabase()
	target := "/api/v1/jobs"
	if process {
		target = "/api/v1/processes/" + uuid.NewString()
	}
	r := httptest.NewRecorder()
	newServer(f, apiTestLogger()).Handler().ServeHTTP(r, httptest.NewRequest("GET", target, nil).WithContext(ctx))
	if r.Code != 200 {
		t.Fatalf("status = %d", r.Code)
	}
	got := f.listJobsContext
	if process {
		got = f.getProcessContext
	}
	if got == nil || got.Value(key{}) != "value" || !errors.Is(got.Err(), context.Canceled) {
		t.Fatal("request context not propagated")
	}
	requireReadOnly(t, f)
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
		{http.MethodPost, "/api/v1/processes/" + uuid.NewString(), http.StatusMethodNotAllowed},
		{http.MethodPost, "/api/v1/jobs", http.StatusMethodNotAllowed},
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

func TestListActionsReturnsEnabledVersions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	server, verification := newActionsIntegrationServer(ctx, t)
	action := "t23-" + uuid.NewString()
	for _, row := range readActions(t, server) {
		if row.Action == action {
			t.Fatalf("fixture action already exists: %q", action)
		}
	}

	var inserted int64
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		tag, err := verification.Exec(ctx,
			"DELETE FROM public.sm_actions WHERE action = $1 AND version IN (2, 3, 10)", action)
		if err != nil {
			t.Errorf("clean catalog fixture: %v", err)
		} else if tag.RowsAffected() != inserted {
			t.Errorf("deleted %d catalog rows, want %d", tag.RowsAffected(), inserted)
		}
	})
	tag, err := verification.Exec(ctx, `INSERT INTO public.sm_actions (action, version, title, enabled)
		VALUES ($1, 10, 'Version ten', true), ($1, 2, 'Version two', true), ($1, 3, 'Disabled', false)`, action)
	if err != nil {
		t.Fatalf("insert catalog fixture: %v", err)
	}
	inserted = tag.RowsAffected()
	if inserted != 3 {
		t.Fatalf("inserted %d catalog rows, want 3", inserted)
	}

	var actual []actionContract
	for _, row := range readActions(t, server) {
		if row.Action == action {
			actual = append(actual, row)
		}
	}
	want := []actionContract{{action, 2, "Version two"}, {action, 10, "Version ten"}}
	if !reflect.DeepEqual(actual, want) {
		t.Fatalf("fixture versions = %+v, want %+v", actual, want)
	}
}

func TestListActionsEmpty(t *testing.T) {
	server := newServer(newFakeDatabase(), apiTestLogger())
	requireReadJSON(t, performRequest(server, http.MethodGet, "/api/v1/actions", ""),
		http.StatusOK, []actionContract{})
}

func TestListActionsHasDatabaseDeadline(t *testing.T) {
	f := newFakeDatabase()
	server := newServer(f, apiTestLogger())
	before := time.Now()
	response := performRequest(server, http.MethodGet, "/api/v1/actions", "")
	after := time.Now()
	if response.Code != http.StatusOK || f.listActionsContext == nil {
		t.Fatalf("status = %d, ListActions context = %v", response.Code, f.listActionsContext)
	}
	deadline, ok := f.listActionsContext.Deadline()
	if !ok || deadline.Before(before.Add(5*time.Second)) || deadline.After(after.Add(5*time.Second)) {
		t.Fatalf("deadline = %v, want within call interval + 5s", deadline)
	}
}

func TestListActionsDatabaseErrors(t *testing.T) {
	const path = "/api/v1/actions"
	const message = "failed to list actions"
	t.Run("internal error", func(t *testing.T) {
		f := newFakeDatabase()
		f.listActionsErr = errors.New("private catalog database details")
		server := newServer(f, apiTestLogger())
		requireReadJSON(t, performRequest(server, http.MethodGet, path, ""),
			http.StatusInternalServerError, map[string]string{"error": message})
	})
	t.Run("database timeout", func(t *testing.T) {
		parent, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		f := &waitingDatabase{wait: func(ctx context.Context, _ string) error {
			<-ctx.Done()
			return fmt.Errorf("wait for catalog: %w", ctx.Err())
		}}
		server := newServer(f, apiTestLogger())
		server.databaseTimeout = 20 * time.Millisecond
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil).WithContext(parent))
		requireReadJSON(t, response, http.StatusGatewayTimeout, map[string]string{"error": "database request timed out"})
	})
	t.Run("parent cancellation", func(t *testing.T) {
		parent, cancel := context.WithCancel(context.Background())
		defer cancel()
		f := &waitingDatabase{wait: func(ctx context.Context, _ string) error {
			cancel()
			<-ctx.Done()
			if !errors.Is(ctx.Err(), context.Canceled) {
				t.Errorf("database context error = %v, want Canceled", ctx.Err())
			}
			return fmt.Errorf("wait for catalog: %w", ctx.Err())
		}}
		server := newServer(f, apiTestLogger())
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil).WithContext(parent))
		requireReadJSON(t, response, http.StatusInternalServerError, map[string]string{"error": message})
	})
}

func TestListActionsDisablePreservesJob(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	server, verification := newActionsIntegrationServer(ctx, t)

	source := "t23-" + uuid.NewString()
	var createdInput bool
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		tag, err := verification.Exec(ctx, "DELETE FROM public.sm_inputs WHERE source = $1", source)
		if err != nil {
			t.Errorf("clean catalog test input: %v", err)
		} else if createdInput && tag.RowsAffected() != 1 {
			t.Errorf("deleted %d inputs, want 1", tag.RowsAffected())
		}
	})
	body := fmt.Sprintf(`{"source":%q,"payload":{"tool":"echo-test"}}`, source)
	response := performRequest(server, http.MethodPost, "/api/v1/inputs", body)
	if response.Code != http.StatusCreated {
		t.Fatalf("POST input status = %d, want 201; body: %s", response.Code, response.Body)
	}
	createdInput = true
	var created struct {
		ProcessID uuid.UUID `json:"process_id"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil || created.ProcessID == uuid.Nil {
		t.Fatalf("invalid input response: %s (decode error: %v)", response.Body, err)
	}
	jobsPath := "/api/v1/jobs?process_id=" + created.ProcessID.String()
	readJob := func() map[string]json.RawMessage {
		t.Helper()
		response := performRequest(server, http.MethodGet, jobsPath, "")
		if response.Code != http.StatusOK {
			t.Fatalf("GET jobs status = %d, want 200; body: %s", response.Code, response.Body)
		}
		requireJSONContentType(t, response)
		var jobs []map[string]json.RawMessage
		if err := json.Unmarshal(response.Body.Bytes(), &jobs); err != nil || len(jobs) != 1 {
			t.Fatalf("expected one job: %s (decode error: %v)", response.Body, err)
		}
		var id uuid.UUID
		if err := json.Unmarshal(jobs[0]["id"], &id); err != nil || id == uuid.Nil {
			t.Fatalf("invalid job ID: %s (decode error: %v)", jobs[0]["id"], err)
		}
		return jobs[0]
	}
	before := readJob()
	hasEchoV1 := func() bool {
		for _, row := range readActions(t, server) {
			if row.Action == "echo-test" && row.Version == 1 {
				return true
			}
		}
		return false
	}
	if !hasEchoV1() {
		t.Fatal("echo-test v1 missing before disabling")
	}

	var originalEnabled bool
	err := verification.QueryRow(ctx, `SELECT enabled FROM public.sm_actions
		WHERE action = 'echo-test' AND version = 1`).Scan(&originalEnabled)
	if err != nil {
		t.Fatalf("read original seed availability: %v", err)
	}
	const setEnabled = `UPDATE public.sm_actions SET enabled = $1
		WHERE action = 'echo-test' AND version = 1`
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		tag, err := verification.Exec(ctx, setEnabled, originalEnabled)
		if err != nil {
			t.Errorf("restore seed availability: %v", err)
		} else if tag.RowsAffected() != 1 {
			t.Errorf("restored %d seed rows, want 1", tag.RowsAffected())
		}
	})
	tag, err := verification.Exec(ctx, setEnabled, false)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("disable echo-test v1: updated %d rows (error: %v)", tag.RowsAffected(), err)
	}
	if hasEchoV1() {
		t.Fatal("disabled echo-test v1 still in catalog")
	}
	if after := readJob(); !reflect.DeepEqual(after, before) {
		t.Fatalf("job changed after disabling action: before %v, after %v", before, after)
	}
}

func newActionsIntegrationServer(ctx context.Context, t *testing.T) (*Server, *pgxpool.Pool) {
	t.Helper()
	dsn := requireTestDSN(ctx, t)
	database, err := db.New(ctx, dsn)
	if err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(database.Close)
	verification, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("create verification pool: %v", err)
	}
	t.Cleanup(verification.Close)
	return NewServer(database, apiTestLogger()), verification
}

type actionContract struct {
	Action  string `json:"action"`
	Version int    `json:"version"`
	Title   string `json:"title"`
}

func readActions(t *testing.T, server *Server) []actionContract {
	t.Helper()
	response := performRequest(server, http.MethodGet, "/api/v1/actions", "")
	if response.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/actions status = %d, want 200; body: %s", response.Code, response.Body)
	}
	requireJSONContentType(t, response)
	var objects []map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &objects); err != nil || objects == nil {
		t.Fatalf("expected catalog JSON array: %s (decode error: %v)", response.Body, err)
	}
	rows := make([]actionContract, len(objects))
	for i, obj := range objects {
		if len(obj) != 3 || obj["action"] == nil || obj["version"] == nil || obj["title"] == nil {
			t.Fatalf("unexpected action keys: %v", obj)
		}
		for key, dest := range map[string]any{"action": &rows[i].Action, "version": &rows[i].Version, "title": &rows[i].Title} {
			if err := json.Unmarshal(obj[key], dest); err != nil || string(obj[key]) == "null" {
				t.Fatalf("invalid action field %s: %s (decode error: %v)", key, obj[key], err)
			}
		}
		if i > 0 {
			prev, row := rows[i-1], rows[i]
			if prev.Action > row.Action || (prev.Action == row.Action && prev.Version >= row.Version) {
				t.Fatalf("catalog is not ordered by (action, version): %+v before %+v", prev, row)
			}
		}
	}
	return rows
}

func TestServerIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dsn := requireTestDSN(ctx, t)
	database, err := db.New(ctx, dsn)
	if err != nil {
		t.Fatalf("create database: %v", err)
	}
	verification, err := pgxpool.New(ctx, dsn)
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
	if created.InputID == uuid.Nil || created.ProcessID == uuid.Nil {
		t.Fatalf("integration response contains nil UUID: %+v", created)
	}
	if created.Status != "new" {
		t.Fatalf("integration input status = %q, want new", created.Status)
	}
	if created.ID != created.InputID {
		t.Fatalf(
			"integration legacy ID = %s, want input ID %s",
			created.ID,
			created.InputID,
		)
	}

	var jobID uuid.UUID
	var processID uuid.UUID
	if err := verification.QueryRow(
		ctx,
		`SELECT j.id, p.id
		 FROM sm_jobs j
		 JOIN sm_processes p ON p.id = j.process_id
		 WHERE p.input_id = $1`,
		created.InputID,
	).Scan(&jobID, &processID); err != nil {
		t.Fatalf("query integration job: %v", err)
	}
	if processID != created.ProcessID {
		t.Fatalf("database process ID = %s, want %s", processID, created.ProcessID)
	}

	// Keep the HTTP contract independent of both production DTOs and DB models,
	// which deliberately have no JSON tags.
	type integrationJob struct {
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
	type integrationOutput struct {
		ID           uuid.UUID       `json:"id"`
		ProcessID    uuid.UUID       `json:"process_id"`
		ArtifactType string          `json:"artifact_type"`
		Content      *string         `json:"content"`
		Metadata     json.RawMessage `json:"metadata"`
		CreatedAt    time.Time       `json:"created_at"`
	}
	type integrationProcess struct {
		ID      uuid.UUID           `json:"id"`
		InputID uuid.UUID           `json:"input_id"`
		State   string              `json:"state"`
		Jobs    []integrationJob    `json:"jobs"`
		Outputs []integrationOutput `json:"outputs"`
	}
	readGET := func(ctx context.Context, target string, status int, destination any) {
		t.Helper()
		response, err := getJSON(ctx, httpServer.Client(), httpServer.URL+target)
		if err != nil {
			t.Fatalf("get integration %s: %v", target, err)
		}
		defer response.Body.Close()
		if response.Header.Get("Content-Type") != "application/json; charset=utf-8" {
			t.Fatalf("GET %s content type = %q", target, response.Header.Get("Content-Type"))
		}
		if response.StatusCode != status {
			body, _ := io.ReadAll(response.Body)
			t.Fatalf("GET %s status = %d, want %d: %s", target, response.StatusCode, status, body)
		}
		if err := json.NewDecoder(response.Body).Decode(destination); err != nil {
			t.Fatalf("decode integration GET %s: %v", target, err)
		}
	}
	processURL := "/api/v1/processes/" + processID.String()
	jobsURL := "/api/v1/jobs?process_id=" + processID.String()
	var pendingProcess integrationProcess
	readGET(ctx, processURL, http.StatusOK, &pendingProcess)
	if pendingProcess.ID != processID || pendingProcess.InputID != created.InputID ||
		pendingProcess.State != "init" || len(pendingProcess.Jobs) != 1 ||
		pendingProcess.Outputs == nil || len(pendingProcess.Outputs) != 0 {
		t.Fatalf("pending process = %+v, want own init process with one job and outputs=[]", pendingProcess)
	}
	pendingJob := pendingProcess.Jobs[0]
	var pendingPayload map[string]any
	if err := json.Unmarshal(pendingJob.Payload, &pendingPayload); err != nil {
		t.Fatalf("decode pending job payload: %v", err)
	}
	if pendingJob.ID != jobID || pendingJob.ProcessID != processID ||
		pendingJob.ToolName != "echo-test" || pendingJob.Status != db.StatusPending ||
		pendingJob.Priority != 0 || pendingJob.ResultPath != nil || pendingJob.ErrorMsg != nil ||
		pendingJob.CreatedAt.IsZero() || pendingJob.UpdatedAt.IsZero() ||
		!reflect.DeepEqual(pendingPayload, map[string]any{"tool": "echo-test", "message": "integration"}) {
		t.Fatalf("pending job = %+v, payload = %s", pendingJob, pendingJob.Payload)
	}
	var pendingJobs []integrationJob
	readGET(ctx, jobsURL+"&status=pending&limit=1", http.StatusOK, &pendingJobs)
	if len(pendingJobs) != 1 || !reflect.DeepEqual(pendingJobs[0], pendingJob) {
		t.Fatalf("pending jobs = %+v, want same process job %+v", pendingJobs, pendingJob)
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

	var completedProcess integrationProcess
	readGET(ctx, processURL, http.StatusOK, &completedProcess)
	if completedProcess.ID != processID || completedProcess.InputID != created.InputID ||
		completedProcess.State != "completed" || len(completedProcess.Jobs) != 1 ||
		len(completedProcess.Outputs) != 1 {
		t.Fatalf("completed process = %+v", completedProcess)
	}
	completedJob := completedProcess.Jobs[0]
	if completedJob.ID != jobID || completedJob.ProcessID != processID ||
		completedJob.Status != db.StatusCompleted || completedJob.ResultPath == nil ||
		*completedJob.ResultPath != resultPath || completedJob.ErrorMsg != nil {
		t.Fatalf("completed job = %+v", completedJob)
	}
	var storedOutput integrationOutput
	if err := verification.QueryRow(
		ctx,
		`SELECT id, process_id, artifact_type, content, metadata, created_at
		 FROM sm_outputs WHERE process_id = $1`,
		processID,
	).Scan(
		&storedOutput.ID,
		&storedOutput.ProcessID,
		&storedOutput.ArtifactType,
		&storedOutput.Content,
		&storedOutput.Metadata,
		&storedOutput.CreatedAt,
	); err != nil {
		t.Fatalf("query integration artifact details: %v", err)
	}
	output := completedProcess.Outputs[0]
	var outputMetadata, storedMetadata map[string]any
	if err := json.Unmarshal(output.Metadata, &outputMetadata); err != nil {
		t.Fatalf("decode HTTP artifact metadata: %v", err)
	}
	if err := json.Unmarshal(storedOutput.Metadata, &storedMetadata); err != nil {
		t.Fatalf("decode stored artifact metadata: %v", err)
	}
	if output.ID != storedOutput.ID || output.ProcessID != processID ||
		output.ProcessID != storedOutput.ProcessID || output.ArtifactType != storedOutput.ArtifactType ||
		!reflect.DeepEqual(output.Content, storedOutput.Content) ||
		!output.CreatedAt.Equal(storedOutput.CreatedAt) ||
		!reflect.DeepEqual(outputMetadata, storedMetadata) || outputMetadata["result_path"] != resultPath {
		t.Fatalf("HTTP artifact = %+v, want stored artifact %+v", output, storedOutput)
	}
	var completedJobs []integrationJob
	readGET(ctx, jobsURL+"&status=completed&limit=1", http.StatusOK, &completedJobs)
	if len(completedJobs) != 1 || !reflect.DeepEqual(completedJobs[0], completedJob) {
		t.Fatalf("completed jobs = %+v, want %+v", completedJobs, completedJob)
	}
	for _, target := range []string{
		jobsURL + "&status=completed&limit=1&cursor=" + jobID.String(),
		jobsURL + "&status=pending&limit=1",
		"/api/v1/jobs?process_id=" + uuid.NewString(),
	} {
		var jobs []integrationJob
		readGET(ctx, target, http.StatusOK, &jobs)
		if jobs == nil || len(jobs) != 0 {
			t.Fatalf("GET %s jobs = %+v, want []", target, jobs)
		}
	}
	var missingProcess map[string]string
	readGET(ctx, "/api/v1/processes/"+uuid.NewString(), http.StatusNotFound, &missingProcess)
	if !reflect.DeepEqual(missingProcess, map[string]string{"error": "process not found"}) {
		t.Fatalf("missing process response = %+v", missingProcess)
	}
}

func getJSON(ctx context.Context, client *http.Client, url string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create HTTP GET request: %w", err)
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("execute HTTP GET request: %w", err)
	}
	return response, nil
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
