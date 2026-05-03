//go:build integration

// Package mediaintegration exercises the media slice against a real
// Postgres instance booted in Docker via testcontainers-go. It lives
// in its own directory and package so:
//   - the domain folder (internal/media) stays focused on production
//     code plus fast unit tests,
//   - integration tests can only touch the public API of the media
//     package, mirroring how any other package (or bounded context)
//     would consume it.
//
// Every test in this package requires Docker; the //go:build
// integration tag keeps the default `go test ./...` fast.
//
// This file is duplicated from internal/tags/integration/helpers_test.go
// on purpose: with only two integration suites today, an extracted
// `internal/testenv` package would be premature abstraction. Promote
// on the third occurrence (rule of three).
package mediaintegration

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/tern/v2/migrate"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// newTestPool boots a fresh Postgres 16 container, applies every
// migration in db/migrations/, and returns a pgxpool.Pool connected
// to the new cluster.
//
// Both the pool and the container are torn down via t.Cleanup so
// tests cannot leak state into each other and cannot leak containers
// on panic. A fresh container per test keeps failures isolated; the
// ~1.5s cold-start is a cheap price for determinism at this scale.
func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	ctx := context.Background()

	container, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase("scoreplay"),
		tcpostgres.WithUsername("scoreplay"),
		tcpostgres.WithPassword("scoreplay"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(30*time.Second),
		),
	)
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}
	t.Cleanup(func() {
		termCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := container.Terminate(termCtx); err != nil {
			t.Logf("terminate postgres container: %v", err)
		}
	})

	connStr, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}

	applyMigrations(t, ctx, connStr)

	pool, err := pgxpool.New(ctx, connStr)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		t.Fatalf("ping pool: %v", err)
	}
	return pool
}

// applyMigrations runs every migration in db/migrations/ against the
// freshly booted container, using tern's Go API (same engine as the
// CLI). The migrations path is resolved relative to this source file
// so tests work from any CWD.
func applyMigrations(t *testing.T, ctx context.Context, connStr string) {
	t.Helper()

	conn, err := pgx.Connect(ctx, connStr)
	if err != nil {
		t.Fatalf("migrate: connect: %v", err)
	}
	defer conn.Close(ctx)

	migrator, err := migrate.NewMigrator(ctx, conn, "schema_version")
	if err != nil {
		t.Fatalf("migrate: new migrator: %v", err)
	}

	migrationsPath := migrationsDir(t)
	if err := migrator.LoadMigrations(os.DirFS(migrationsPath)); err != nil {
		t.Fatalf("migrate: load %q: %v", migrationsPath, err)
	}
	if err := migrator.Migrate(ctx); err != nil {
		t.Fatalf("migrate: apply: %v", err)
	}
}

// migrationsDir resolves db/migrations relative to this test file, so
// tests are CWD-independent. runtime.Caller(0) returns this file's
// path; we climb out of internal/media/integration/ to the repo root
// and down into db/migrations/.
func migrationsDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
	return filepath.Join(repoRoot, "db", "migrations")
}
