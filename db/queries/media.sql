-- name: CreateMedia :exec
INSERT INTO media (id, name, file_key, content_type, created_at)
VALUES ($1, $2, $3, $4, $5);

-- name: AttachTag :exec
INSERT INTO media_tags (media_id, tag_id) VALUES ($1, $2);

-- name: TagsExist :many
SELECT id FROM tags WHERE id = ANY(@ids::uuid[]);

-- name: GetMedia :one
-- Fetches the metadata of a single media.
SELECT id, name, file_key, content_type, created_at
FROM media
WHERE id = $1;

-- name: ListTagsByMediaID :many
-- Returns every tag attached to the given media, ordered by name so
-- the API response is deterministic.
SELECT t.id, t.name, t.created_at
FROM tags t
JOIN media_tags mt ON mt.tag_id = t.id
WHERE mt.media_id = $1
ORDER BY t.name ASC;
