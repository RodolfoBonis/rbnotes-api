package ai

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/require"

	dbgen "github.com/RodolfoBonis/rbnotes-api/db/gen"
	"github.com/RodolfoBonis/rbnotes-api/internal/ai/claude"
	"github.com/RodolfoBonis/rbnotes-api/internal/ai/stream"
	"github.com/RodolfoBonis/rbnotes-api/internal/platform/queue"
	"github.com/RodolfoBonis/rbnotes-api/internal/testsupport"
)

const leakSecret = "ConteudoSecretoDaNotaCifrada"

// errRunner devolve um erro de modelo (res.Err) — exercita o caminho fail.
type errRunner struct{ msg string }

func (r errRunner) Run(context.Context, claude.Request, func(claude.Event)) (claude.Result, error) {
	return claude.Result{Err: r.msg}, nil
}

// rlRunner sinaliza limite de uso — exercita o caminho rateLimited.
type rlRunner struct{}

func (rlRunner) Run(context.Context, claude.Request, func(claude.Event)) (claude.Result, error) {
	return claude.Result{RateLimited: true, Err: "limite de uso atingido"}, nil
}

// fakeChatRunner responde o chat com uma saída estruturada fixa, registra o workdir/HOME recebidos e
// o prompt, e SIMULA a CLI gravando a sessão em $HOME/.claude/projects e arquivos no cwd (para o teste
// provar que a limpeza apaga tudo).
type fakeChatRunner struct {
	structured []byte
	sessionID  string
	mu         sync.Mutex
	gotWorkdir string
	gotHome    string
	gotPrompt  string
}

func (f *fakeChatRunner) Run(_ context.Context, req claude.Request, _ func(claude.Event)) (claude.Result, error) {
	f.mu.Lock()
	f.gotWorkdir = req.Workdir
	f.gotHome = req.HomeDir
	f.gotPrompt = req.Prompt
	f.mu.Unlock()
	if req.HomeDir != "" {
		dir := filepath.Join(req.HomeDir, ".claude", "projects", "x")
		_ = os.MkdirAll(dir, 0o750)
		_ = os.WriteFile(filepath.Join(dir, "session.jsonl"), []byte(leakSecret), 0o600)
	}
	if req.Workdir != "" {
		_ = os.WriteFile(filepath.Join(req.Workdir, "scratch.txt"), []byte(leakSecret), 0o600)
	}
	return claude.Result{Structured: f.structured, SessionID: f.sessionID}, nil
}

func newLeakSvc(t *testing.T, runner Runner) (*Service, *dbgen.Queries, pgtype.UUID, string) {
	t.Helper()
	pool := testsupport.NewPostgres(t)
	rdb := testsupport.NewRedis(t)
	q := dbgen.New(pool)
	u, err := q.CreateUser(context.Background(), dbgen.CreateUserParams{Issuer: "i", Subject: "s-" + t.Name(), Email: "a@x", Name: "A"})
	require.NoError(t, err)
	workdir := t.TempDir()
	svc := New(Deps{
		Queries: q, Pool: pool, Queue: queue.NewPostgres(pool, queue.DefaultBackoff), Redis: rdb, Runner: runner,
		Model: "haiku", Workdir: workdir, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return svc, q, u.ID, workdir
}

// ephemeralEnrichInput monta um input de enrich CIFRADO com o segredo na transcrição.
func ephemeralEnrichInput(t *testing.T) []byte {
	t.Helper()
	in := EnrichInput{
		Transcript: []Segment{{Index: 0, Speaker: "me", StartMs: 0, EndMs: 1000, Text: leakSecret}},
		Ephemeral:  true,
	}
	b, err := json.Marshal(in)
	require.NoError(t, err)
	return b
}

// requireNoClearText confere que input/result do job não guardam o segredo (ambos {}).
func requireNoClearText(ctx context.Context, t *testing.T, q *dbgen.Queries, id pgtype.UUID) {
	t.Helper()
	got, err := q.GetAIJob(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "error", got.Status)
	require.JSONEq(t, "{}", string(got.Input), "input (transcrição) foi limpo")
	require.JSONEq(t, "{}", string(got.Result), "result foi limpo")
	require.NotContains(t, string(got.Input)+string(got.Result)+got.Error, leakSecret)
}

// A6 — fail de job efêmero limpa input/result (antes só chamava FailAIJob e deixava a transcrição).
func TestFailEphemeralClearsInput(t *testing.T) {
	ctx := context.Background()
	svc, q, uid, _ := newLeakSvc(t, errRunner{msg: "modelo explodiu"})
	job, err := q.CreateAIJob(ctx, dbgen.CreateAIJobParams{UserID: uid, Kind: KindEnrich, NoteID: "n1", Model: "haiku", Input: ephemeralEnrichInput(t)})
	require.NoError(t, err)
	payload, err := json.Marshal(jobPayload{JobID: uuidString(job.ID)})
	require.NoError(t, err)

	err = svc.Handle(ctx, queue.Job{ID: "j", Payload: payload})
	require.Error(t, err)
	require.True(t, queue.IsPermanent(err))
	requireNoClearText(ctx, t, q, job.ID)
}

// A6 — giveUp de job efêmero limpa input/result.
func TestGiveUpEphemeralClearsInput(t *testing.T) {
	ctx := context.Background()
	svc, q, uid, _ := newLeakSvc(t, errRunner{})
	job, err := q.CreateAIJob(ctx, dbgen.CreateAIJobParams{UserID: uid, Kind: KindEnrich, NoteID: "n1", Model: "haiku", Input: ephemeralEnrichInput(t)})
	require.NoError(t, err)
	payload, err := json.Marshal(jobPayload{JobID: uuidString(job.ID)})
	require.NoError(t, err)

	svc.giveUp(ctx, queue.Job{ID: "j", Payload: payload})
	requireNoClearText(ctx, t, q, job.ID)
}

// A6 — o reaper limpa input/result de um job efêmero preso (e preserva o de um não-efêmero).
func TestReaperEphemeralClearsInput(t *testing.T) {
	ctx := context.Background()
	svc, q, uid, _ := newLeakSvc(t, errRunner{})

	eph, err := q.CreateAIJob(ctx, dbgen.CreateAIJobParams{UserID: uid, Kind: KindEnrich, NoteID: "n1", Model: "haiku", Input: ephemeralEnrichInput(t)})
	require.NoError(t, err)
	plain, err := q.CreateAIJob(ctx, dbgen.CreateAIJobParams{UserID: uid, Kind: KindAsk, Model: "haiku", Input: []byte(`{"question":"` + leakSecret + `"}`)})
	require.NoError(t, err)
	_, err = svc.pool.Exec(ctx, "UPDATE ai_jobs SET updated_at = now() - interval '20 minutes'")
	require.NoError(t, err)

	n, err := svc.ReapStuckJobs(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, n)

	requireNoClearText(ctx, t, q, eph.ID)

	// Não-efêmero: o reaper marca erro mas PRESERVA o input (só a retenção o limpa depois).
	gotPlain, err := q.GetAIJob(ctx, plain.ID)
	require.NoError(t, err)
	require.Equal(t, "error", gotPlain.Status)
	require.Contains(t, string(gotPlain.Input), leakSecret, "input de não-efêmero é preservado pelo reaper")
}

// A6 — limite de uso num job efêmero NÃO reagenda (manteria a transcrição em claro durante a espera):
// limpa input/result e falha em definitivo (permanente), para o worker não requeue.
func TestRateLimitEphemeralClearsAndIsPermanent(t *testing.T) {
	ctx := context.Background()
	svc, q, uid, _ := newLeakSvc(t, rlRunner{})
	job, err := q.CreateAIJob(ctx, dbgen.CreateAIJobParams{UserID: uid, Kind: KindEnrich, NoteID: "n1", Model: "haiku", Input: ephemeralEnrichInput(t)})
	require.NoError(t, err)
	payload, err := json.Marshal(jobPayload{JobID: uuidString(job.ID)})
	require.NoError(t, err)

	err = svc.Handle(ctx, queue.Job{ID: "j", Payload: payload})
	require.Error(t, err)
	require.True(t, queue.IsPermanent(err), "efêmero não reagenda no limite de uso")
	_, ok := queue.RetryAfterTime(err)
	require.False(t, ok, "não devolve RetryAfter (não vira requeue)")
	requireNoClearText(ctx, t, q, job.ID)
}

// A6 — a retenção (Cleanup) é rede de segurança: limpa input de QUALQUER efêmero terminal, mesmo
// recente, caso algum caminho tenha escapado e deixado texto claro.
func TestCleanupClearsEphemeralEvenRecent(t *testing.T) {
	ctx := context.Background()
	svc, q, uid, _ := newLeakSvc(t, errRunner{})
	// Simula um efêmero terminal que escapou com texto claro no input (updated_at recente).
	job, err := q.CreateAIJob(ctx, dbgen.CreateAIJobParams{UserID: uid, Kind: KindEnrich, NoteID: "n1", Model: "haiku", Input: ephemeralEnrichInput(t)})
	require.NoError(t, err)
	_, err = svc.pool.Exec(ctx, "UPDATE ai_jobs SET status = 'error', result = '{\"r\":1}'::jsonb WHERE id = $1", job.ID)
	require.NoError(t, err)

	require.NoError(t, svc.Cleanup(ctx))

	got, err := q.GetAIJob(ctx, job.ID)
	require.NoError(t, err)
	require.JSONEq(t, "{}", string(got.Input))
	require.JSONEq(t, "{}", string(got.Result))
}

// A5 — a stream de um enrich efêmero expira em ~60s (não em 1h), com o resultado só no `done`.
func TestEnrichEphemeralStreamTTLIsShort(t *testing.T) {
	ctx := context.Background()
	structured := []byte(`{"title":"` + leakSecret + `","tldr":"t","points":["p"],` +
		`"quote":null,"actions":[],"decisions":[],"questions":[],"speakerNames":{"others":[],"segments":[]}}`)
	svc, q, uid, _ := newLeakSvc(t, fakeEnrichRunner{structured: structured, partials: []string{`{"title":"` + leakSecret}})
	ts := pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}
	require.NoError(t, q.UpsertNote(ctx, dbgen.UpsertNoteParams{UserID: uid, ID: "n1", UpdatedAt: ts, Deleted: false, Data: []byte(`{"enc":"v1"}`), ChangeSeq: 1}))

	job, err := q.CreateAIJob(ctx, dbgen.CreateAIJobParams{UserID: uid, Kind: KindEnrich, NoteID: "n1", Model: "haiku", Input: ephemeralEnrichInput(t)})
	require.NoError(t, err)
	payload, err := json.Marshal(jobPayload{JobID: uuidString(job.ID)})
	require.NoError(t, err)
	require.NoError(t, svc.Handle(ctx, queue.Job{ID: "j", Payload: payload}))

	ttl, err := svc.rdb.TTL(ctx, stream.Key(uuidString(job.ID))).Result()
	require.NoError(t, err)
	require.Greater(t, ttl, time.Duration(0))
	require.LessOrEqual(t, ttl, 60*time.Second, "stream efêmera expira em ~60s")

	// O resultado (texto claro) só existe no `done` do SSE; não ficou no ai_jobs.
	got, err := q.GetAIJob(ctx, job.ID)
	require.NoError(t, err)
	require.JSONEq(t, "{}", string(got.Result))
	require.JSONEq(t, "{}", string(got.Input))
}

// M8 — o chat efêmero não persiste sessão, não usa --resume, roda em workdir/HOME temporários e NÃO
// deixa nenhum arquivo para trás (nem a sessão da CLI no HOME). O histórico do cliente vai no prompt.
func TestChatEphemeralLeavesNoFilesNorSession(t *testing.T) {
	ctx := context.Background()
	runner := &fakeChatRunner{structured: []byte(`{"text":"resposta curta","cites":[]}`), sessionID: "sess-123"}
	svc, q, uid, workdir := newLeakSvc(t, runner)

	in := ChatInput{
		ThreadID:  "t1",
		Message:   "minha pergunta " + leakSecret,
		History:   []ChatTurn{{Role: "user", Content: "oi " + leakSecret}, {Role: "assistant", Content: "olá"}},
		Ephemeral: true,
	}
	inputJSON, err := json.Marshal(in)
	require.NoError(t, err)
	job, err := q.CreateAIJob(ctx, dbgen.CreateAIJobParams{UserID: uid, Kind: KindChat, ThreadID: "t1", Model: "haiku", Input: inputJSON})
	require.NoError(t, err)
	payload, err := json.Marshal(jobPayload{JobID: uuidString(job.ID)})
	require.NoError(t, err)

	require.NoError(t, svc.Handle(ctx, queue.Job{ID: "j", Payload: payload}))

	// 1) Nenhuma sessão persistida (mesmo com o runner devolvendo SessionID) e sem --resume.
	_, serr := q.GetThreadSession(ctx, dbgen.GetThreadSessionParams{UserID: uid, ThreadID: "t1"})
	require.ErrorIs(t, serr, pgx.ErrNoRows, "chat efêmero não grava sessão")

	// 2) O runner recebeu workdir/HOME temporários sob AI_WORKDIR/tmp, SEM --resume.
	require.NotEmpty(t, runner.gotWorkdir)
	require.NotEmpty(t, runner.gotHome)
	require.True(t, strings.HasPrefix(runner.gotWorkdir, filepath.Join(workdir, "tmp")))
	require.True(t, strings.HasPrefix(runner.gotHome, filepath.Join(workdir, "tmp")))
	require.Contains(t, runner.gotPrompt, "oi "+leakSecret, "histórico do cliente entra no prompt")

	// 3) Os diretórios temporários (e os arquivos da CLI dentro deles) foram apagados.
	require.NoDirExists(t, runner.gotWorkdir)
	require.NoDirExists(t, runner.gotHome)
	entries, err := os.ReadDir(filepath.Join(workdir, "tmp"))
	require.NoError(t, err)
	require.Empty(t, entries, "AI_WORKDIR/tmp fica vazio após o chat efêmero")

	// 4) input/result limpos; resultado só no `done` do SSE.
	got, err := q.GetAIJob(ctx, job.ID)
	require.NoError(t, err)
	require.Equal(t, "done", got.Status)
	require.JSONEq(t, "{}", string(got.Input))
	require.JSONEq(t, "{}", string(got.Result))

	evs, err := stream.Read(ctx, svc.rdb, uuidString(job.ID), "0", 500*time.Millisecond)
	require.NoError(t, err)
	require.NotEmpty(t, evs)
	require.Equal(t, stream.TypeDone, evs[len(evs)-1].Type)
	require.Contains(t, evs[len(evs)-1].Data, "resposta curta")
}
