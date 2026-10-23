package migrations_test

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/require"

	migrations "github.com/RodolfoBonis/rbnotes-api/db/migrations"
	"github.com/RodolfoBonis/rbnotes-api/internal/testsupport"
)

// TestMigrationsAndFTS aplica todas as migrations, exercita o índice FTS em português (com
// unaccent) e confere que o down/up é reversível.
func TestMigrationsAndFTS(t *testing.T) {
	ctx := context.Background()
	sqlDB, err := sql.Open("pgx", testsupport.NewPostgresDSN(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	goose.SetBaseFS(migrations.FS)
	t.Cleanup(func() { goose.SetBaseFS(nil) })
	require.NoError(t, goose.SetDialect("postgres"))
	require.NoError(t, goose.UpContext(ctx, sqlDB, "."))

	var userID string
	require.NoError(t, sqlDB.QueryRowContext(ctx, `
		INSERT INTO users (issuer, subject, email, name)
		VALUES ('iss', 'sub', 'a@x', 'A') RETURNING id`).Scan(&userID))

	// Texto acentuado em pt-BR; a busca por termo SEM acento deve casar (unaccent).
	_, err = sqlDB.ExecContext(ctx, `
		INSERT INTO note_search (user_id, note_id, title, tldr, transcript)
		VALUES ($1, 'n1', 'Reunião de produção', 'Decisões sobre métricas', 'falamos de estratégia')`, userID)
	require.NoError(t, err)

	var n int
	require.NoError(t, sqlDB.QueryRowContext(ctx, `
		SELECT count(*) FROM note_search, plainto_tsquery('portuguese', rbnotes_unaccent('producao')) q
		WHERE user_id = $1 AND tsv @@ q`, userID).Scan(&n))
	require.Equal(t, 1, n, "busca sem acento (producao) casa com 'produção' via unaccent")

	require.NoError(t, sqlDB.QueryRowContext(ctx, `
		SELECT count(*) FROM note_search, plainto_tsquery('portuguese', rbnotes_unaccent('métricas')) q
		WHERE user_id = $1 AND tsv @@ q`, userID).Scan(&n))
	require.Equal(t, 1, n, "stemming pt-BR + acento casam")

	// Reversível: desce tudo e sobe de novo.
	require.NoError(t, goose.DownToContext(ctx, sqlDB, ".", 0))
	require.NoError(t, goose.UpContext(ctx, sqlDB, "."))
}
