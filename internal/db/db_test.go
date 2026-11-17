package db_test

import (
	"context"
	"testing"
	"time"

	"petar/internal/db"
)

func TestPing(t *testing.T) {
	ctx, cancel := context.WithTimeout(
		context.Background(),
		5*time.Second,
	)
	defer cancel()
	dsn := requireTestDSN(ctx, t)

	database, err := db.New(ctx, dsn)
	if err != nil {
		t.Fatalf("create database pool: %v", err)
	}
	t.Cleanup(database.Close)

	if err := database.Ping(ctx); err != nil {
		t.Fatalf("ping petar_postgres: %v", err)
	}
}
