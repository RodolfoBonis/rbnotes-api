-- name: GetAudio :one
SELECT * FROM note_audio WHERE user_id = $1 AND note_id = $2;

-- UpsertAudioInit registra (ou reinicia) o multipart upload da nota.
-- name: UpsertAudioInit :one
INSERT INTO note_audio (user_id, note_id, object_key, upload_id, total_parts, content_type)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (user_id, note_id) DO UPDATE
   SET object_key = EXCLUDED.object_key, upload_id = EXCLUDED.upload_id,
       total_parts = EXCLUDED.total_parts, content_type = EXCLUDED.content_type,
       parts = '{}'::jsonb, completed = false, size = 0, updated_at = now()
RETURNING *;

-- SetAudioPart grava o ETag de uma parte (merge no mapa parts).
-- name: SetAudioPart :one
UPDATE note_audio SET parts = parts || sqlc.arg(part)::jsonb, updated_at = now()
WHERE user_id = $1 AND note_id = $2
RETURNING *;

-- name: CompleteAudio :exec
UPDATE note_audio SET completed = true, size = $3, updated_at = now()
WHERE user_id = $1 AND note_id = $2;
