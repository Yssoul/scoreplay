-- name: CreateTag :exec
INSERT INTO tags (id, name, created_at) VALUES ($1, $2, $3);

-- name: ListTags :many
SELECT id, name, created_at FROM tags ORDER BY name ASC;
