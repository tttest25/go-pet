package db

import (
	"context"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
)

type DB struct {
	pool     *pgxpool.Pool
	listener *pgxpool.Conn

	listenerMu sync.Mutex
}

func New(ctx context.Context, dsn string) (*DB, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("create database pool: %w", err)
	}

	listener, err := pool.Acquire(ctx)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("acquire listener connection: %w", err)
	}

	if _, err := listener.Exec(ctx, listenJobsQuery); err != nil {
		listener.Release()
		pool.Close()
		return nil, fmt.Errorf("listen sm_jobs_channel: %w", err)
	}

	return &DB{
		pool:     pool,
		listener: listener,
	}, nil
}

func (d *DB) Ping(ctx context.Context) error {
	if err := d.pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}
	return nil
}

func (d *DB) Close() {
	d.listenerMu.Lock()
	defer d.listenerMu.Unlock()

	if d.listener != nil {
		d.listener.Release()
		d.listener = nil
	}
	d.pool.Close()
}
