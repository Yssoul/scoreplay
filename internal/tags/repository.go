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

type pgTagRepository struct {
	queries *tagsdb.Queries
}

func NewTagRepository(pool *pgxpool.Pool) *pgTagRepository {
	return &pgTagRepository{queries: tagsdb.New(pool)}
}

func (r *pgTagRepository) CreateTag(ctx context.Context, tag Tag) error {
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
