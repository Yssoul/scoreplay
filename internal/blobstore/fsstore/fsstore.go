// Package fsstore is a blobstore.Store backed by the local filesystem.
//
// It is meant for development and single-node deployments. Production
// deployments should use an S3-compatible backend (see DESIGN.md): the
// blobstore.Store interface lets handlers swap implementations without
// too many changes.
//
// Layout under the configured root:
//
//	<root>/
//	  blobs/   ← final, durable objects (one file per key)
//	  tmp/     ← in-flight uploads, renamed into blobs/ on success
package fsstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"

	"github.com/google/uuid"

	"github.com/ygalmessas/scoreplay/internal/blobstore"
)

const (
	blobsDir = "blobs"
	tmpDir   = "tmp"

	dirPerm  = 0o755
	filePerm = 0o644
)

// keyPattern restricts keys to characters we are willing to translate
// 1:1 into a path segment. UUIDs (with or without dashes) match this
// pattern; arbitrary user input does not. This is a defense-in-depth
// measure against path traversal even though callers are trusted to
// pass UUIDv7 strings.
var keyPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)

// Store persists blobs as files under root.
type Store struct {
	root string
}

// New returns a Store rooted at root. It creates root, root/blobs and
// root/tmp if they don't already exist so callers don't have to bootstrap
// the layout manually.
func New(root string) (*Store, error) {
	if root == "" {
		return nil, errors.New("fsstore: root path is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("fsstore: resolve root: %w", err)
	}
	for _, sub := range []string{blobsDir, tmpDir} {
		if err := os.MkdirAll(filepath.Join(abs, sub), dirPerm); err != nil {
			return nil, fmt.Errorf("fsstore: create %s: %w", sub, err)
		}
	}
	return &Store{root: abs}, nil
}

// Put writes r under key atomically: it streams into root/tmp/<random>
// then renames into root/blobs/<key>. A concurrent Open on the same key
// will either see the fully written file or ErrNotFound, never a
// partially written one.
func (s *Store) Put(ctx context.Context, key string, r io.Reader) error {
	if err := validateKey(key); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("fsstore: put %s: %w", key, err)
	}

	tmpPath := filepath.Join(s.root, tmpDir, uuid.NewString())
	// tmpPath = trusted store root + constant subdir + server-generated
	// UUIDv4. None of those segments come from user input, so the
	// G304 "potential file inclusion via variable" rule does not apply.
	//nolint:gosec // path is fully server-controlled (see comment above).
	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, filePerm)
	if err != nil {
		return fmt.Errorf("fsstore: create tmp: %w", err)
	}
	// Best-effort cleanup if anything below fails before the rename: a
	// failed Put must not leak a tmp file. After a successful rename the
	// path no longer exists, so Remove is a harmless no-op.
	defer func() { _ = os.Remove(tmpPath) }()

	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		return fmt.Errorf("fsstore: write tmp: %w", err)
	}
	// Sync before close so a kernel crash between Close() and Rename()
	// cannot leave a zero-byte file masquerading as a successful upload.
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("fsstore: sync tmp: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("fsstore: close tmp: %w", err)
	}

	finalPath := s.pathFor(key)
	if err := os.Rename(tmpPath, finalPath); err != nil {
		return fmt.Errorf("fsstore: publish %s: %w", key, err)
	}
	return nil
}

// Open returns the file stored under key. *os.File satisfies
// io.ReadSeekCloser, which lets http.ServeContent honor Range requests
// (essential for video streaming) without any extra plumbing.
func (s *Store) Open(_ context.Context, key string) (io.ReadSeekCloser, error) {
	if err := validateKey(key); err != nil {
		return nil, err
	}
	// validateKey above rejects traversal/absolute keys, and pathFor
	// always prepends the trusted store root, so G304 does not apply.
	//nolint:gosec // key is validated and joined under s.root.
	f, err := os.Open(s.pathFor(key))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("fsstore: open %s: %w", key, blobstore.ErrNotFound)
		}
		return nil, fmt.Errorf("fsstore: open %s: %w", key, err)
	}
	return f, nil
}

// Delete removes the file stored under key. A missing file is reported
// as ErrNotFound so callers can tell "already gone" from "deleted now".
func (s *Store) Delete(_ context.Context, key string) error {
	if err := validateKey(key); err != nil {
		return err
	}
	if err := os.Remove(s.pathFor(key)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("fsstore: delete %s: %w", key, blobstore.ErrNotFound)
		}
		return fmt.Errorf("fsstore: delete %s: %w", key, err)
	}
	return nil
}

// pathFor returns the final on-disk path for key. It assumes key has
// already been validated by validateKey.
func (s *Store) pathFor(key string) string {
	return filepath.Join(s.root, blobsDir, key)
}

// validateKey rejects keys that could escape the blobs directory or
// confuse the filesystem (path separators, traversal, empty input).
// Callers pass UUIDv7 strings, which always satisfy keyPattern.
func validateKey(key string) error {
	if !keyPattern.MatchString(key) {
		return fmt.Errorf("fsstore: invalid key %q", key)
	}
	return nil
}
