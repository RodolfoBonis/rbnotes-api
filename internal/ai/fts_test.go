package ai

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	dbgen "github.com/RodolfoBonis/rbnotes-api/db/gen"
	"github.com/RodolfoBonis/rbnotes-api/internal/testsupport"
)

// TestSearchNotesFTSNaturalLanguage cobre a busca do `ask` com uma pergunta em linguagem natural de
// vários termos, incluindo o fallback para o OU dos lexemas quando o "E" não casa.
func TestSearchNotesFTSNaturalLanguage(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	q := dbgen.New(pool)
	ctx := context.Background()
	u, err := q.CreateUser(ctx, dbgen.CreateUserParams{Issuer: "i", Subject: "s-" + t.Name(), Email: "a@x", Name: "A"})
	require.NoError(t, err)

	require.NoError(t, q.UpsertNoteSearchEnrich(ctx, dbgen.UpsertNoteSearchEnrichParams{
		UserID: u.ID, NoteID: "roadmap", Title: "Reunião de roadmap",
		Tldr: "Priorizar a busca antes do export", Transcript: "falamos sobre a busca e o export",
	}))
	require.NoError(t, q.UpsertNoteSearchEnrich(ctx, dbgen.UpsertNoteSearchEnrichParams{
		UserID: u.ID, NoteID: "bolo", Title: "Receita de bolo",
		Tldr: "ingredientes e modo de preparo", Transcript: "farinha, ovos e açúcar",
	}))

	ids := func(rows []dbgen.SearchNotesFTSRow) map[string]bool {
		m := map[string]bool{}
		for _, r := range rows {
			m[r.NoteID] = true
		}
		return m
	}

	// Pergunta natural com vários termos: "decidimos" não está em nenhuma nota, então o "E" não casa
	// e o fallback "OU" acha a nota por roadmap/busca.
	rows, err := q.SearchNotesFTS(ctx, dbgen.SearchNotesFTSParams{Q: "o que a gente decidiu sobre o roadmap da busca?", Uid: u.ID, Lim: 12})
	require.NoError(t, err)
	got := ids(rows)
	require.True(t, got["roadmap"], "a nota de roadmap deve aparecer (fallback OU dos lexemas)")
	require.False(t, got["bolo"], "a receita de bolo não é relevante")

	// Consulta cujos termos casam todos (E): roadmap + busca.
	rows, err = q.SearchNotesFTS(ctx, dbgen.SearchNotesFTSParams{Q: "roadmap busca", Uid: u.ID, Lim: 12})
	require.NoError(t, err)
	require.True(t, ids(rows)["roadmap"])

	// Outra pergunta direciona para a receita.
	rows, err = q.SearchNotesFTS(ctx, dbgen.SearchNotesFTSParams{Q: "ingredientes da receita de bolo", Uid: u.ID, Lim: 12})
	require.NoError(t, err)
	require.True(t, ids(rows)["bolo"])
}
