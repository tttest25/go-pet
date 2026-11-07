package config

import (
	"fmt"
	"os"
	"time"
)

const (
	defaultDBDSN        = "postgres://petar_user:petar_password@127.0.0.1:5432/petar_db?sslmode=disable"
	defaultSpoolDir     = "/opt/go-pet/spool"
	defaultHTTPAddr     = "127.0.0.1:8080"
	defaultPollInterval = 45 * time.Second
)

type Config struct {
	DBDSN        string
	SpoolDir     string
	HTTPAddr     string
	PollInterval time.Duration
}

func Load() (Config, error) {
	pollIntervalValue := envOrDefault("POLL_INTERVAL", defaultPollInterval.String())
	pollInterval, err := time.ParseDuration(pollIntervalValue)
	if err != nil {
		return Config{}, fmt.Errorf("parse POLL_INTERVAL %q: %w", pollIntervalValue, err)
	}
	if pollInterval <= 0 {
		return Config{}, fmt.Errorf("invalid POLL_INTERVAL %q: must be positive", pollIntervalValue)
	}

	return Config{
		DBDSN:        envOrDefault("DB_DSN", defaultDBDSN),
		SpoolDir:     envOrDefault("SPOOL_DIR", defaultSpoolDir),
		HTTPAddr:     envOrDefault("HTTP_ADDR", defaultHTTPAddr),
		PollInterval: pollInterval,
	}, nil
}

func envOrDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
