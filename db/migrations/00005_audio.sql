-- +goose Up
-- Áudio das notas: enviado em chunks pela API e montado como um multipart upload no MinIO interno
-- (objeto users/<uid>/notes/<id>.m4a). O estado do multipart vive aqui entre as requisições de
-- parte (cada PUT é stateless): upload_id do S3 e os ETags por número de parte.
CREATE TABLE note_audio (
    user_id      uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    note_id      text        NOT NULL,
    object_key   text        NOT NULL,
    upload_id    text        NOT NULL DEFAULT '',
    total_parts  integer     NOT NULL DEFAULT 0,
    parts        jsonb       NOT NULL DEFAULT '{}'::jsonb,
    content_type text        NOT NULL DEFAULT 'audio/mp4',
    size         bigint      NOT NULL DEFAULT 0,
    completed    boolean     NOT NULL DEFAULT false,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, note_id)
);

-- +goose Down
DROP TABLE note_audio;
