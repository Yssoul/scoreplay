package tags

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	tagsdb "github.com/ygalmessas/scoreplay/internal/tags/db"
)

// PgRepository persists tags in Postgres.
type PgRepository struct {
	queries *tagsdb.Queries
}

// NewPgRepository builds a media repository.
func NewPgRepository(pool *pgxpool.Pool) *PgRepository {
	return &PgRepository{queries: tagsdb.New(pool)}
}

func (r *PgRepository) CreateTag(ctx context.Context, tag Tag) error {
	query := tagsdb.CreateTagParams{
		ID:        tag.ID,
		Name:      tag.Name,
		CreatedAt: time.Now().UTC(),
	}
	if err := r.queries.CreateTag(ctx, query); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgerrcode.UniqueViolation {
			return fmt.Errorf("%w: %s", ErrNameConflict, tag.Name)
		}
		return fmt.Errorf("create tag: %w", err)
	}
	return nil
}

func (r *PgRepository) ListTags(ctx context.Context) ([]Tag, error) {
	rows, err := r.queries.ListTags(ctx)
	if err != nil {
		return nil, fmt.Errorf("list tags: %w", err)
	}
	out := make([]Tag, 0, len(rows))
	for _, row := range rows {
		out = append(out, Tag{ID: row.ID, Name: row.Name})
	}
	return out, nil
}
