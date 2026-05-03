// Package blobstore defines a small, transport-agnostic abstraction for
// reading and writing binary objects (images, videos…) by key.
//
// The interface is deliberately narrow so handlers and use-cases can stay
// unaware of the actual storage backend.
package blobstore

import (
	"context"
	"errors"
	"io"
)

// ErrNotFound is returned by Open and Delete when the requested key does
// not exist.
var ErrNotFound = errors.New("blob not found")

// Store reads and writes binary objects by key. Implementations must be
// safe for concurrent use by multiple goroutines.
type Store interface {
	// Put stores the entire content of r under key. It must be atomic
	// from a reader's point of view: a concurrent Open on the same key
	// either returns ErrNotFound or the fully written object, never a
	// truncated one.
	Put(ctx context.Context, key string, r io.Reader) error

	// Open returns a reader on the object stored under key. The returned
	// ReadSeekCloser is required so callers can use http.ServeContent,
	// which relies on Seek to honor Range requests. Callers must Close.
	// If the key does not exist, Open returns ErrNotFound.
	Open(ctx context.Context, key string) (io.ReadSeekCloser, error)

	// Delete removes the object stored under key. Deleting an unknown
	// key returns ErrNotFound.
	Delete(ctx context.Context, key string) error
}
