-- name: GetAudio :one
SELECT * FROM note_audio WHERE user_id = $1 AND note_id = $2;

-- GetAudioForUpdate trava a linha da nota (FOR UPDATE) para serializar os PUTs concorrentes de
-- partes da MESMA nota: só um envio por nota avança por vez, evitando dupla finalização.
-- name: GetAudioForUpdate :one
SELECT * FROM note_audio WHERE user_id = $1 AND note_id = $2 FOR UPDATE;

-- UpsertAudioInit registra (ou reinicia) o multipart upload da nota.
-- name: UpsertAudioInit :one
INSERT INTO note_audio (user_id, note_id, object_key, upload_id, total_parts, content_type)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (user_id, note_id) DO UPDATE
   SET object_key = EXCLUDED.object_key, upload_id = EXCLUDED.upload_id,
       total_parts = EXCLUDED.total_parts, content_type = EXCLUDED.content_type,
       parts = '{}'::jsonb, part_sizes = '{}'::jsonb, completed = false, size = 0, updated_at = now()
RETURNING *;

-- SetAudioPart grava o ETag e o tamanho de uma parte (merge nos mapas parts/part_sizes).
-- name: SetAudioPart :one
UPDATE note_audio
   SET parts = parts || sqlc.arg(part)::jsonb,
       part_sizes = part_sizes || sqlc.arg(part_size)::jsonb,
       updated_at = now()
WHERE user_id = $1 AND note_id = $2
RETURNING *;

-- name: CompleteAudio :exec
UPDATE note_audio SET completed = true, size = $3, updated_at = now()
WHERE user_id = $1 AND note_id = $2;

-- DeleteAudio apaga o estado do upload/áudio da nota (escopo por usuário). Idempotente na borda: o
-- serviço aborta o multipart em andamento e remove o objeto antes de chamar isto.
-- name: DeleteAudio :exec
DELETE FROM note_audio WHERE user_id = $1 AND note_id = $2;
