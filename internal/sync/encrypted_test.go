package sync_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	dbgen "github.com/RodolfoBonis/rbnotes-api/db/gen"
	syncpkg "github.com/RodolfoBonis/rbnotes-api/internal/sync"
	"github.com/RodolfoBonis/rbnotes-api/internal/testsupport"
)

// TestEncryptedNotesStayOutOfSearch prova as regras das notas cifradas no sync: uma nota que vira
// cifrada é removida do índice FTS (logo some de ask/diary_month/weekly_summary), o note_ai cifrado
// vivo é aceito sem a exigência de title/tldr, e o envelope é guardado como está (round-trip).
func TestEncryptedNotesStayOutOfSearch(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	q := dbgen.New(pool)
	svc := syncpkg.New(q, pool)
	ctx := context.Background()

	u, err := q.CreateUser(ctx, dbgen.CreateUserParams{Issuer: "i", Subject: "s-" + t.Name(), Email: "a@x", Name: "A"})
	require.NoError(t, err)

	now := time.Now().UTC()
	enc := json.RawMessage(`{"enc":"v1","nonce":"YWJj","ct":"ZGVm"}`)

	// 1) Nota em texto claro → indexada no FTS (candidata a ask/diary/weekly).
	_, err = svc.Push(ctx, u.ID, []syncpkg.Change{{
		Entity: "note", ID: "n1", UpdatedAt: now, Data: json.RawMessage(`{"title":"Reunião secreta de produção"}`),
	}})
	require.NoError(t, err)
	rows, err := q.SearchNotesFTS(ctx, dbgen.SearchNotesFTSParams{Q: "producao", Uid: u.ID, Lim: 12})
	require.NoError(t, err)
	require.NotEmpty(t, rows, "a nota em claro é encontrável")

	// 2) A MESMA nota passa a ser cifrada → sai do índice.
	_, err = svc.Push(ctx, u.ID, []syncpkg.Change{{
		Entity: "note", ID: "n1", UpdatedAt: now.Add(time.Minute), Data: enc,
	}})
	require.NoError(t, err)
	rows, err = q.SearchNotesFTS(ctx, dbgen.SearchNotesFTSParams{Q: "producao", Uid: u.ID, Lim: 12})
	require.NoError(t, err)
	require.Empty(t, rows, "nota cifrada sai do FTS (e do ask/diary_month/weekly_summary)")
	recent, err := q.RecentNotes(ctx, dbgen.RecentNotesParams{UserID: u.ID, Lim: 12})
	require.NoError(t, err)
	require.Empty(t, recent, "nem como nota recente a cifrada entra no contexto de IA")

	// 3) note_ai CIFRADO vivo (sem title/tldr) é aceito — a regra 422 não vale para cifrados.
	_, err = svc.Push(ctx, u.ID, []syncpkg.Change{{
		Entity: "note_ai", ID: "n1", UpdatedAt: now, Data: enc,
	}})
	require.NoError(t, err)

	// 4) note_ai NÃO cifrado vivo sem conteúdo continua recusado (regra preservada).
	_, err = svc.Push(ctx, u.ID, []syncpkg.Change{{
		Entity: "note_ai", ID: "n2", UpdatedAt: now, Data: json.RawMessage(`{}`),
	}})
	require.ErrorIs(t, err, syncpkg.ErrInvalidNoteAi)

	// 5) O envelope cifrado é guardado como está (round-trip pelo pull).
	pull, err := svc.Pull(ctx, u.ID, "0")
	require.NoError(t, err)
	var foundNote, foundAi bool
	for _, c := range pull.Changes {
		if c.Entity == "note" && c.ID == "n1" {
			require.JSONEq(t, string(enc), string(c.Data))
			foundNote = true
		}
		if c.Entity == "note_ai" && c.ID == "n1" {
			require.JSONEq(t, string(enc), string(c.Data))
			foundAi = true
		}
	}
	require.True(t, foundNote, "a nota cifrada volta no pull")
	require.True(t, foundAi, "o note_ai cifrado volta no pull")
}
