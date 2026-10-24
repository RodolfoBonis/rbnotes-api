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

-- FailAIJobEphemeral marca como erro um job EFÊMERO (nota cifrada) e esvazia input/result ({}): a
-- transcrição em claro não pode sobrar em ai_jobs.input numa falha. Usado em fail/giveUp/reaper e no
-- limite de uso (que, para efêmero, encerra em vez de reagendar). O consumo registrado é preservado.
-- name: FailAIJobEphemeral :exec
UPDATE ai_jobs SET status = 'error', error = $2, input = '{}'::jsonb, result = '{}'::jsonb, updated_at = now()
WHERE id = $1;

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
-- um job vivo na fila (worker morreu sem fechar a stream). Para jobs EFÊMEROS (nota cifrada) também
-- esvazia input/result ({}), para não deixar transcrição em claro. Devolve id + ephemeral (para o
-- reaper fechar a stream com o TTL curto de efêmero).
-- name: ReapStuckAIJobs :many
WITH stuck AS (
    SELECT id, COALESCE(input->>'ephemeral', '') = 'true' AS ephemeral
    FROM ai_jobs
    WHERE status IN ('queued', 'running') AND ai_jobs.updated_at < $1
      AND NOT EXISTS (
          SELECT 1 FROM jobs j
          WHERE j.payload->>'job_id' = ai_jobs.id::text AND j.status IN ('queued', 'running'))
    FOR UPDATE
)
UPDATE ai_jobs a SET
    status     = 'error',
    error      = $2,
    input      = CASE WHEN s.ephemeral THEN '{}'::jsonb ELSE a.input END,
    result     = CASE WHEN s.ephemeral THEN '{}'::jsonb ELSE a.result END,
    updated_at = now()
FROM stuck s
WHERE a.id = s.id
RETURNING a.id, s.ephemeral;

-- DeleteOldAIRequests apaga os registros de cota mais antigos que o cutoff (retenção).
-- name: DeleteOldAIRequests :exec
DELETE FROM ai_requests WHERE created_at < $1;

-- ClearOldAIJobData esvazia o payload/resultado pesado dos ai_jobs antigos, preservando o status.
-- name: ClearOldAIJobData :exec
UPDATE ai_jobs SET input = '{}'::jsonb, result = NULL
WHERE updated_at < $1 AND (input <> '{}'::jsonb OR result IS NOT NULL);

-- ClearEphemeralAIJobData é a rede de segurança da retenção para notas CIFRADAS: esvazia input/result
-- ({}) de QUALQUER job efêmero já terminal (done/error) que ainda tenha texto claro, sem esperar o
-- cutoff de retenção. Em operação normal o input já é limpo no fim (doneEphemeral/FailAIJobEphemeral);
-- isto garante que nenhuma transcrição de efêmero sobreviva caso algum caminho escape.
-- name: ClearEphemeralAIJobData :exec
UPDATE ai_jobs SET input = '{}'::jsonb, result = '{}'::jsonb
WHERE status IN ('done', 'error')
  AND COALESCE(input->>'ephemeral', '') = 'true'
  AND (input <> '{}'::jsonb OR result IS DISTINCT FROM '{}'::jsonb);

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

-- ── consumo de IA (Fase 3) ────────────────────────────────────────────────────────────────
-- CompleteAIJobEphemeral conclui um job EFÊMERO (nota cifrada): não persiste o resultado em texto
-- claro e esvazia o input (que carregava a transcrição). O resultado volta só no `done` do SSE.
-- name: CompleteAIJobEphemeral :exec
UPDATE ai_jobs SET status = 'done', result = '{}'::jsonb, input = '{}'::jsonb, model = $2, error = '', updated_at = now()
WHERE id = $1;

-- SetAIJobUsage ACUMULA o consumo informado pela CLI (ver internal/ai/claude). Acumula para somar as
-- duas execuções do retry sem --resume do chat. O resolved_model fica com o último valor não-vazio.
-- name: SetAIJobUsage :exec
UPDATE ai_jobs SET
    cost_usd              = cost_usd              + sqlc.arg(cost_usd)::numeric,
    input_tokens          = input_tokens          + sqlc.arg(input_tokens),
    output_tokens         = output_tokens         + sqlc.arg(output_tokens),
    cache_creation_tokens = cache_creation_tokens + sqlc.arg(cache_creation_tokens),
    cache_read_tokens     = cache_read_tokens     + sqlc.arg(cache_read_tokens),
    duration_ms           = duration_ms           + sqlc.arg(duration_ms),
    num_turns             = num_turns             + sqlc.arg(num_turns),
    resolved_model        = CASE WHEN sqlc.arg(resolved_model)::text <> '' THEN sqlc.arg(resolved_model)::text ELSE resolved_model END,
    updated_at            = now()
WHERE id = sqlc.arg(id);

-- AIUsageByDay agrega o consumo por dia (no fuso IANA informado) e por kind, no intervalo
-- [from_ts, to_ts). Escopo por usuário. O custo é estimativa (preço de API). Alimenta GET /v1/ai/usage.
-- name: AIUsageByDay :many
SELECT
    (created_at AT TIME ZONE sqlc.arg(tz)::text)::date AS day,
    kind,
    count(*)::bigint                                AS requests,
    count(*) FILTER (WHERE status = 'done')::bigint AS done,
    count(*) FILTER (WHERE status = 'error')::bigint AS errors,
    coalesce(sum(input_tokens), 0)::bigint          AS input_tokens,
    coalesce(sum(output_tokens), 0)::bigint         AS output_tokens,
    coalesce(sum(cache_creation_tokens), 0)::bigint AS cache_creation_tokens,
    coalesce(sum(cache_read_tokens), 0)::bigint     AS cache_read_tokens,
    coalesce(sum(cost_usd), 0)::numeric             AS cost_usd,
    coalesce(sum(duration_ms), 0)::bigint           AS duration_ms
FROM ai_jobs
WHERE user_id = sqlc.arg(user_id)
  AND created_at >= sqlc.arg(from_ts)
  AND created_at <  sqlc.arg(to_ts)
GROUP BY day, kind
ORDER BY day, kind;
