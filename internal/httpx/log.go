package httpx

import (
	"context"
	"log/slog"
	"net/http"
)

type loggerCtxKey struct{}

// WithLogger returns a new context carrying l. Use it in middleware to
// attach a per-request logger (already enriched with request_id, trace_id,
// etc.) so downstream code can pull it via the request context.
func WithLogger(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerCtxKey{}, l)
}

// LoggerMiddleware attaches l to every incoming request's context.
func LoggerMiddleware(l *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := WithLogger(r.Context(), l)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// loggerFrom returns the logger attached to ctx, or slog.Default() if none
// is present. The fallback keeps tests and ad-hoc code working without a
// middleware setup.
func loggerFrom(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(loggerCtxKey{}).(*slog.Logger); ok && l != nil {
		return l
	}
	return slog.Default()
}
