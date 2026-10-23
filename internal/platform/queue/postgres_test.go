package queue_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/RodolfoBonis/rbnotes-api/internal/platform/queue"
	"github.com/RodolfoBonis/rbnotes-api/internal/testsupport"
)

func TestEnqueueClaimComplete(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	q := queue.NewPostgres(pool, queue.DefaultBackoff)
	ctx := context.Background()

	job, enq, err := q.Enqueue(ctx, queue.EnqueueParams{Kind: "sync_platform", Payload: map[string]string{"platform": "steam"}})
	require.NoError(t, err)
	require.True(t, enq)
	require.NotEmpty(t, job.ID)

	claimed, err := q.Claim(ctx, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, claimed)
	require.Equal(t, job.ID, claimed.ID)
	require.Equal(t, 1, claimed.Attempts)
	require.NotEmpty(t, claimed.LeaseToken, "claim deve gerar um lease_token")
	require.JSONEq(t, `{"platform":"steam"}`, string(claimed.Payload))

	// Nada mais elegível (o único job está running).
	none, err := q.Claim(ctx, time.Minute)
	require.NoError(t, err)
	require.Nil(t, none)

	require.NoError(t, q.Complete(ctx, claimed.ID, claimed.LeaseToken))
	after, err := q.Claim(ctx, time.Minute)
	require.NoError(t, err)
	require.Nil(t, after)
}

func TestDedupeSuppressesDuplicate(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	q := queue.NewPostgres(pool, queue.DefaultBackoff)
	ctx := context.Background()

	_, enq1, err := q.Enqueue(ctx, queue.EnqueueParams{Kind: "sync_game", DedupeKey: "game:1"})
	require.NoError(t, err)
	require.True(t, enq1)

	job2, enq2, err := q.Enqueue(ctx, queue.EnqueueParams{Kind: "sync_game", DedupeKey: "game:1"})
	require.NoError(t, err)
	require.False(t, enq2, "segundo enqueue idêntico deve ser suprimido")
	require.Nil(t, job2)

	// Após concluir, um novo enqueue com a mesma chave é permitido.
	claimed, err := q.Claim(ctx, time.Minute)
	require.NoError(t, err)
	require.NoError(t, q.Complete(ctx, claimed.ID, claimed.LeaseToken))

	_, enq3, err := q.Enqueue(ctx, queue.EnqueueParams{Kind: "sync_game", DedupeKey: "game:1"})
	require.NoError(t, err)
	require.True(t, enq3)
}

func TestFailRetriesThenDead(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	// Backoff mínimo para o job voltar a ficar elegível de imediato no teste.
	q := queue.NewPostgres(pool, queue.BackoffConfig{Base: time.Nanosecond, Max: time.Nanosecond, Jitter: 0})
	ctx := context.Background()

	_, _, err := q.Enqueue(ctx, queue.EnqueueParams{Kind: "sync_game", MaxAttempts: 2})
	require.NoError(t, err)

	// Tentativa 1 → falha → reagenda.
	c1, err := q.Claim(ctx, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, c1)
	require.NoError(t, q.Fail(ctx, c1.ID, c1.LeaseToken, errors.New("boom")))

	time.Sleep(10 * time.Millisecond)

	// Tentativa 2 (== max) → falha → dead.
	c2, err := q.Claim(ctx, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, c2)
	require.Equal(t, 2, c2.Attempts)
	require.NoError(t, q.Fail(ctx, c2.ID, c2.LeaseToken, errors.New("boom again")))

	// Não deve mais ser reivindicável.
	time.Sleep(10 * time.Millisecond)
	dead, err := q.Claim(ctx, time.Minute)
	require.NoError(t, err)
	require.Nil(t, dead)

	var status string
	require.NoError(t, pool.QueryRow(ctx, `SELECT status FROM jobs WHERE id=$1`, c2.ID).Scan(&status))
	require.Equal(t, "dead", status)
}

func TestClaimReclaimsExpiredLease(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	q := queue.NewPostgres(pool, queue.DefaultBackoff)
	ctx := context.Background()

	_, _, err := q.Enqueue(ctx, queue.EnqueueParams{Kind: "sync_game"})
	require.NoError(t, err)

	// Lease curtíssimo: expira antes do próximo claim.
	c1, err := q.Claim(ctx, 1*time.Millisecond)
	require.NoError(t, err)
	require.NotNil(t, c1)

	time.Sleep(20 * time.Millisecond)

	c2, err := q.Claim(ctx, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, c2, "job com lease expirado deve ser reivindicável")
	require.Equal(t, c1.ID, c2.ID)
	require.Equal(t, 2, c2.Attempts)
	require.NotEqual(t, c1.LeaseToken, c2.LeaseToken, "o reclaim gera um novo lease_token")
}

// TestLeaseOwnership garante que Complete/Fail só valem para o dono atual do lease: um worker com
// lease expirado (re-reivindicado por outro) recebe ErrLeaseLost e não processa duas vezes.
func TestLeaseOwnership(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	q := queue.NewPostgres(pool, queue.DefaultBackoff)
	ctx := context.Background()

	_, _, err := q.Enqueue(ctx, queue.EnqueueParams{Kind: "sync_game"})
	require.NoError(t, err)

	c1, err := q.Claim(ctx, 1*time.Millisecond) // lease curtíssimo
	require.NoError(t, err)
	require.NotNil(t, c1)

	time.Sleep(20 * time.Millisecond)

	c2, err := q.Claim(ctx, time.Minute) // outro worker reivindica
	require.NoError(t, err)
	require.NotNil(t, c2)
	require.Equal(t, c1.ID, c2.ID)

	// O worker "zumbi" (token antigo) não consegue completar nem falhar.
	require.ErrorIs(t, q.Complete(ctx, c1.ID, c1.LeaseToken), queue.ErrLeaseLost)
	require.ErrorIs(t, q.Fail(ctx, c1.ID, c1.LeaseToken, errors.New("zumbi")), queue.ErrLeaseLost)

	// O dono atual completa normalmente.
	require.NoError(t, q.Complete(ctx, c2.ID, c2.LeaseToken))
}

// TestPoisonJobGoesDead garante que um job com lease expirado e tentativas esgotadas vai para
// 'dead' no próximo Claim, em vez de ser reivindicado de novo (loop de job envenenado).
func TestPoisonJobGoesDead(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	q := queue.NewPostgres(pool, queue.DefaultBackoff)
	ctx := context.Background()

	_, _, err := q.Enqueue(ctx, queue.EnqueueParams{Kind: "sync_game", MaxAttempts: 1})
	require.NoError(t, err)

	c1, err := q.Claim(ctx, 1*time.Millisecond) // attempts=1 == max; lease curtíssimo
	require.NoError(t, err)
	require.NotNil(t, c1)
	require.Equal(t, 1, c1.Attempts)

	time.Sleep(20 * time.Millisecond) // lease expira; worker "morreu" sem completar

	next, err := q.Claim(ctx, time.Minute)
	require.NoError(t, err)
	require.Nil(t, next, "job envenenado não deve ser reivindicável de novo")

	var status string
	require.NoError(t, pool.QueryRow(ctx, `SELECT status FROM jobs WHERE id=$1`, c1.ID).Scan(&status))
	require.Equal(t, "dead", status)
}

// TestPermanentErrorGoesDead garante que um erro Permanent mata o job na hora (sem consumir todas
// as tentativas).
func TestPermanentErrorGoesDead(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	q := queue.NewPostgres(pool, queue.DefaultBackoff)
	ctx := context.Background()

	_, _, err := q.Enqueue(ctx, queue.EnqueueParams{Kind: "sync_game", MaxAttempts: 5})
	require.NoError(t, err)

	c1, err := q.Claim(ctx, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, c1)
	require.Equal(t, 1, c1.Attempts, "primeira tentativa")

	require.NoError(t, q.Fail(ctx, c1.ID, c1.LeaseToken, queue.Permanent(errors.New("credencial inválida"))))

	var status string
	require.NoError(t, pool.QueryRow(ctx, `SELECT status FROM jobs WHERE id=$1`, c1.ID).Scan(&status))
	require.Equal(t, "dead", status, "erro permanente mata o job mesmo com tentativas restantes")
}

func TestCleanupRemovesOldTerminalJobs(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	q := queue.NewPostgres(pool, queue.DefaultBackoff)
	ctx := context.Background()

	_, _, err := q.Enqueue(ctx, queue.EnqueueParams{Kind: "sync_game"})
	require.NoError(t, err)
	c1, err := q.Claim(ctx, time.Minute)
	require.NoError(t, err)
	require.NoError(t, q.Complete(ctx, c1.ID, c1.LeaseToken))

	// Envelhece o job terminal para antes da janela de retenção.
	_, err = pool.Exec(ctx, `UPDATE jobs SET updated_at = now() - interval '8 days' WHERE id=$1`, c1.ID)
	require.NoError(t, err)

	// Um dead da mesma idade fica (retenção maior); um dead bem velho sai.
	_, _, err = q.Enqueue(ctx, queue.EnqueueParams{Kind: "sync_game"})
	require.NoError(t, err)
	d1, err := q.Claim(ctx, time.Minute)
	require.NoError(t, err)
	require.NoError(t, q.Fail(ctx, d1.ID, d1.LeaseToken, queue.Permanent(errors.New("x"))))
	_, err = pool.Exec(ctx, `UPDATE jobs SET updated_at = now() - interval '8 days' WHERE id=$1`, d1.ID)
	require.NoError(t, err)

	n, err := q.Cleanup(ctx, 7*24*time.Hour, 30*24*time.Hour)
	require.NoError(t, err)
	require.Equal(t, int64(1), n)

	var count int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE id=$1`, c1.ID).Scan(&count))
	require.Equal(t, 0, count)
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE id=$1`, d1.ID).Scan(&count))
	require.Equal(t, 1, count, "dead de 8 dias ainda fica")

	_, err = pool.Exec(ctx, `UPDATE jobs SET updated_at = now() - interval '31 days' WHERE id=$1`, d1.ID)
	require.NoError(t, err)
	n, err = q.Cleanup(ctx, 7*24*time.Hour, 30*24*time.Hour)
	require.NoError(t, err)
	require.Equal(t, int64(1), n)
}

func TestRetryDeadJob(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	q := queue.NewPostgres(pool, queue.DefaultBackoff)
	ctx := context.Background()

	_, _, err := q.Enqueue(ctx, queue.EnqueueParams{Kind: "sync_game"})
	require.NoError(t, err)
	j, err := q.Claim(ctx, time.Minute)
	require.NoError(t, err)
	require.ErrorIs(t, q.Retry(ctx, j.ID), queue.ErrNotRetryable, "running não reprocessa")
	require.NoError(t, q.Fail(ctx, j.ID, j.LeaseToken, queue.Permanent(errors.New("perfil privado"))))
	require.NoError(t, q.Retry(ctx, j.ID))
	again, err := q.Claim(ctx, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, again)
	require.Equal(t, j.ID, again.ID)
	require.Equal(t, 1, again.Attempts, "tentativas zeradas antes do novo claim")
	require.ErrorIs(t, q.Retry(ctx, "00000000-0000-0000-0000-000000000000"), queue.ErrNotRetryable)
}

func TestClaimFiltersByKind(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	q := queue.NewPostgres(pool, queue.DefaultBackoff)
	ctx := context.Background()

	ai, _, err := q.Enqueue(ctx, queue.EnqueueParams{Kind: "ai_guide"})
	require.NoError(t, err)
	sync, _, err := q.Enqueue(ctx, queue.EnqueueParams{Kind: "sync_game"})
	require.NoError(t, err)

	got, err := q.Claim(ctx, time.Minute, "sync_platform", "sync_game")
	require.NoError(t, err)
	require.Equal(t, sync.ID, got.ID, "o worker de sync não pega job de IA")
	none, err := q.Claim(ctx, time.Minute, "sync_platform", "sync_game")
	require.NoError(t, err)
	require.Nil(t, none)

	got, err = q.Claim(ctx, time.Minute, "ai_guide", "ai_chat")
	require.NoError(t, err)
	require.Equal(t, ai.ID, got.ID)
}
