package media

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	mediadb "github.com/ygalmessas/scoreplay/internal/media/db"
)

// PgRepository persists media and their tag attachments in Postgres.
type PgRepository struct {
	pool    *pgxpool.Pool
	queries *mediadb.Queries
}

// NewPgRepository builds a repository backed by the given pool.
func NewPgRepository(pool *pgxpool.Pool) *PgRepository {
	return &PgRepository{pool: pool, queries: mediadb.New(pool)}
}

// Create inserts a media row and attaches the requested tags in a
// single transaction, so a partial state can never be observed by
// concurrent readers.
func (r *PgRepository) Create(ctx context.Context, m Media, tagIDs []uuid.UUID) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("create media: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := r.queries.WithTx(tx)

	if err := q.CreateMedia(ctx, mediadb.CreateMediaParams{
		ID:          m.ID,
		Name:        m.Name,
		FileKey:     m.FileKey,
		ContentType: m.ContentType,
		CreatedAt:   time.Now().UTC(),
	}); err != nil {
		return fmt.Errorf("create media: insert: %w", err)
	}

	for _, tagID := range tagIDs {
		if err := q.AttachTag(ctx, mediadb.AttachTagParams{
			MediaID: m.ID,
			TagID:   tagID,
		}); err != nil {
			return fmt.Errorf("create media: attach tag %s: %w", tagID, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("create media: commit: %w", err)
	}
	return nil
}

// Get returns the media metadata together with the tags currently
// attached to it.
func (r *PgRepository) Get(ctx context.Context, id uuid.UUID) (Media, []Tag, error) {
	row, err := r.queries.GetMedia(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Media{}, nil, ErrMediaNotFound
		}
		return Media{}, nil, fmt.Errorf("get media: %w", err)
	}

	tagRows, err := r.queries.ListTagsByMediaID(ctx, id)
	if err != nil {
		return Media{}, nil, fmt.Errorf("get media tags: %w", err)
	}

	tags := make([]Tag, 0, len(tagRows))
	for _, t := range tagRows {
		tags = append(tags, Tag{ID: t.ID, Name: t.Name})
	}

	return Media{
		ID:          row.ID,
		Name:        row.Name,
		FileKey:     row.FileKey,
		ContentType: row.ContentType,
	}, tags, nil
}

// MissingTags returns the subset of ids that does not exist in the
// `tags` table. An empty (or nil) slice means every id is valid.
func (r *PgRepository) MissingTags(ctx context.Context, ids []uuid.UUID) ([]uuid.UUID, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	found, err := r.queries.TagsExist(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("missing tags: %w", err)
	}

	foundSet := make(map[uuid.UUID]struct{}, len(found))
	for _, id := range found {
		foundSet[id] = struct{}{}
	}

	var missing []uuid.UUID
	for _, id := range ids {
		if _, ok := foundSet[id]; !ok {
			missing = append(missing, id)
		}
	}
	return missing, nil
}
