package api

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequestIDAndLoggingMiddleware(t *testing.T) {
	// Simple handler that responds with OK and checks context request ID
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqID := RequestIDFromContext(r.Context())
		if reqID == "" {
			t.Fatal("expected request id in context, got empty")
		}
		w.Header().Set("X-Test-ReqID", reqID)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("hello"))
	})

	wrapped := loggingMiddleware(slog.Default())(handler)

	t.Run("generates request id if missing", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.RemoteAddr = "192.168.1.50:12345"
		rec := httptest.NewRecorder()

		wrapped.ServeHTTP(rec, req)

		res := rec.Result()
		defer res.Body.Close()

		if res.StatusCode != http.StatusAccepted {
			t.Fatalf("expected status %d, got %d", http.StatusAccepted, res.StatusCode)
		}

		respReqID := res.Header.Get("X-Request-ID")
		if respReqID == "" {
			t.Fatal("expected X-Request-ID response header")
		}

		testReqID := res.Header.Get("X-Test-ReqID")
		if respReqID != testReqID {
			t.Fatalf("expected context request id %s to match response header %s", testReqID, respReqID)
		}
	})

	t.Run("preserves existing request id and extracts x-forwarded-for ip", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/inputs", nil)
		req.Header.Set("X-Request-ID", "custom-request-id-123")
		req.Header.Set("X-Forwarded-For", "203.0.113.195, 70.41.3.18")
		rec := httptest.NewRecorder()

		wrapped.ServeHTTP(rec, req)

		res := rec.Result()
		defer res.Body.Close()

		if got := res.Header.Get("X-Request-ID"); got != "custom-request-id-123" {
			t.Fatalf("expected X-Request-ID 'custom-request-id-123', got %q", got)
		}
	})

	t.Run("default status 200 when writeheader not called explicitly", func(t *testing.T) {
		okHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("ok"))
		})
		wrappedOK := loggingMiddleware(slog.Default())(okHandler)

		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		rec := httptest.NewRecorder()

		wrappedOK.ServeHTTP(rec, req)

		res := rec.Result()
		defer res.Body.Close()

		if res.StatusCode != http.StatusOK {
			t.Fatalf("expected status 200, got %d", res.StatusCode)
		}
	})
}

func TestExtractClientIP(t *testing.T) {
	tests := []struct {
		name       string
		headers    map[string]string
		remoteAddr string
		expected   string
	}{
		{
			name:       "x-forwarded-for single",
			headers:    map[string]string{"X-Forwarded-For": "1.2.3.4"},
			remoteAddr: "127.0.0.1:8080",
			expected:   "1.2.3.4",
		},
		{
			name:       "x-forwarded-for multiple",
			headers:    map[string]string{"X-Forwarded-For": "5.6.7.8, 10.0.0.1"},
			remoteAddr: "127.0.0.1:8080",
			expected:   "5.6.7.8",
		},
		{
			name:       "x-real-ip fallback",
			headers:    map[string]string{"X-Real-IP": "9.10.11.12"},
			remoteAddr: "127.0.0.1:8080",
			expected:   "9.10.11.12",
		},
		{
			name:       "remoteaddr fallback",
			headers:    map[string]string{},
			remoteAddr: "192.168.0.42:54321",
			expected:   "192.168.0.42",
		},
		{
			name:       "remoteaddr without port",
			headers:    map[string]string{},
			remoteAddr: "10.0.0.5",
			expected:   "10.0.0.5",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			for k, v := range tt.headers {
				req.Header.Set(k, v)
			}
			req.RemoteAddr = tt.remoteAddr

			got := extractClientIP(req)
			if got != tt.expected {
				t.Fatalf("expected %q, got %q", tt.expected, got)
			}
		})
	}
}
