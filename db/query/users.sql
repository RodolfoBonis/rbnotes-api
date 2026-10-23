-- name: GetUserByIdentity :one
SELECT * FROM users WHERE issuer = $1 AND subject = $2;

-- name: CreateUser :one
INSERT INTO users (issuer, subject, email, name, last_seen_at)
VALUES ($1, $2, $3, $4, now())
RETURNING *;

-- name: TouchUser :exec
UPDATE users SET email = $2, name = $3, last_seen_at = now(), updated_at = now()
WHERE id = $1;
