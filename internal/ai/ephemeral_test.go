package ai

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/require"

	dbgen "github.com/RodolfoBonis/rbnotes-api/db/gen"
	"github.com/RodolfoBonis/rbnotes-api/internal/ai/stream"
	"github.com/RodolfoBonis/rbnotes-api/internal/platform/queue"
	"github.com/RodolfoBonis/rbnotes-api/internal/testsupport"
)

// TestEnrichEphemeralLeavesNoClearText prova que o enrich de uma nota CIFRADA (ephemeral) não deixa
// NENHUM texto claro no banco: não grava a entidade note_ai, não indexa no FTS e esvazia
// input/result do ai_jobs. O resultado volta só no `done` do SSE (o cliente cifra e guarda).
func TestEnrichEphemeralLeavesNoClearText(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	rdb := testsupport.NewRedis(t)
	q := dbgen.New(pool)
	ctx := context.Background()

	u, err := q.CreateUser(ctx, dbgen.CreateUserParams{Issuer: "i", Subject: "s-" + t.Name(), Email: "a@x", Name: "A"})
	require.NoError(t, err)
	ts := pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}
	require.NoError(t, q.UpsertNote(ctx, dbgen.UpsertNoteParams{
		UserID: u.ID, ID: "n1", UpdatedAt: ts, Deleted: false, Data: []byte(`{"enc":"v1","nonce":"x","ct":"y"}`), ChangeSeq: 1,
	}))

	const secret = "ConteudoSecretoDaNotaCifrada"
	structured := []byte(`{"title":"` + secret + `","tldr":"tldr secreto","points":["p"],` +
		`"quote":null,"actions":[],"decisions":[],"questions":[],"speakerNames":{"others":[],"segments":[]}}`)
	runner := fakeEnrichRunner{structured: structured}

	svc := New(Deps{
		Queries: q, Pool: pool, Redis: rdb, Runner: runner, Model: "haiku",
		Workdir: t.TempDir(), Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	in := EnrichInput{
		Transcript: []Segment{{Index: 0, Speaker: "me", StartMs: 0, EndMs: 1000, Text: "texto falado secreto"}},
		Ephemeral:  true,
	}
	inputJSON, err := json.Marshal(in)
	require.NoError(t, err)
	job, err := q.CreateAIJob(ctx, dbgen.CreateAIJobParams{UserID: u.ID, Kind: KindEnrich, NoteID: "n1", Model: "haiku", Input: inputJSON})
	require.NoError(t, err)
	payload, err := json.Marshal(jobPayload{JobID: uuidString(job.ID)})
	require.NoError(t, err)

	require.NoError(t, svc.Handle(ctx, queue.Job{ID: "j1", Payload: payload}))

	// 1) Nenhuma entidade note_ai gravada em texto claro.
	_, err = q.GetNoteAi(ctx, dbgen.GetNoteAiParams{UserID: u.ID, ID: "n1"})
	require.ErrorIs(t, err, pgx.ErrNoRows, "não grava note_ai de nota cifrada")

	// 2) Nada indexado no FTS.
	recent, err := q.RecentNotes(ctx, dbgen.RecentNotesParams{UserID: u.ID, Lim: 50})
	require.NoError(t, err)
	require.Empty(t, recent, "nota cifrada não é indexada (fora de ask/diary/weekly)")

	// 3) ai_jobs sem texto claro: input e result = {} e status done.
	got, err := q.GetAIJob(ctx, job.ID)
	require.NoError(t, err)
	require.Equal(t, "done", got.Status)
	require.JSONEq(t, "{}", string(got.Input), "input (transcrição) foi limpo")
	require.JSONEq(t, "{}", string(got.Result), "result foi limpo")

	// 4) Varredura: o segredo não aparece em NENHUM lugar textual do ai_jobs.
	var cnt int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM ai_jobs WHERE input::text LIKE '%'||$1||'%' OR result::text LIKE '%'||$1||'%' OR error LIKE '%'||$1||'%'`,
		secret).Scan(&cnt))
	require.Equal(t, 0, cnt, "nenhum texto claro da nota cifrada ficou no ai_jobs")

	// 5) O consumo continua registrado (não é texto claro).
	require.GreaterOrEqual(t, got.NumTurns, int32(0))

	// 6) O resultado (texto claro) volta só pelo SSE `done`.
	evs, err := stream.Read(ctx, rdb, uuidString(job.ID), "0", 500*time.Millisecond)
	require.NoError(t, err)
	require.NotEmpty(t, evs)
	last := evs[len(evs)-1]
	require.Equal(t, stream.TypeDone, last.Type)
	require.True(t, strings.Contains(last.Data, secret), "o cliente recebe o resultado pelo SSE")
}
