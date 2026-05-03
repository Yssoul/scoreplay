package httpx

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
)

// Sentinel errors.
var (
	ErrBadRequest = errors.New("bad request")
	ErrNotFound   = errors.New("not found")
	ErrConflict   = errors.New("conflict")
	ErrInternal   = errors.New("internal server error")
)

// StatusClientClosedRequest is the non-standard 499 status code popularised
// by nginx to signal that the client closed the connection before the
// server could respond.
const StatusClientClosedRequest = 499

// WriteError translates err into an RFC 7807 Problem response.
//
// Status is decided by classify(err). For 4xx, err.Error() is forwarded as
// the detail (so callers control the client-facing message by what they put
// in the wrap chain). For 5xx, the wrapped chain is logged but only a
// generic detail reaches the client, so internals never leak.
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	if err == nil {
		return
	}

	//TODO: review this
	// Order matters: DeadlineExceeded must be checked before Canceled
	// because a deadline firing also cancels the context tree, so
	// errors.Is(err, context.Canceled) can be true for what is really a
	// server-side timeout. We want those reported as 504, not 499.
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		// fall through to classify() -> 504 Gateway Timeout.
	case errors.Is(err, context.Canceled):
		// Client went away. Nothing useful to send; writing a body
		// would just surface a broken pipe. The status is still set
		// so access-log middlewares can record it.
		w.WriteHeader(StatusClientClosedRequest)
		return
	}

	status, title := classify(err)

	detail := err.Error()
	if status >= http.StatusInternalServerError {
		detail = title
		LoggerFrom(r.Context()).ErrorContext(r.Context(), "request failed",
			slog.String("path", r.URL.Path),
			slog.String("method", r.Method),
			slog.Int("status", status),
			slog.Any("err", err),
		)
	}

	writeProblem(w, Problem{
		Title:  title,
		Status: status,
		Detail: detail,
	})
}

func classify(err error) (status int, title string) {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout, "request timed out"
	case errors.Is(err, ErrBadRequest):
		return http.StatusBadRequest, ErrBadRequest.Error()
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound, ErrNotFound.Error()
	case errors.Is(err, ErrConflict):
		return http.StatusConflict, ErrConflict.Error()
	default:
		return http.StatusInternalServerError, ErrInternal.Error()
	}
}
