package api

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

type contextKey string

const requestIDKey contextKey = "request_id"

type responseRecorder struct {
	http.ResponseWriter
	status int
	size   int64
}

func (rec *responseRecorder) WriteHeader(code int) {
	if rec.status != 0 {
		return
	}
	rec.status = code
	rec.ResponseWriter.WriteHeader(code)
}

func (rec *responseRecorder) Write(b []byte) (int, error) {
	if rec.status == 0 {
		rec.WriteHeader(http.StatusOK)
	}
	n, err := rec.ResponseWriter.Write(b)
	rec.size += int64(n)
	return n, err
}

func RequestIDFromContext(ctx context.Context) string {
	if id, ok := ctx.Value(requestIDKey).(string); ok {
		return id
	}
	return ""
}

func extractClientIP(r *http.Request) string {
	// 1. Check X-Forwarded-For
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		if ip := strings.TrimSpace(parts[0]); ip != "" {
			return ip
		}
	}

	// 2. Check X-Real-IP
	if xrip := r.Header.Get("X-Real-IP"); xrip != "" {
		if ip := strings.TrimSpace(xrip); ip != "" {
			return ip
		}
	}

	// 3. Fallback to RemoteAddr
	if r.RemoteAddr != "" {
		if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
			return host
		}
		return r.RemoteAddr
	}

	return ""
}

func loggingMiddleware(logger *slog.Logger) func(http.Handler) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()

			reqID := r.Header.Get("X-Request-ID")
			if reqID == "" {
				reqID = uuid.New().String()
			}

			w.Header().Set("X-Request-ID", reqID)

			ctx := context.WithValue(r.Context(), requestIDKey, reqID)
			r = r.WithContext(ctx)

			clientIP := extractClientIP(r)

			rec := &responseRecorder{
				ResponseWriter: w,
			}

			next.ServeHTTP(rec, r)

			duration := time.Since(start)
			status := rec.status
			if status == 0 {
				status = http.StatusOK
			}

			logger.Info("http request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", status,
				"latency_ms", duration.Milliseconds(),
				"request_id", reqID,
				"client_ip", clientIP,
				"response_size", rec.size,
			)
		})
	}
}
