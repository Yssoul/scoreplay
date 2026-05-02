//go:build integration

package tagsintegration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ygalmessas/scoreplay/internal/tags"
)

// newTag builds a domain Tag with a freshly generated UUIDv7. Centralised
// so tests do not repeat the boilerplate and so test failures point at the
// behaviour under test, not at id generation.
func newTag(t *testing.T, name string) tags.Tag {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("uuid.NewV7: %v", err)
	}
	return tags.Tag{ID: id, Name: name}
}

func TestPgRepository_CreateTag_Success(t *testing.T) {
	pool := newTestPool(t)
	repo := tags.NewTagRepository(pool)
	ctx := context.Background()

	before := time.Now().UTC()
	tag := newTag(t, "Messi")
	if err := repo.CreateTag(ctx, tag); err != nil {
		t.Fatalf("CreateTag: %v", err)
	}
	after := time.Now().UTC()

	// Verify the row was persisted with the exact id, case-preserved name,
	// and a created_at stamped by the repository within the test window.
	var (
		gotID        uuid.UUID
		gotName      string
		gotCreatedAt time.Time
	)
	err := pool.QueryRow(ctx,
		`SELECT id, name, created_at FROM tags WHERE id = $1`, tag.ID,
	).Scan(&gotID, &gotName, &gotCreatedAt)
	if err != nil {
		t.Fatalf("select inserted row: %v", err)
	}

	if gotID != tag.ID {
		t.Errorf("id: got %s, want %s", gotID, tag.ID)
	}
	if gotName != "Messi" {
		t.Errorf("name: got %q, want %q (case must be preserved)", gotName, "Messi")
	}
	if gotCreatedAt.Before(before) || gotCreatedAt.After(after) {
		t.Errorf("created_at %s not within [%s, %s]", gotCreatedAt, before, after)
	}
}

func TestPgRepository_CreateTag_Conflict(t *testing.T) {
	pool := newTestPool(t)
	repo := tags.NewTagRepository(pool)
	ctx := context.Background()

	first := newTag(t, "Messi")
	if err := repo.CreateTag(ctx, first); err != nil {
		t.Fatalf("CreateTag first: %v", err)
	}

	// Same name in a different case: CITEXT on tags.name must collide on
	// the UNIQUE constraint, and the repository must translate pgx's
	// SQLSTATE 23505 into tags.ErrNameConflict.
	second := newTag(t, "messi")
	err := repo.CreateTag(ctx, second)
	if err == nil {
		t.Fatalf("CreateTag second: want error, got nil")
	}
	if !errors.Is(err, tags.ErrNameConflict) {
		t.Fatalf("CreateTag second: got %v, want wraps tags.ErrNameConflict", err)
	}

	// The conflicting insert must not have created a second row.
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM tags`).Scan(&count); err != nil {
		t.Fatalf("count tags: %v", err)
	}
	if count != 1 {
		t.Errorf("count: got %d, want 1 (the conflicting insert must not create a row)", count)
	}
}

func TestPgRepository_CreateTag_ContextCanceled(t *testing.T) {
	pool := newTestPool(t)
	repo := tags.NewTagRepository(pool)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before the call, so the insert never reaches the server

	err := repo.CreateTag(ctx, newTag(t, "Messi"))
	if err == nil {
		t.Fatal("CreateTag: want error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("CreateTag: got %v, want wraps context.Canceled", err)
	}

	// No row should have been inserted on a canceled-before-send request.
	var count int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM tags`).Scan(&count); err != nil {
		t.Fatalf("count tags: %v", err)
	}
	if count != 0 {
		t.Errorf("count: got %d, want 0", count)
	}
}
