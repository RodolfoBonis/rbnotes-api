-- +goose Up
-- part_sizes guarda o tamanho (bytes) recebido por número de parte, no mesmo formato de `parts`
-- ({"1": 1234}). É idempotente a reenvios (reenviar a parte N sobrescreve o tamanho dela), então a
-- soma dos valores é o total real recebido — usado para aplicar o teto AUDIO_MAX_BYTES sem contar
-- em dobro quando um chunk é reenviado.
ALTER TABLE note_audio ADD COLUMN part_sizes jsonb NOT NULL DEFAULT '{}'::jsonb;

-- +goose Down
ALTER TABLE note_audio DROP COLUMN part_sizes;
