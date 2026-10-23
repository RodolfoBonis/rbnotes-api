-- +goose Up
-- Sincronização offline-first. Cada entidade tem sua própria tabela, mas todas compartilham a
-- mesma forma: id (gerado no cliente), updated_at (relógio do cliente, base do LWW), deleted,
-- data (o modelo do cliente em JSON) e change_seq (cursor monotônico GLOBAL por usuário — ver
-- sync_cursors). A PK (user_id, id) garante o isolamento por usuário.

-- sync_cursors aloca o change_seq por usuário. O push serializa pelo lock desta linha, então o
-- change_seq é estritamente crescente e único por usuário em todas as entidades (cursor do pull).
CREATE TABLE sync_cursors (
    user_id uuid   PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    seq     bigint NOT NULL DEFAULT 0
);

CREATE TABLE notes (
    user_id           uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    id                text        NOT NULL,
    updated_at        timestamptz NOT NULL,
    deleted           boolean     NOT NULL DEFAULT false,
    data              jsonb       NOT NULL DEFAULT '{}'::jsonb,
    change_seq        bigint      NOT NULL,
    server_updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, id)
);
CREATE INDEX idx_notes_seq ON notes (user_id, change_seq);

CREATE TABLE segment_sets (
    user_id           uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    id                text        NOT NULL,
    updated_at        timestamptz NOT NULL,
    deleted           boolean     NOT NULL DEFAULT false,
    data              jsonb       NOT NULL DEFAULT '{}'::jsonb,
    change_seq        bigint      NOT NULL,
    server_updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, id)
);
CREATE INDEX idx_segment_sets_seq ON segment_sets (user_id, change_seq);

CREATE TABLE folders (
    user_id           uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    id                text        NOT NULL,
    updated_at        timestamptz NOT NULL,
    deleted           boolean     NOT NULL DEFAULT false,
    data              jsonb       NOT NULL DEFAULT '{}'::jsonb,
    change_seq        bigint      NOT NULL,
    server_updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, id)
);
CREATE INDEX idx_folders_seq ON folders (user_id, change_seq);

CREATE TABLE actions (
    user_id           uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    id                text        NOT NULL,
    updated_at        timestamptz NOT NULL,
    deleted           boolean     NOT NULL DEFAULT false,
    data              jsonb       NOT NULL DEFAULT '{}'::jsonb,
    change_seq        bigint      NOT NULL,
    server_updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, id)
);
CREATE INDEX idx_actions_seq ON actions (user_id, change_seq);

CREATE TABLE collections (
    user_id           uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    id                text        NOT NULL,
    updated_at        timestamptz NOT NULL,
    deleted           boolean     NOT NULL DEFAULT false,
    data              jsonb       NOT NULL DEFAULT '{}'::jsonb,
    change_seq        bigint      NOT NULL,
    server_updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, id)
);
CREATE INDEX idx_collections_seq ON collections (user_id, change_seq);

CREATE TABLE vocabulary (
    user_id           uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    id                text        NOT NULL,
    updated_at        timestamptz NOT NULL,
    deleted           boolean     NOT NULL DEFAULT false,
    data              jsonb       NOT NULL DEFAULT '{}'::jsonb,
    change_seq        bigint      NOT NULL,
    server_updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, id)
);
CREATE INDEX idx_vocabulary_seq ON vocabulary (user_id, change_seq);

CREATE TABLE chat_messages (
    user_id           uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    id                text        NOT NULL,
    updated_at        timestamptz NOT NULL,
    deleted           boolean     NOT NULL DEFAULT false,
    data              jsonb       NOT NULL DEFAULT '{}'::jsonb,
    change_seq        bigint      NOT NULL,
    server_updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, id)
);
CREATE INDEX idx_chat_messages_seq ON chat_messages (user_id, change_seq);

CREATE TABLE note_ai (
    user_id           uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    id                text        NOT NULL,
    updated_at        timestamptz NOT NULL,
    deleted           boolean     NOT NULL DEFAULT false,
    data              jsonb       NOT NULL DEFAULT '{}'::jsonb,
    change_seq        bigint      NOT NULL,
    server_updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, id)
);
CREATE INDEX idx_note_ai_seq ON note_ai (user_id, change_seq);

-- note_search: índice FTS em português (unaccent) sobre título + tldr + transcrição, para o `ask`.
-- Mantido pelo servidor: título vem do push da nota e do enrich; tldr + transcrição vêm do enrich.
CREATE TABLE note_search (
    user_id    uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    note_id    text        NOT NULL,
    title      text        NOT NULL DEFAULT '',
    tldr       text        NOT NULL DEFAULT '',
    transcript text        NOT NULL DEFAULT '',
    updated_at timestamptz NOT NULL DEFAULT now(),
    tsv        tsvector    GENERATED ALWAYS AS (
        to_tsvector('portuguese',
            rbnotes_unaccent(coalesce(title, '') || ' ' || coalesce(tldr, '') || ' ' || coalesce(transcript, '')))
    ) STORED,
    PRIMARY KEY (user_id, note_id)
);
CREATE INDEX idx_note_search_tsv ON note_search USING gin (tsv);
CREATE INDEX idx_note_search_recent ON note_search (user_id, updated_at DESC);

-- +goose Down
DROP TABLE note_search;
DROP TABLE note_ai;
DROP TABLE chat_messages;
DROP TABLE vocabulary;
DROP TABLE collections;
DROP TABLE actions;
DROP TABLE folders;
DROP TABLE segment_sets;
DROP TABLE notes;
DROP TABLE sync_cursors;
