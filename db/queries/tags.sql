-- name: CreateTag :exec
INSERT INTO tags (id, name, created_at) VALUES ($1, $2, $3);
