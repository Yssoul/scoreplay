// Command api is the ScorePlay HTTP service entrypoint: it wires the
// configuration, Postgres pool, blob store, and HTTP routes, then
// runs a graceful shutdown loop.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ygalmessas/scoreplay/internal/blobstore/fsstore"
	"github.com/ygalmessas/scoreplay/internal/config"
	"github.com/ygalmessas/scoreplay/internal/httpx"
	"github.com/ygalmessas/scoreplay/internal/media"
	"github.com/ygalmessas/scoreplay/internal/postgres"
	"github.com/ygalmessas/scoreplay/internal/tags"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	if err := run(logger); err != nil {
		logger.Error("api exited with error", slog.Any("err", err))
		os.Exit(1)
	}
}

// run holds the full startup/shutdown lifecycle.
func run(logger *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := postgres.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("init postgres: %w", err)
	}
	defer pool.Close()
	logger.Info("postgres pool ready")

	blobs, err := fsstore.New(cfg.BlobDir)
	if err != nil {
		return fmt.Errorf("init blob store: %w", err)
	}
	logger.Info("blob store ready", slog.String("dir", cfg.BlobDir))

	srv := newHTTPServer(cfg, logger, pool, blobs)
	return serve(ctx, logger, srv, cfg.ShutdownTimeout)
}

// newHTTPServer assembles the HTTP server: routes, handlers, middleware and
// timeouts.
//
// immutable by convention and copied exactly once at startup; trading
// the 96-byte copy for a *config.Config pointer would invite mutation
// and add a nil-pointer state to reason about.
//
//nolint:gocritic // hugeParam: cfg is passed by value on purpose. It is
func newHTTPServer(cfg config.Config, logger *slog.Logger, pool *pgxpool.Pool, blobs *fsstore.Store) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthHandler)

	tagHandler := tags.NewHandler(tags.NewPgRepository(pool))
	mux.HandleFunc("POST /tags", tagHandler.Create)
	mux.HandleFunc("GET /tags", tagHandler.List)

	mediaHandler := media.NewHandler(media.NewPgRepository(pool), blobs, cfg.MaxUploadBytes)
	mux.HandleFunc("POST /media", mediaHandler.Create)
	mux.HandleFunc("GET /media/{id}", mediaHandler.Get)
	mux.HandleFunc("GET /media/{id}/file", mediaHandler.ServeFile)

	return &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           httpx.LoggerMiddleware(logger)(mux),
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
	}
}

func healthHandler(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// serve runs srv until ctx is cancelled or ListenAndServe fails, then performs
// a bounded graceful shutdown. After Shutdown returns, the goroutine running
// ListenAndServe is drained so a late error cannot be silently swallowed.
func serve(ctx context.Context, logger *slog.Logger, srv *http.Server, shutdownTimeout time.Duration) error {
	serverErr := make(chan error, 1)
	go func() {
		defer close(serverErr)
		logger.Info("api listening", slog.String("addr", srv.Addr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- fmt.Errorf("listen: %w", err)
		}
	}()

	select {
	case err := <-serverErr:
		return err
	case <-ctx.Done():
		logger.Info("shutdown signal received, draining connections")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}

	// Drain a possible late error from ListenAndServe so it is reported
	// instead of being lost when the goroutine returns after Shutdown.
	if err, ok := <-serverErr; ok && err != nil {
		return err
	}

	logger.Info("server stopped cleanly")
	return nil
}
