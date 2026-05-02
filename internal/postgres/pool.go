// Package postgres constructs a pgxpool.Pool wired for the rest of the app.
//
// pgxpool is Postgres-aware: better defaults than database/sql, per-conn
// prepared-statement cache, native support for pgx types. We tune a handful
// of pool knobs here; everything else stays on pgx defaults.
package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// NewPool parses the connection string, applies opinionated pool defaults,
// opens the pool, and pings it to fail fast on misconfiguration. The caller
// owns the returned *pgxpool.Pool and must call Close on shutdown.
func NewPool(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse config: %w", err)
	}

	cfg.MaxConns = 10
	cfg.MinConns = 1
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.HealthCheckPeriod = 30 * time.Second

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("postgres: open pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: ping: %w", err)
	}
	return pool, nil
}
