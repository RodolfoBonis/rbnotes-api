-- +goose Up
-- Consumo de IA por pedido. Cada ai_jobs passa a registrar o custo e os tokens informados pela CLI
-- na linha `result` (ver internal/ai/claude): custo estimado em USD (preço de API — o dono usa
-- assinatura, então é só estimativa), tokens de entrada/saída e de cache, duração, número de turnos
-- e o modelo realmente resolvido pela CLI (ex.: claude-haiku-4-5-20251001). Alimenta GET /v1/ai/usage.
-- Defaults 0/'' para as linhas já existentes; o worker acumula via SetAIJobUsage.
ALTER TABLE ai_jobs
    ADD COLUMN cost_usd              numeric(12, 6) NOT NULL DEFAULT 0,
    ADD COLUMN input_tokens          bigint         NOT NULL DEFAULT 0,
    ADD COLUMN output_tokens         bigint         NOT NULL DEFAULT 0,
    ADD COLUMN cache_creation_tokens bigint         NOT NULL DEFAULT 0,
    ADD COLUMN cache_read_tokens     bigint         NOT NULL DEFAULT 0,
    ADD COLUMN duration_ms           integer        NOT NULL DEFAULT 0,
    ADD COLUMN num_turns             integer        NOT NULL DEFAULT 0,
    ADD COLUMN resolved_model        text           NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE ai_jobs
    DROP COLUMN cost_usd,
    DROP COLUMN input_tokens,
    DROP COLUMN output_tokens,
    DROP COLUMN cache_creation_tokens,
    DROP COLUMN cache_read_tokens,
    DROP COLUMN duration_ms,
    DROP COLUMN num_turns,
    DROP COLUMN resolved_model;
