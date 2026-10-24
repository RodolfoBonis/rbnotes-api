package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	dbgen "github.com/RodolfoBonis/rbnotes-api/db/gen"
	"github.com/RodolfoBonis/rbnotes-api/internal/ai"
	"github.com/RodolfoBonis/rbnotes-api/internal/ai/stream"
	"github.com/RodolfoBonis/rbnotes-api/internal/testsupport"
)

// newAIHandlers monta Handlers com Postgres e Redis reais e a IA ligada (sem worker/CLI: os testes
// publicam direto na stream ou semeiam o ai_jobs). Devolve também o usuário do teste.
func newAIHandlers(t *testing.T) (*Handlers, dbgen.User, *dbgen.Queries) {
	t.Helper()
	pool := testsupport.NewPostgres(t)
	rdb := testsupport.NewRedis(t)
	q := dbgen.New(pool)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := ai.New(ai.Deps{Queries: q, Pool: pool, Redis: rdb, Logger: logger})
	h := NewHandlers(HandlerDeps{Queries: q, Pool: pool, AI: svc, Redis: rdb, Logger: logger})
	u, err := q.CreateUser(context.Background(), dbgen.CreateUserParams{Issuer: "dev", Subject: "s-" + t.Name(), Email: "a@x", Name: "A"})
	require.NoError(t, err)
	return h, u, q
}

func seedAIJob(t *testing.T, q *dbgen.Queries, u dbgen.User, status string, result []byte, errMsg string) dbgen.AiJob {
	t.Helper()
	ctx := context.Background()
	job, err := q.CreateAIJob(ctx, dbgen.CreateAIJobParams{
		UserID: u.ID, Kind: ai.KindEnrich, NoteID: "n1", Model: "sonnet", Input: []byte(`{}`),
	})
	require.NoError(t, err)
	switch status {
	case "done":
		require.NoError(t, q.CompleteAIJob(ctx, dbgen.CompleteAIJobParams{ID: job.ID, Result: result, Model: "sonnet"}))
	case "error":
		require.NoError(t, q.FailAIJob(ctx, dbgen.FailAIJobParams{ID: job.ID, Error: errMsg}))
	}
	got, err := q.GetAIJob(ctx, job.ID)
	require.NoError(t, err)
	return got
}

// reqWithJob monta um *http.Request autenticado (usuário no contexto) com o id na rota do chi.
func reqWithJob(u dbgen.User, id string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/v1/ai/streams/"+id, nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", id)
	ctx := context.WithValue(r.Context(), chi.RouteCtxKey, rctx)
	ctx = withAppUser(ctx, u)
	return r.WithContext(ctx)
}

// TestStreamFinalStateCarriesResult: quando a stream do Redis já expirou, o estado final de um job
// concluído traz o RESULTADO canônico (antes vinha vazio — a causa do note_ai {} no desktop).
func TestStreamFinalStateCarriesResult(t *testing.T) {
	h, u, q := newAIHandlers(t)
	result := []byte(`{"title":"Kickoff","tldr":"Resumo bom","points":["a"]}`)
	job := seedAIJob(t, q, u, "done", result, "")

	rec := httptest.NewRecorder()
	final, ok := h.streamFinalState(rec, reqWithJob(u, uuidStr(job.ID)), job.ID)
	require.True(t, ok)
	require.NotNil(t, final)
	require.Equal(t, stream.TypeDone, final.typ)
	require.JSONEq(t, string(result), final.data, "o done final carrega o resultado do banco (JSONB normaliza a formatação)")

	// Job com erro: o final traz a mensagem de erro.
	ejob := seedAIJob(t, q, u, "error", nil, "A IA não conseguiu responder.")
	final, ok = h.streamFinalState(rec, reqWithJob(u, uuidStr(ejob.ID)), ejob.ID)
	require.True(t, ok)
	require.NotNil(t, final)
	require.Equal(t, stream.TypeError, final.typ)
	require.Equal(t, "A IA não conseguiu responder.", final.data)
}

// TestAIStreamEmitsEnvelopedDone: o SSE emite o evento `done` com o resultado embrulhado no envelope
// `{"data":...}`, lendo da stream do Redis (caminho normal, resposta ainda quente).
func TestAIStreamEmitsEnvelopedDone(t *testing.T) {
	h, u, q := newAIHandlers(t)
	result := []byte(`{"title":"Kickoff","tldr":"Resumo bom"}`)
	job := seedAIJob(t, q, u, "done", result, "")

	// Semeia a stream do Redis com alguns json parciais + o done com o resultado completo.
	ctx := context.Background()
	pub := stream.NewPublisher(ctx, h.rdb, uuidStr(job.ID))
	pub.Emit(ctx, stream.TypeJSON, `{"title":"Kic`)
	pub.Emit(ctx, stream.TypeJSON, `koff"}`)
	require.NoError(t, pub.Close(ctx, stream.TypeDone, string(result)))

	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { h.aiStream(rec, reqWithJob(u, uuidStr(job.ID))); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("aiStream não encerrou")
	}

	body := rec.Body.String()
	require.Contains(t, body, "event: done")
	// O done vem embrulhado no envelope com o resultado como string JSON escapada.
	require.Contains(t, body, `data: {"data":"{\"title\":\"Kickoff\",\"tldr\":\"Resumo bom\"}"}`)
}
