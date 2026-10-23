package ai

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/require"

	dbgen "github.com/RodolfoBonis/rbnotes-api/db/gen"
	"github.com/RodolfoBonis/rbnotes-api/internal/platform/queue"
	"github.com/RodolfoBonis/rbnotes-api/internal/testsupport"
)

func newJobsSvc(t *testing.T, dailyLimit int, retention time.Duration) (*Service, *dbgen.Queries, pgtype.UUID) {
	t.Helper()
	pool := testsupport.NewPostgres(t)
	q := dbgen.New(pool)
	u, err := q.CreateUser(context.Background(), dbgen.CreateUserParams{Issuer: "i", Subject: "s-" + t.Name(), Email: "a@x", Name: "A"})
	require.NoError(t, err)
	svc := New(Deps{
		Queries: q, Pool: pool, Queue: queue.NewPostgres(pool, queue.DefaultBackoff),
		DailyLimit: dailyLimit, Retention: retention, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return svc, q, u.ID
}

// TestCheckQuotaAtomicUnderConcurrency: com teto 3, 8 pedidos simultâneos do mesmo usuário geram
// exatamente 3 aceitos — o lock por usuário impede furar a cota.
func TestCheckQuotaAtomicUnderConcurrency(t *testing.T) {
	svc, q, uid := newJobsSvc(t, 3, time.Hour)
	ctx := context.Background()

	var mu sync.Mutex
	ok, limited := 0, 0
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := svc.RequestAsk(ctx, uid, AskInput{Question: "oi"})
			mu.Lock()
			switch {
			case err == nil:
				ok++
			case err == ErrDailyLimit:
				limited++
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	require.Equal(t, 3, ok, "exatamente o teto é aceito")
	require.Equal(t, 5, limited)

	n, err := q.CountAIRequestsSince(ctx, dbgen.CountAIRequestsSinceParams{UserID: uid, CreatedAt: pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true}})
	require.NoError(t, err)
	require.Equal(t, int64(3), n, "nenhum pedido além do teto foi registrado")
}

// TestRequestAtomicCreatesJobAndQueue: um pedido cria o ai_job E o job na fila (atômico).
func TestRequestAtomicCreatesJobAndQueue(t *testing.T) {
	svc, q, uid := newJobsSvc(t, 0, time.Hour)
	ctx := context.Background()
	job, err := svc.RequestAsk(ctx, uid, AskInput{Question: "oi"})
	require.NoError(t, err)

	got, err := q.GetAIJob(ctx, job.ID)
	require.NoError(t, err)
	require.Equal(t, "queued", got.Status)

	claimed, err := svc.queue.Claim(ctx, time.Minute, queueKind)
	require.NoError(t, err)
	require.NotNil(t, claimed, "há um job correspondente na fila")
}

// TestReapStuckJobs marca como erro um ai_job preso sem job vivo na fila.
func TestReapStuckJobs(t *testing.T) {
	svc, q, uid := newJobsSvc(t, 0, time.Hour)
	ctx := context.Background()
	job, err := q.CreateAIJob(ctx, dbgen.CreateAIJobParams{UserID: uid, Kind: "ask", Input: []byte("{}")})
	require.NoError(t, err)
	// Envelhece o job além do teto do reaper (sem job na fila).
	_, err = svc.pool.Exec(ctx, "UPDATE ai_jobs SET updated_at = now() - interval '20 minutes' WHERE id = $1", job.ID)
	require.NoError(t, err)

	n, err := svc.ReapStuckJobs(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	got, err := q.GetAIJob(ctx, job.ID)
	require.NoError(t, err)
	require.Equal(t, "error", got.Status)
	require.NotEmpty(t, got.Error)
}

// TestCleanupRetention purga ai_requests antigos e esvazia input/result de ai_jobs antigos.
func TestCleanupRetention(t *testing.T) {
	svc, q, uid := newJobsSvc(t, 0, time.Hour)
	ctx := context.Background()

	require.NoError(t, q.InsertAIRequest(ctx, dbgen.InsertAIRequestParams{UserID: uid, Kind: "ask"}))
	_, err := svc.pool.Exec(ctx, "UPDATE ai_requests SET created_at = now() - interval '2 hours' WHERE user_id = $1", uid)
	require.NoError(t, err)

	job, err := q.CreateAIJob(ctx, dbgen.CreateAIJobParams{UserID: uid, Kind: "ask", Input: []byte(`{"q":"grande"}`)})
	require.NoError(t, err)
	_, err = svc.pool.Exec(ctx, "UPDATE ai_jobs SET result = '{\"r\":1}'::jsonb, updated_at = now() - interval '2 hours' WHERE id = $1", job.ID)
	require.NoError(t, err)

	require.NoError(t, svc.Cleanup(ctx))

	n, err := q.CountAIRequestsSince(ctx, dbgen.CountAIRequestsSinceParams{UserID: uid, CreatedAt: pgtype.Timestamptz{Time: time.Now().Add(-24 * time.Hour), Valid: true}})
	require.NoError(t, err)
	require.Equal(t, int64(0), n, "ai_requests antigos foram apagados")

	got, err := q.GetAIJob(ctx, job.ID)
	require.NoError(t, err)
	require.JSONEq(t, "{}", string(got.Input), "input antigo foi esvaziado")
	require.Nil(t, got.Result, "result antigo foi removido")
}
