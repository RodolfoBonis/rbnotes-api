package ai

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/require"

	dbgen "github.com/RodolfoBonis/rbnotes-api/db/gen"
	"github.com/RodolfoBonis/rbnotes-api/internal/ai/claude"
	"github.com/RodolfoBonis/rbnotes-api/internal/ai/stream"
	"github.com/RodolfoBonis/rbnotes-api/internal/platform/queue"
	"github.com/RodolfoBonis/rbnotes-api/internal/testsupport"
)

// fakeEnrichRunner devolve uma saída estruturada fixa, emitindo antes alguns json parciais (como a
// CLI faria), para o teste exercitar o caminho real de handleEnrich.
type fakeEnrichRunner struct {
	structured []byte
	partials   []string
}

func (f fakeEnrichRunner) Run(_ context.Context, _ claude.Request, onEvent func(claude.Event)) (claude.Result, error) {
	for _, p := range f.partials {
		onEvent(claude.Event{Type: claude.EventJSON, Text: p})
	}
	return claude.Result{Structured: f.structured}, nil
}

// TestEnrichDoneCarriesResultAndWritesEntity: ao concluir o enrich, o evento `done` da stream carrega
// o RESULTADO canônico (não vazio) e a entidade note_ai é gravada com o resumo correto. Era aqui que
// o desktop caía no bug do envelope: sem resultado no done, salvava note_ai {} e marcava ready.
func TestEnrichDoneCarriesResultAndWritesEntity(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	rdb := testsupport.NewRedis(t)
	q := dbgen.New(pool)
	ctx := context.Background()

	u, err := q.CreateUser(ctx, dbgen.CreateUserParams{Issuer: "i", Subject: "s-" + t.Name(), Email: "a@x", Name: "A"})
	require.NoError(t, err)
	ts := pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}
	require.NoError(t, q.UpsertNote(ctx, dbgen.UpsertNoteParams{
		UserID: u.ID, ID: "n1", UpdatedAt: ts, Deleted: false, Data: []byte(`{"title":"n1"}`), ChangeSeq: 1,
	}))

	structured := []byte(`{"title":"Kickoff","tldr":"Resumo bom do kickoff.","points":["p1"],` +
		`"quote":null,"actions":[],"decisions":[],"questions":[],"speakerNames":{"others":[],"segments":[]}}`)
	runner := fakeEnrichRunner{structured: structured, partials: []string{`{"title":"Kic`, `koff",...`}}

	svc := New(Deps{
		Queries: q, Pool: pool, Redis: rdb, Runner: runner, Model: "sonnet",
		Workdir: t.TempDir(), Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	in := EnrichInput{Transcript: []Segment{{Index: 0, Speaker: "me", StartMs: 0, EndMs: 1000, Text: "oi"}}}
	inputJSON, err := json.Marshal(in)
	require.NoError(t, err)
	job, err := q.CreateAIJob(ctx, dbgen.CreateAIJobParams{UserID: u.ID, Kind: KindEnrich, NoteID: "n1", Model: "sonnet", Input: inputJSON})
	require.NoError(t, err)

	payload, err := json.Marshal(jobPayload{JobID: uuidString(job.ID)})
	require.NoError(t, err)
	require.NoError(t, svc.Handle(ctx, queue.Job{ID: "j1", Payload: payload}))

	// A entidade note_ai foi gravada com o resumo correto (não vazio).
	row, err := q.GetNoteAi(ctx, dbgen.GetNoteAiParams{UserID: u.ID, ID: "n1"})
	require.NoError(t, err)
	var saved NoteAi
	require.NoError(t, json.Unmarshal(row.Data, &saved))
	require.Equal(t, "Kickoff", saved.Title)
	require.Equal(t, "Resumo bom do kickoff.", saved.Tldr)

	// O evento final `done` da stream carrega o resultado canônico (igual ao salvo no banco).
	evs, err := stream.Read(ctx, rdb, uuidString(job.ID), "0", 500*time.Millisecond)
	require.NoError(t, err)
	require.NotEmpty(t, evs)
	last := evs[len(evs)-1]
	require.True(t, last.Final())
	require.Equal(t, stream.TypeDone, last.Type)
	require.NotEmpty(t, last.Data, "o done nunca vem vazio")
	var fromStream NoteAi
	require.NoError(t, json.Unmarshal([]byte(last.Data), &fromStream))
	require.Equal(t, "Kickoff", fromStream.Title)
}
