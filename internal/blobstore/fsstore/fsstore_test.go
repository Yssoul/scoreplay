package fsstore_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/ygalmessas/scoreplay/internal/blobstore"
	"github.com/ygalmessas/scoreplay/internal/blobstore/fsstore"
)

// Compile-time proof that *Store satisfies the blobstore.Store contract.
var _ blobstore.Store = (*fsstore.Store)(nil)

// newStore returns a Store rooted in t.TempDir(), so each test runs
// against an isolated filesystem that is removed automatically.
func newStore(t *testing.T) *fsstore.Store {
	t.Helper()
	s, err := fsstore.New(t.TempDir())
	if err != nil {
		t.Fatalf("fsstore.New: %v", err)
	}
	return s
}

// newKey returns a fresh UUIDv7 string suitable as a blob key.
func newKey(t *testing.T) string {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("uuid.NewV7: %v", err)
	}
	return id.String()
}

func TestNew_RejectsEmptyRoot(t *testing.T) {
	if _, err := fsstore.New(""); err == nil {
		t.Fatal("New(\"\"): want error, got nil")
	}
}

func TestPutThenOpen_ReturnsExactBytes(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	key := newKey(t)
	want := []byte("hello scoreplay")

	if err := s.Put(ctx, key, bytes.NewReader(want)); err != nil {
		t.Fatalf("Put: %v", err)
	}

	rc, err := s.Open(ctx, key)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("content: got %q, want %q", got, want)
	}
}

func TestOpen_UnknownKey_ReturnsErrNotFound(t *testing.T) {
	s := newStore(t)
	_, err := s.Open(context.Background(), newKey(t))
	if !errors.Is(err, blobstore.ErrNotFound) {
		t.Fatalf("err: got %v, want wraps blobstore.ErrNotFound", err)
	}
}

func TestDelete_ThenOpen_ReturnsErrNotFound(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	key := newKey(t)

	if err := s.Put(ctx, key, strings.NewReader("data")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Open(ctx, key); !errors.Is(err, blobstore.ErrNotFound) {
		t.Errorf("Open after Delete: got %v, want wraps ErrNotFound", err)
	}
}

func TestDelete_UnknownKey_ReturnsErrNotFound(t *testing.T) {
	s := newStore(t)
	err := s.Delete(context.Background(), newKey(t))
	if !errors.Is(err, blobstore.ErrNotFound) {
		t.Fatalf("err: got %v, want wraps blobstore.ErrNotFound", err)
	}
}

// invalid keys must be rejected before any filesystem operation, so a
// caller cannot escape the blobs/ directory via "../" or absolute paths.
func TestKeyValidation_RejectsTraversalAndSeparators(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	bad := []string{
		"",
		"../etc/passwd",
		"a/b",
		"a\\b",
		"with space",
		strings.Repeat("a", 200),
	}
	for _, key := range bad {
		t.Run(key, func(t *testing.T) {
			if err := s.Put(ctx, key, strings.NewReader("x")); err == nil {
				t.Errorf("Put(%q): want error, got nil", key)
			}
			if _, err := s.Open(ctx, key); err == nil {
				t.Errorf("Open(%q): want error, got nil", key)
			}
			if err := s.Delete(ctx, key); err == nil {
				t.Errorf("Delete(%q): want error, got nil", key)
			}
		})
	}
}

// TestPut_Concurrent_NoCorruption verifies that two goroutines writing
// the same key concurrently never produce a truncated or interleaved
// file: the final content must equal one of the two payloads exactly.
// This locks the temp+rename atomicity contract from Put's godoc.
func TestPut_Concurrent_NoCorruption(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	key := newKey(t)

	a := bytes.Repeat([]byte("A"), 64*1024)
	b := bytes.Repeat([]byte("B"), 64*1024)

	var wg sync.WaitGroup
	wg.Add(2)
	for _, payload := range [][]byte{a, b} {
		go func(p []byte) {
			defer wg.Done()
			if err := s.Put(ctx, key, bytes.NewReader(p)); err != nil {
				t.Errorf("Put: %v", err)
			}
		}(payload)
	}
	wg.Wait()

	rc, err := s.Open(ctx, key)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, a) && !bytes.Equal(got, b) {
		t.Errorf("content corrupted: not equal to either writer payload (len=%d)", len(got))
	}
}

func TestPut_CanceledContext_ReturnsError(t *testing.T) {
	s := newStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := s.Put(ctx, newKey(t), strings.NewReader("x"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err: got %v, want wraps context.Canceled", err)
	}
}

// TestPut_LeavesNoTempFile guarantees that a successful Put cleans up
// the staging file. A leak would slowly fill the disk in production.
func TestPut_LeavesNoTempFile(t *testing.T) {
	root := t.TempDir()
	s, err := fsstore.New(root)
	if err != nil {
		t.Fatalf("fsstore.New: %v", err)
	}
	if err := s.Put(context.Background(), newKey(t), strings.NewReader("x")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "tmp"))
	if err != nil {
		t.Fatalf("read tmp: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("tmp dir not empty after Put: %d entries", len(entries))
	}
}
