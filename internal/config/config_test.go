package config_test

import (
	"testing"
	"time"

	"petar/internal/config"
)

func TestLoadDefaults(t *testing.T) {
	for _, key := range []string{
		"DB_DSN",
		"SPOOL_DIR",
		"HTTP_ADDR",
		"POLL_INTERVAL",
	} {
		t.Setenv(key, "")
	}

	got, err := config.Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	want := config.Config{
		DBDSN: "postgres://petar_user:petar_password@" +
			"127.0.0.1:5432/petar_db?sslmode=disable",
		SpoolDir:     "/opt/go-pet/spool",
		HTTPAddr:     "127.0.0.1:8080",
		PollInterval: 45 * time.Second,
	}

	if got != want {
		t.Fatalf("Load() = %+v, want %+v", got, want)
	}
}

func TestLoadEnvironmentOverrides(t *testing.T) {
	t.Setenv("DB_DSN", "postgres://example:test@db:5432/example")
	t.Setenv("SPOOL_DIR", "/tmp/test-spool")
	t.Setenv("HTTP_ADDR", ":9090")
	t.Setenv("POLL_INTERVAL", "2s")

	got, err := config.Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	want := config.Config{
		DBDSN:        "postgres://example:test@db:5432/example",
		SpoolDir:     "/tmp/test-spool",
		HTTPAddr:     ":9090",
		PollInterval: 2 * time.Second,
	}

	if got != want {
		t.Fatalf("Load() = %+v, want %+v", got, want)
	}
}

func TestLoadRejectsInvalidPollInterval(t *testing.T) {
	for _, value := range []string{
		"invalid",
		"0s",
		"-1s",
	} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("POLL_INTERVAL", value)

			if _, err := config.Load(); err == nil {
				t.Fatalf("Load() accepted POLL_INTERVAL=%q", value)
			}
		})
	}
}
