-- +goose Up
-- Resumo semanal (Claude): novo kind weekly_summary. O CHECK original de ai_jobs.kind (migração
-- 00006) nasce com o nome implícito ai_jobs_kind_check; recriamos a constraint incluindo o novo kind.
ALTER TABLE ai_jobs DROP CONSTRAINT ai_jobs_kind_check;
ALTER TABLE ai_jobs ADD CONSTRAINT ai_jobs_kind_check
    CHECK (kind IN ('note_enrich', 'note_chat', 'ask', 'collection_rule', 'diary_month', 'daily_prompt', 'weekly_summary'));

-- +goose Down
ALTER TABLE ai_jobs DROP CONSTRAINT ai_jobs_kind_check;
ALTER TABLE ai_jobs ADD CONSTRAINT ai_jobs_kind_check
    CHECK (kind IN ('note_enrich', 'note_chat', 'ask', 'collection_rule', 'diary_month', 'daily_prompt'));
