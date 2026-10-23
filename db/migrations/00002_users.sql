-- +goose Up
-- Cada pessoa é um tenant isolado: tudo é escopo por user_id. O usuário nasce no primeiro request
-- autenticado (identidade do IdP: issuer + subject). O app é pessoal — não há dono/convite.
CREATE TABLE users (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    issuer       text        NOT NULL,
    subject      text        NOT NULL,
    email        text        NOT NULL DEFAULT '',
    name         text        NOT NULL DEFAULT '',
    last_seen_at timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);
-- Identidade estável (o mesmo sub em realms diferentes é outra pessoa).
CREATE UNIQUE INDEX uq_users_identity ON users (issuer, subject);

-- +goose Down
DROP TABLE users;
