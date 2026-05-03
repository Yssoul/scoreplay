//go:build integration

package mediaintegration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ygalmessas/scoreplay/internal/media"
)

// newMedia builds a domain Media with a freshly generated UUIDv7 and
// a deterministic file_key derived from the same id.
func newMedia(t *testing.T, name string) media.Media {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("uuid.NewV7: %v", err)
	}
	return media.Media{
		ID:          id,
		Name:        name,
		FileKey:     "blob/" + id.String(),
		ContentType: "image/jpeg",
	}
}

// seedTag inserts a tag straight via SQL so media integration tests
// stay independent from the tags repository implementation. The
// returned id is what the media handler would receive over the wire
// from a previous POST /tags call.
func seedTag(t *testing.T, pool *pgxpool.Pool, name string) uuid.UUID {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("uuid.NewV7: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO tags (id, name, created_at) VALUES ($1, $2, $3)`,
		id, name, time.Now().UTC(),
	); err != nil {
		t.Fatalf("seed tag %q: %v", name, err)
	}
	return id
}

// countMediaTags returns how many media_tags rows reference mediaID.
// Used as a transactional witness: after a failed Create it must be
// zero, after a successful one it must equal the attached set.
func countMediaTags(t *testing.T, pool *pgxpool.Pool, mediaID uuid.UUID) int {
	t.Helper()
	var n int
	err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM media_tags WHERE media_id = $1`, mediaID,
	).Scan(&n)
	if err != nil {
		t.Fatalf("count media_tags: %v", err)
	}
	return n
}

func TestPgRepository_Create_Success(t *testing.T) {
	pool := newTestPool(t)
	repo := media.NewPgRepository(pool)
	ctx := context.Background()

	tagA := seedTag(t, pool, "Messi")
	tagB := seedTag(t, pool, "Real Madrid")

	before := time.Now().UTC()
	m := newMedia(t, "Goal of the year")
	if err := repo.Create(ctx, m, []uuid.UUID{tagA, tagB}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	after := time.Now().UTC()

	// The media row must be persisted with id, name, file_key and
	// content_type as supplied, and a created_at stamped by the
	// repository within the test window.
	var (
		gotName        string
		gotFileKey     string
		gotContentType string
		gotCreatedAt   time.Time
	)
	err := pool.QueryRow(ctx,
		`SELECT name, file_key, content_type, created_at FROM media WHERE id = $1`, m.ID,
	).Scan(&gotName, &gotFileKey, &gotContentType, &gotCreatedAt)
	if err != nil {
		t.Fatalf("select media: %v", err)
	}
	if gotName != m.Name {
		t.Errorf("name: got %q, want %q", gotName, m.Name)
	}
	if gotFileKey != m.FileKey {
		t.Errorf("file_key: got %q, want %q", gotFileKey, m.FileKey)
	}
	if gotContentType != m.ContentType {
		t.Errorf("content_type: got %q, want %q", gotContentType, m.ContentType)
	}
	if gotCreatedAt.Before(before) || gotCreatedAt.After(after) {
		t.Errorf("created_at %s not within [%s, %s]", gotCreatedAt, before, after)
	}

	if got := countMediaTags(t, pool, m.ID); got != 2 {
		t.Errorf("media_tags rows: got %d, want 2", got)
	}
}

func TestPgRepository_Create_NoTags_OK(t *testing.T) {
	pool := newTestPool(t)
	repo := media.NewPgRepository(pool)
	ctx := context.Background()

	// The brief allows a media to be created with an empty tag list.
	// The repository must accept it and create no media_tags row.
	m := newMedia(t, "Untagged shot")
	if err := repo.Create(ctx, m, nil); err != nil {
		t.Fatalf("Create with no tags: %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM media WHERE id = $1`, m.ID).Scan(&count); err != nil {
		t.Fatalf("count media: %v", err)
	}
	if count != 1 {
		t.Errorf("media row count: got %d, want 1", count)
	}
	if got := countMediaTags(t, pool, m.ID); got != 0 {
		t.Errorf("media_tags rows: got %d, want 0", got)
	}
}

// TestPgRepository_Create_FailsOnUnknownTag_LeavesNoPartialState
// pins the transactional contract of Create: a tag id that does not
// exist must trip the foreign key, and the rollback must remove
// every trace of the failed attempt — including the media row that
// was inserted just before the bad AttachTag.
func TestPgRepository_Create_FailsOnUnknownTag_LeavesNoPartialState(t *testing.T) {
	pool := newTestPool(t)
	repo := media.NewPgRepository(pool)
	ctx := context.Background()

	good := seedTag(t, pool, "Messi")
	bogus, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("uuid.NewV7: %v", err)
	}

	m := newMedia(t, "Goal of the year")
	err = repo.Create(ctx, m, []uuid.UUID{good, bogus})
	if err == nil {
		t.Fatalf("Create with unknown tag: want error, got nil")
	}

	var mediaCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM media WHERE id = $1`, m.ID).Scan(&mediaCount); err != nil {
		t.Fatalf("count media: %v", err)
	}
	if mediaCount != 0 {
		t.Errorf("media row count after rollback: got %d, want 0", mediaCount)
	}
	if got := countMediaTags(t, pool, m.ID); got != 0 {
		t.Errorf("media_tags rows after rollback: got %d, want 0", got)
	}
}

func TestPgRepository_Get_ReturnsMediaAndTags(t *testing.T) {
	pool := newTestPool(t)
	repo := media.NewPgRepository(pool)
	ctx := context.Background()

	// Tags are seeded out of alphabetical order to verify Get sorts
	// them by name ascending (the SQL contract of ListTagsByMediaID).
	tagMessi := seedTag(t, pool, "Messi")
	tagBarca := seedTag(t, pool, "Barça")
	tagCL := seedTag(t, pool, "Champions League")

	m := newMedia(t, "Iconic free kick")
	if err := repo.Create(ctx, m, []uuid.UUID{tagMessi, tagBarca, tagCL}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	gotMedia, gotTags, err := repo.Get(ctx, m.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if gotMedia.ID != m.ID || gotMedia.Name != m.Name ||
		gotMedia.FileKey != m.FileKey || gotMedia.ContentType != m.ContentType {
		t.Errorf("media: got %+v, want %+v", gotMedia, m)
	}

	wantNames := []string{"Barça", "Champions League", "Messi"}
	if len(gotTags) != len(wantNames) {
		t.Fatalf("tag count: got %d, want %d (got=%+v)", len(gotTags), len(wantNames), gotTags)
	}
	for i, name := range wantNames {
		if gotTags[i].Name != name {
			t.Errorf("tag[%d]: got %q, want %q", i, gotTags[i].Name, name)
		}
	}
}

func TestPgRepository_Get_NotFound_ReturnsErrMediaNotFound(t *testing.T) {
	pool := newTestPool(t)
	repo := media.NewPgRepository(pool)

	missing, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("uuid.NewV7: %v", err)
	}

	gotMedia, gotTags, err := repo.Get(context.Background(), missing)
	if !errors.Is(err, media.ErrMediaNotFound) {
		t.Fatalf("err: got %v, want wraps media.ErrMediaNotFound", err)
	}
	if gotMedia != (media.Media{}) {
		t.Errorf("media on miss: got %+v, want zero value", gotMedia)
	}
	if gotTags != nil {
		t.Errorf("tags on miss: got %+v, want nil", gotTags)
	}
}

func TestPgRepository_MissingTags(t *testing.T) {
	pool := newTestPool(t)
	repo := media.NewPgRepository(pool)
	ctx := context.Background()

	existing1 := seedTag(t, pool, "Messi")
	existing2 := seedTag(t, pool, "Barça")
	missing1, _ := uuid.NewV7()
	missing2, _ := uuid.NewV7()

	t.Run("nil input returns nil", func(t *testing.T) {
		got, err := repo.MissingTags(ctx, nil)
		if err != nil {
			t.Fatalf("MissingTags(nil): %v", err)
		}
		if got != nil {
			t.Errorf("got %v, want nil", got)
		}
	})

	t.Run("all existing returns empty", func(t *testing.T) {
		got, err := repo.MissingTags(ctx, []uuid.UUID{existing1, existing2})
		if err != nil {
			t.Fatalf("MissingTags: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("got %v, want empty", got)
		}
	})

	t.Run("returns missing in input order", func(t *testing.T) {
		// Order matters for the API contract: the handler echoes the
		// missing ids back to the client, and a deterministic order
		// makes responses reproducible.
		input := []uuid.UUID{existing1, missing1, existing2, missing2}
		got, err := repo.MissingTags(ctx, input)
		if err != nil {
			t.Fatalf("MissingTags: %v", err)
		}
		want := []uuid.UUID{missing1, missing2}
		if len(got) != len(want) {
			t.Fatalf("len: got %d, want %d (got=%v)", len(got), len(want), got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("idx %d: got %s, want %s", i, got[i], want[i])
			}
		}
	})
}
