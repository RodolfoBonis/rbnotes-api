-- +goose Up
-- unaccent + pg_trgm dão a busca em português tolerante a acentos. A FTS do `ask` indexa
-- título + tldr + transcrição (ver 00004_sync).
CREATE EXTENSION IF NOT EXISTS unaccent;
CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- rbnotes_unaccent envolve unaccent num wrapper IMMUTABLE para poder entrar numa coluna gerada /
-- índice (o unaccent padrão é STABLE porque resolve o dicionário por nome em runtime; fixamos o
-- dicionário 'unaccent' e marcamos IMMUTABLE, seguro enquanto o dicionário não mudar).
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION rbnotes_unaccent(text)
    RETURNS text
    LANGUAGE sql
    IMMUTABLE STRICT PARALLEL SAFE
AS $$
    SELECT unaccent('unaccent', $1)
$$;
-- +goose StatementEnd

-- +goose Down
DROP FUNCTION IF EXISTS rbnotes_unaccent(text);
DROP EXTENSION IF EXISTS pg_trgm;
DROP EXTENSION IF EXISTS unaccent;
