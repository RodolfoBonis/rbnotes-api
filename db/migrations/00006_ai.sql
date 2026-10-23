-- +goose Up
-- IA assíncrona. Cada pedido vira um ai_jobs (fonte de verdade do status/resultado para o SSE) e
-- um job na fila (jobs) consumido pelo ai-worker. O resultado é persistido aqui e, quando a saída
-- é uma entidade sincronizável (note_enrich -> note_ai, note_chat -> chat_messages), também é
-- gravado na tabela de sync (chega no próximo pull).
CREATE TABLE ai_jobs (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    kind       text        NOT NULL
        CHECK (kind IN ('note_enrich', 'note_chat', 'ask', 'collection_rule', 'diary_month', 'daily_prompt')),
    status     text        NOT NULL DEFAULT 'queued'
        CHECK (status IN ('queued', 'running', 'done', 'error')),
    note_id    text        NOT NULL DEFAULT '',
    thread_id  text        NOT NULL DEFAULT '',
    model      text        NOT NULL DEFAULT '',
    input      jsonb       NOT NULL DEFAULT '{}'::jsonb,
    result     jsonb,
    error      text        NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_ai_jobs_user ON ai_jobs (user_id, created_at DESC);

-- chat_threads mapeia o threadId (gerado no cliente) para a sessão do Claude (--resume), por
-- usuário. O workdir da CLI é estável por thread (chat/<threadId>), exigência do --resume.
CREATE TABLE chat_threads (
    user_id           uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    thread_id         text        NOT NULL,
    claude_session_id text        NOT NULL DEFAULT '',
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, thread_id)
);

-- ai_requests: cota diária por usuário (AI_DAILY_LIMIT pedidos em 24 h).
CREATE TABLE ai_requests (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    kind       text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_ai_requests_user_time ON ai_requests (user_id, created_at DESC);

-- +goose Down
DROP TABLE ai_requests;
DROP TABLE chat_threads;
DROP TABLE ai_jobs;
