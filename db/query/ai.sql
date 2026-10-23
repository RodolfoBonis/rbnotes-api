-- ── ai_jobs ─────────────────────────────────────────────────────────────────────────────
-- name: CreateAIJob :one
INSERT INTO ai_jobs (user_id, kind, note_id, thread_id, model, input)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetAIJob :one
SELECT * FROM ai_jobs WHERE id = $1;

-- name: SetAIJobRunning :exec
UPDATE ai_jobs SET status = 'running', updated_at = now() WHERE id = $1;

-- name: CompleteAIJob :exec
UPDATE ai_jobs SET status = 'done', result = $2, model = $3, error = '', updated_at = now()
WHERE id = $1;

-- name: FailAIJob :exec
UPDATE ai_jobs SET status = 'error', error = $2, updated_at = now() WHERE id = $1;

-- ── cota diária ─────────────────────────────────────────────────────────────────────────
-- name: CountAIRequestsSince :one
SELECT count(*) FROM ai_requests WHERE user_id = $1 AND created_at >= $2;

-- name: InsertAIRequest :exec
INSERT INTO ai_requests (user_id, kind) VALUES ($1, $2);

-- LockUserForQuota serializa a checagem de cota + criação do job por usuário (advisory lock de
-- transação): dois pedidos simultâneos do mesmo usuário não furam o teto diário.
-- name: LockUserForQuota :exec
SELECT pg_advisory_xact_lock(hashtextextended($1::text, 0));

-- ReapStuckAIJobs marca como erro os ai_jobs presos (queued/running há mais de $1) que NÃO têm mais
-- um job vivo na fila (worker morreu sem fechar a stream). Devolve os ids para fechar as streams.
-- name: ReapStuckAIJobs :many
UPDATE ai_jobs SET status = 'error', error = $2, updated_at = now()
WHERE status IN ('queued', 'running') AND ai_jobs.updated_at < $1
  AND NOT EXISTS (
      SELECT 1 FROM jobs j
      WHERE j.payload->>'job_id' = ai_jobs.id::text AND j.status IN ('queued', 'running'))
RETURNING id;

-- DeleteOldAIRequests apaga os registros de cota mais antigos que o cutoff (retenção).
-- name: DeleteOldAIRequests :exec
DELETE FROM ai_requests WHERE created_at < $1;

-- ClearOldAIJobData esvazia o payload/resultado pesado dos ai_jobs antigos, preservando o status.
-- name: ClearOldAIJobData :exec
UPDATE ai_jobs SET input = '{}'::jsonb, result = NULL
WHERE updated_at < $1 AND (input <> '{}'::jsonb OR result IS NOT NULL);

-- ── chat threads (--resume) ─────────────────────────────────────────────────────────────
-- name: GetThreadSession :one
SELECT claude_session_id FROM chat_threads WHERE user_id = $1 AND thread_id = $2;

-- name: UpsertThreadSession :exec
INSERT INTO chat_threads (user_id, thread_id, claude_session_id)
VALUES ($1, $2, $3)
ON CONFLICT (user_id, thread_id) DO UPDATE
   SET claude_session_id = EXCLUDED.claude_session_id, updated_at = now();

-- ── busca (FTS) para o `ask` + manutenção do índice ───────────────────────────────────────
-- name: UpsertNoteSearchTitle :exec
INSERT INTO note_search (user_id, note_id, title, updated_at)
VALUES ($1, $2, $3, now())
ON CONFLICT (user_id, note_id) DO UPDATE
   SET title = EXCLUDED.title, updated_at = now();

-- name: UpsertNoteSearchEnrich :exec
INSERT INTO note_search (user_id, note_id, title, tldr, transcript, updated_at)
VALUES ($1, $2, $3, $4, $5, now())
ON CONFLICT (user_id, note_id) DO UPDATE
   SET title = EXCLUDED.title, tldr = EXCLUDED.tldr, transcript = EXCLUDED.transcript, updated_at = now();

-- name: DeleteNoteSearch :exec
DELETE FROM note_search WHERE user_id = $1 AND note_id = $2;

-- SearchNotesFTS: candidatas por relevância para o `ask`. Usa websearch_to_tsquery (entende
-- pergunta em linguagem natural, aspas e OR) sobre o texto sem acento; se nenhuma nota casar com a
-- consulta "E" (todos os termos), cai para o "OU" dos lexemas (qualquer termo), para uma pergunta
-- de vários termos ainda encontrar notas parcialmente relevantes.
-- name: SearchNotesFTS :many
WITH ws AS (
    SELECT websearch_to_tsquery('portuguese', rbnotes_unaccent(sqlc.arg(q)::text)) AS q
),
orq AS (
    SELECT nullif(replace(plainto_tsquery('portuguese', rbnotes_unaccent(sqlc.arg(q)::text))::text, '&', '|'), '')::tsquery AS q
),
eff AS (
    SELECT CASE
        WHEN EXISTS (SELECT 1 FROM note_search n, ws WHERE n.user_id = sqlc.arg(uid) AND n.tsv @@ ws.q)
        THEN (SELECT q FROM ws)
        ELSE (SELECT q FROM orq)
    END AS q
)
SELECT note_id, title, tldr
FROM note_search, eff
WHERE note_search.user_id = sqlc.arg(uid) AND note_search.tsv @@ eff.q
ORDER BY ts_rank(note_search.tsv, eff.q) DESC, updated_at DESC
LIMIT sqlc.arg(lim)::int;

-- RecentNotes completa as candidatas quando a FTS traz poucas (nota recém-criada ainda sem termos).
-- name: RecentNotes :many
SELECT note_id, title, tldr FROM note_search
WHERE user_id = $1
ORDER BY updated_at DESC
LIMIT sqlc.arg(lim)::int;
