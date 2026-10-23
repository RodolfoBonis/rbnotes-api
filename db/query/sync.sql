-- NextChangeSeq aloca o próximo change_seq do usuário (serializa o push pelo lock da linha).
-- name: NextChangeSeq :one
INSERT INTO sync_cursors (user_id, seq) VALUES ($1, 1)
ON CONFLICT (user_id) DO UPDATE SET seq = sync_cursors.seq + 1
RETURNING seq;

-- PullChanges devolve as mudanças de todas as entidades do usuário após o cursor, em ordem de
-- change_seq (cursor monotônico global por usuário).
-- name: PullChanges :many
SELECT entity, id, updated_at, deleted, data, change_seq FROM (
    SELECT 'note'::text        AS entity, notes.id, notes.updated_at, notes.deleted, notes.data, notes.change_seq FROM notes         WHERE notes.user_id = sqlc.arg(uid) AND notes.change_seq > sqlc.arg(cur)
    UNION ALL
    SELECT 'segment_set'::text AS entity, segment_sets.id, segment_sets.updated_at, segment_sets.deleted, segment_sets.data, segment_sets.change_seq FROM segment_sets  WHERE segment_sets.user_id = sqlc.arg(uid) AND segment_sets.change_seq > sqlc.arg(cur)
    UNION ALL
    SELECT 'folder'::text      AS entity, folders.id, folders.updated_at, folders.deleted, folders.data, folders.change_seq FROM folders       WHERE folders.user_id = sqlc.arg(uid) AND folders.change_seq > sqlc.arg(cur)
    UNION ALL
    SELECT 'action'::text      AS entity, actions.id, actions.updated_at, actions.deleted, actions.data, actions.change_seq FROM actions       WHERE actions.user_id = sqlc.arg(uid) AND actions.change_seq > sqlc.arg(cur)
    UNION ALL
    SELECT 'collection'::text  AS entity, collections.id, collections.updated_at, collections.deleted, collections.data, collections.change_seq FROM collections   WHERE collections.user_id = sqlc.arg(uid) AND collections.change_seq > sqlc.arg(cur)
    UNION ALL
    SELECT 'vocabulary'::text  AS entity, vocabulary.id, vocabulary.updated_at, vocabulary.deleted, vocabulary.data, vocabulary.change_seq FROM vocabulary    WHERE vocabulary.user_id = sqlc.arg(uid) AND vocabulary.change_seq > sqlc.arg(cur)
    UNION ALL
    SELECT 'chat_message'::text AS entity, chat_messages.id, chat_messages.updated_at, chat_messages.deleted, chat_messages.data, chat_messages.change_seq FROM chat_messages WHERE chat_messages.user_id = sqlc.arg(uid) AND chat_messages.change_seq > sqlc.arg(cur)
    UNION ALL
    SELECT 'note_ai'::text     AS entity, note_ai.id, note_ai.updated_at, note_ai.deleted, note_ai.data, note_ai.change_seq FROM note_ai       WHERE note_ai.user_id = sqlc.arg(uid) AND note_ai.change_seq > sqlc.arg(cur)
) t
ORDER BY change_seq
LIMIT sqlc.arg(lim)::int;

-- ── notes ───────────────────────────────────────────────────────────────────────────────
-- name: GetNote :one
SELECT updated_at, deleted, data FROM notes WHERE user_id = $1 AND id = $2;
-- name: UpsertNote :exec
INSERT INTO notes (user_id, id, updated_at, deleted, data, change_seq)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (user_id, id) DO UPDATE
   SET updated_at = EXCLUDED.updated_at, deleted = EXCLUDED.deleted,
       data = EXCLUDED.data, change_seq = EXCLUDED.change_seq, server_updated_at = now();

-- ── segment_sets ────────────────────────────────────────────────────────────────────────
-- name: GetSegmentSet :one
SELECT updated_at, deleted, data FROM segment_sets WHERE user_id = $1 AND id = $2;
-- name: UpsertSegmentSet :exec
INSERT INTO segment_sets (user_id, id, updated_at, deleted, data, change_seq)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (user_id, id) DO UPDATE
   SET updated_at = EXCLUDED.updated_at, deleted = EXCLUDED.deleted,
       data = EXCLUDED.data, change_seq = EXCLUDED.change_seq, server_updated_at = now();

-- ── folders ─────────────────────────────────────────────────────────────────────────────
-- name: GetFolder :one
SELECT updated_at, deleted, data FROM folders WHERE user_id = $1 AND id = $2;
-- name: UpsertFolder :exec
INSERT INTO folders (user_id, id, updated_at, deleted, data, change_seq)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (user_id, id) DO UPDATE
   SET updated_at = EXCLUDED.updated_at, deleted = EXCLUDED.deleted,
       data = EXCLUDED.data, change_seq = EXCLUDED.change_seq, server_updated_at = now();

-- ── actions ─────────────────────────────────────────────────────────────────────────────
-- name: GetAction :one
SELECT updated_at, deleted, data FROM actions WHERE user_id = $1 AND id = $2;
-- name: UpsertAction :exec
INSERT INTO actions (user_id, id, updated_at, deleted, data, change_seq)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (user_id, id) DO UPDATE
   SET updated_at = EXCLUDED.updated_at, deleted = EXCLUDED.deleted,
       data = EXCLUDED.data, change_seq = EXCLUDED.change_seq, server_updated_at = now();

-- ── collections ─────────────────────────────────────────────────────────────────────────
-- name: GetCollection :one
SELECT updated_at, deleted, data FROM collections WHERE user_id = $1 AND id = $2;
-- name: UpsertCollection :exec
INSERT INTO collections (user_id, id, updated_at, deleted, data, change_seq)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (user_id, id) DO UPDATE
   SET updated_at = EXCLUDED.updated_at, deleted = EXCLUDED.deleted,
       data = EXCLUDED.data, change_seq = EXCLUDED.change_seq, server_updated_at = now();

-- ── vocabulary ──────────────────────────────────────────────────────────────────────────
-- name: GetVocabulary :one
SELECT updated_at, deleted, data FROM vocabulary WHERE user_id = $1 AND id = $2;
-- name: UpsertVocabulary :exec
INSERT INTO vocabulary (user_id, id, updated_at, deleted, data, change_seq)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (user_id, id) DO UPDATE
   SET updated_at = EXCLUDED.updated_at, deleted = EXCLUDED.deleted,
       data = EXCLUDED.data, change_seq = EXCLUDED.change_seq, server_updated_at = now();

-- ── chat_messages ───────────────────────────────────────────────────────────────────────
-- name: GetChatMessage :one
SELECT updated_at, deleted, data FROM chat_messages WHERE user_id = $1 AND id = $2;
-- name: UpsertChatMessage :exec
INSERT INTO chat_messages (user_id, id, updated_at, deleted, data, change_seq)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (user_id, id) DO UPDATE
   SET updated_at = EXCLUDED.updated_at, deleted = EXCLUDED.deleted,
       data = EXCLUDED.data, change_seq = EXCLUDED.change_seq, server_updated_at = now();

-- ── note_ai ─────────────────────────────────────────────────────────────────────────────
-- name: GetNoteAi :one
SELECT updated_at, deleted, data FROM note_ai WHERE user_id = $1 AND id = $2;
-- name: UpsertNoteAi :exec
INSERT INTO note_ai (user_id, id, updated_at, deleted, data, change_seq)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (user_id, id) DO UPDATE
   SET updated_at = EXCLUDED.updated_at, deleted = EXCLUDED.deleted,
       data = EXCLUDED.data, change_seq = EXCLUDED.change_seq, server_updated_at = now();
