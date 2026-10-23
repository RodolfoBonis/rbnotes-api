package ai

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/RodolfoBonis/rbnotes-api/internal/platform/queue"
)

// fakeHandler permite exercitar o worker sem banco nem CLI.
type fakeHandler struct {
	fn      func(ctx context.Context, job queue.Job) error
	mu      sync.Mutex
	gaveUp  int
	handled int
}

func (f *fakeHandler) Handle(ctx context.Context, job queue.Job) error {
	f.mu.Lock()
	f.handled++
	f.mu.Unlock()
	return f.fn(ctx, job)
}

func (f *fakeHandler) giveUp(_ context.Context, _ queue.Job) {
	f.mu.Lock()
	f.gaveUp++
	f.mu.Unlock()
}

// fakeQueue registra os efeitos do bookkeeping (Complete/Fail).
type fakeQueue struct {
	mu         sync.Mutex
	completed  int
	failed     int
	failCauses []error
}

func (q *fakeQueue) Enqueue(context.Context, queue.EnqueueParams) (*queue.Job, bool, error) {
	return nil, false, nil
}
func (q *fakeQueue) Claim(context.Context, time.Duration, ...string) (*queue.Job, error) {
	return nil, nil
}
func (q *fakeQueue) Complete(_ context.Context, _, _ string) error {
	q.mu.Lock()
	q.completed++
	q.mu.Unlock()
	return nil
}
func (q *fakeQueue) Fail(_ context.Context, _, _ string, cause error) error {
	q.mu.Lock()
	q.failed++
	q.failCauses = append(q.failCauses, cause)
	q.mu.Unlock()
	return nil
}
func (q *fakeQueue) Cleanup(context.Context, time.Duration, time.Duration) (int64, error) {
	return 0, nil
}
func (q *fakeQueue) Retry(context.Context, string) error { return nil }

func newTestWorker(h handler, q queue.Queue, jobTimeout, grace time.Duration) *Worker {
	return &Worker{svc: h, queue: q, jobTimeout: jobTimeout, shutdownGrace: grace,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func TestWorkerProcessSuccess(t *testing.T) {
	h := &fakeHandler{fn: func(context.Context, queue.Job) error { return nil }}
	q := &fakeQueue{}
	w := newTestWorker(h, q, time.Second, time.Second)
	w.process(context.Background(), queue.Job{ID: "j1", MaxAttempts: 5})
	require.Equal(t, 1, q.completed)
	require.Equal(t, 0, q.failed)
	require.Equal(t, 0, h.gaveUp)
}

// TestWorkerProcessJobDeadline: o runner estoura o timeout DO JOB (pai vivo) → falha permanente,
// sem giveUp (não é erro transitório), e o job vai para dead.
func TestWorkerProcessJobDeadline(t *testing.T) {
	h := &fakeHandler{fn: func(ctx context.Context, _ queue.Job) error {
		<-ctx.Done() // bloqueia até o deadline do job
		return ctx.Err()
	}}
	q := &fakeQueue{}
	w := newTestWorker(h, q, 50*time.Millisecond, time.Minute)
	w.process(context.Background(), queue.Job{ID: "j2", MaxAttempts: 5})
	require.Equal(t, 0, q.completed)
	require.Equal(t, 1, q.failed)
	require.True(t, queue.IsPermanent(q.failCauses[0]), "deadline do job vira falha permanente")
	require.Equal(t, 0, h.gaveUp)
}

// TestWorkerProcessParentCancel: SIGTERM (pai cancelado) e o job não cabe na janela de graça → NÃO
// marca o job como falho nem o conclui; o lease expira e ele é retomado depois do deploy.
func TestWorkerProcessParentCancel(t *testing.T) {
	h := &fakeHandler{fn: func(ctx context.Context, _ queue.Job) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	q := &fakeQueue{}
	w := newTestWorker(h, q, 10*time.Second, 20*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // SIGTERM já chegou
	w.process(ctx, queue.Job{ID: "j3", MaxAttempts: 5})
	require.Equal(t, 0, q.completed, "não confirma um job interrompido")
	require.Equal(t, 0, q.failed, "não marca como falho: deve ser retomado")
	require.Equal(t, 0, h.gaveUp)
}

// TestWorkerProcessParentCancelButFinished: no shutdown, se o job terminou com sucesso dentro da
// graça, ele é confirmado normalmente.
func TestWorkerProcessParentCancelButFinished(t *testing.T) {
	h := &fakeHandler{fn: func(context.Context, queue.Job) error { return nil }}
	q := &fakeQueue{}
	w := newTestWorker(h, q, 10*time.Second, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w.process(ctx, queue.Job{ID: "j4", MaxAttempts: 5})
	require.Equal(t, 1, q.completed)
	require.Equal(t, 0, q.failed)
}

// TestWorkerProcessTransientExhausted: erro transitório com tentativas esgotadas → giveUp fecha a
// stream para a UI não esperar para sempre.
func TestWorkerProcessTransientExhausted(t *testing.T) {
	h := &fakeHandler{fn: func(context.Context, queue.Job) error { return errors.New("ai: limite de uso") }}
	q := &fakeQueue{}
	w := newTestWorker(h, q, time.Second, time.Second)
	w.process(context.Background(), queue.Job{ID: "j5", Attempts: 5, MaxAttempts: 5})
	require.Equal(t, 1, q.failed)
	require.False(t, queue.IsPermanent(q.failCauses[0]))
	require.Equal(t, 1, h.gaveUp)
}
