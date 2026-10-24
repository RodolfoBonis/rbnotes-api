package ai

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	dbgen "github.com/RodolfoBonis/rbnotes-api/db/gen"
	"github.com/RodolfoBonis/rbnotes-api/internal/platform/queue"
	"github.com/RodolfoBonis/rbnotes-api/internal/testsupport"
)

func TestValidateWeeklySummary(t *testing.T) {
	cand := map[string]bool{"w1": true}

	raw := []byte(`{"title":"Semana","summary":"resumo","highlights":["a",""],` +
		`"openTasks":[{"text":"x","noteId":"w1"},{"text":"y","noteId":"ghost"},{"text":"  ","noteId":""}],` +
		`"themes":[{"name":"foco","count":2}]}`)
	out, err := ValidateWeeklySummary(raw, cand)
	require.NoError(t, err)
	var wk WeeklySummaryResult
	require.NoError(t, json.Unmarshal(out, &wk))
	require.Equal(t, "Semana", wk.Title)
	require.Equal(t, []string{"a"}, wk.Highlights, "highlights vazios são removidos")
	require.Len(t, wk.OpenTasks, 2, "tarefa com texto em branco é descartada")
	for _, tk := range wk.OpenTasks {
		switch tk.Text {
		case "x":
			require.Equal(t, "w1", tk.NoteID)
		case "y":
			require.Empty(t, tk.NoteID, "noteId fora das candidatas é zerado")
		default:
			t.Fatalf("tarefa inesperada: %q", tk.Text)
		}
	}

	// title/summary obrigatórios.
	_, err = ValidateWeeklySummary([]byte(`{"title":"","summary":"r","highlights":[],"openTasks":[],"themes":[]}`), cand)
	require.ErrorIs(t, err, ErrInvalidOutput)

	// campo desconhecido é rejeitado (validação estrita).
	_, err = ValidateWeeklySummary([]byte(`{"title":"S","summary":"r","highlights":[],"openTasks":[],"themes":[],"x":1}`), cand)
	require.ErrorIs(t, err, ErrInvalidOutput)
}

// TestWeeklySummaryHandle exercita handleWeekly ponta a ponta (fake runner): o kind weekly_summary é
// aceito pelo banco (migração 00009), o resultado é persistido e as tarefas citam só notas conhecidas.
func TestWeeklySummaryHandle(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	rdb := testsupport.NewRedis(t)
	q := dbgen.New(pool)
	ctx := context.Background()

	u, err := q.CreateUser(ctx, dbgen.CreateUserParams{Issuer: "i", Subject: "s-" + t.Name(), Email: "a@x", Name: "A"})
	require.NoError(t, err)
	// Nota indexada → candidata da semana.
	require.NoError(t, q.UpsertNoteSearchEnrich(ctx, dbgen.UpsertNoteSearchEnrichParams{
		UserID: u.ID, NoteID: "w1", Title: "Planejamento", Tldr: "decidir o roadmap", Transcript: "falamos do roadmap",
	}))

	structured := []byte(`{"title":"Semana","summary":"resumo da semana","highlights":["marco"],` +
		`"openTasks":[{"text":"revisar roadmap","noteId":"w1"},{"text":"tarefa fantasma","noteId":"inexistente"}],` +
		`"themes":[{"name":"planejamento","count":2}]}`)
	svc := New(Deps{
		Queries: q, Pool: pool, Redis: rdb, Runner: fakeEnrichRunner{structured: structured}, Model: "haiku",
		Workdir: t.TempDir(), Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	in := WeeklyInput{WeekStart: "2026-10-19", TZ: "UTC"}
	inputJSON, err := json.Marshal(in)
	require.NoError(t, err)
	job, err := q.CreateAIJob(ctx, dbgen.CreateAIJobParams{UserID: u.ID, Kind: KindWeeklySummary, Model: "haiku", Input: inputJSON})
	require.NoError(t, err)
	payload, err := json.Marshal(jobPayload{JobID: uuidString(job.ID)})
	require.NoError(t, err)

	require.NoError(t, svc.Handle(ctx, queue.Job{ID: "j1", Payload: payload}))

	got, err := q.GetAIJob(ctx, job.ID)
	require.NoError(t, err)
	require.Equal(t, "done", got.Status)
	var wk WeeklySummaryResult
	require.NoError(t, json.Unmarshal(got.Result, &wk))
	require.Equal(t, "Semana", wk.Title)
	require.Len(t, wk.OpenTasks, 2)
	for _, tk := range wk.OpenTasks {
		if tk.Text == "tarefa fantasma" {
			require.Empty(t, tk.NoteID, "id inventado é descartado")
		}
		if tk.Text == "revisar roadmap" {
			require.Equal(t, "w1", tk.NoteID)
		}
	}
}

// TestRequestWeeklySummaryValidation cobre a validação de entrada do pedido (weekStart e tz IANA).
func TestRequestWeeklySummaryValidation(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	q := dbgen.New(pool)
	ctx := context.Background()
	u, err := q.CreateUser(ctx, dbgen.CreateUserParams{Issuer: "i", Subject: "s-" + t.Name(), Email: "a@x", Name: "A"})
	require.NoError(t, err)
	svc := New(Deps{
		Queries: q, Pool: pool, Queue: queue.NewPostgres(pool, queue.DefaultBackoff),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	_, err = svc.RequestWeeklySummary(ctx, u.ID, WeeklyInput{WeekStart: "20/10/2026"})
	require.ErrorIs(t, err, ErrBadInput)
	_, err = svc.RequestWeeklySummary(ctx, u.ID, WeeklyInput{WeekStart: "2026-10-19", TZ: "Marte/Olympus"})
	require.ErrorIs(t, err, ErrBadInput)

	// weekStart válido + tz vazio (vira UTC) → aceito e cria o job com kind weekly_summary.
	job, err := svc.RequestWeeklySummary(ctx, u.ID, WeeklyInput{WeekStart: "2026-10-19"})
	require.NoError(t, err)
	got, err := q.GetAIJob(ctx, job.ID)
	require.NoError(t, err)
	require.Equal(t, KindWeeklySummary, got.Kind)
}
