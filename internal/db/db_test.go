package db_test

import (
	"context"
	"testing"
	"time"

	"petar/internal/config"
	"petar/internal/db"
)

func TestPing(t *testing.T) {
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	ctx, cancel := context.WithTimeout(
		context.Background(),
		5*time.Second,
	)
	defer cancel()

	database, err := db.New(ctx, cfg.DBDSN)
	if err != nil {
		t.Fatalf("create database pool: %v", err)
	}
	t.Cleanup(database.Close)

	if err := database.Ping(ctx); err != nil {
		t.Fatalf("ping petar_postgres: %v", err)
	}
}
