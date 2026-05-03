-- Stores the metadata of every uploaded media. The binary content lives
-- outside the database.
CREATE TABLE media (
    id           UUID        PRIMARY KEY,
    name         TEXT        NOT NULL,
    file_key     TEXT        NOT NULL UNIQUE,
    content_type TEXT        NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL
);

-- N:N join between media and tags. Every column is part of the primary
-- key, which doubles as a uniqueness guarantee (a tag is attached at
-- most once to a given media) and as the natural index on media_id.
CREATE TABLE media_tags (
    media_id UUID NOT NULL REFERENCES media(id) ON DELETE CASCADE,
    tag_id   UUID NOT NULL REFERENCES tags(id)  ON DELETE RESTRICT,
    PRIMARY KEY (media_id, tag_id)
);

-- The PK above already indexes (media_id, tag_id), so "list tags of a
-- given media" is fast. The reverse direction ("find media tagged X")
-- is the future search feature called out in the brief; without this
-- index it would be a full scan of media_tags.
CREATE INDEX media_tags_tag_id_idx ON media_tags (tag_id);

---- create above / drop below ----

DROP INDEX IF EXISTS media_tags_tag_id_idx;
DROP TABLE IF EXISTS media_tags;
DROP TABLE IF EXISTS media;
