-- Case-insensitive text type, used for tag names so "Messi" and "messi"
-- collide on the UNIQUE constraint.
CREATE EXTENSION IF NOT EXISTS citext;

-- Trigram operators + GIN support, for future fuzzy/autocomplete search on
-- tag names (ILIKE '%mes%', similarity()). Cheap to enable now; painful to
-- backfill the index later on a populated table without CONCURRENTLY.
CREATE EXTENSION IF NOT EXISTS pg_trgm;

CREATE TABLE tags (
    id         UUID        PRIMARY KEY,
    name       CITEXT      NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL
);

-- GIN trigram index on name for fast ILIKE / similarity queries.
CREATE INDEX tags_name_trgm_idx ON tags USING GIN (name gin_trgm_ops);

---- create above / drop below ----

DROP INDEX IF EXISTS tags_name_trgm_idx;
DROP TABLE IF EXISTS tags;
-- Extensions are intentionally left in place: they're shared, idempotent,
-- and likely used by other tables (media, media_tags) in upcoming migrations.
