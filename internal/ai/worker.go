package ai

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/RodolfoBonis/rbnotes-api/internal/platform/metrics"
	"github.com/RodolfoBonis/rbnotes-api/internal/platform/queue"
)

const (
	emptyPollDelay = 2 * time.Second
	bookkeeping    = 15 * time.Second
)

// Worker consome os jobs de IA, UM POR VEZ: a CLI usa a cota da assinatura e cada execução é pesada
// (Node + modelo). Timeout por job; rate limit vira retry; tentativas esgotadas por erro transitório
// fecham a stream com erro (giveUp) para a UI não esperar para sempre.
type Worker struct {
	svc        *Service
	queue      queue.Queue
	jobTimeout time.Duration
	logger     *slog.Logger
}

// NewWorker constrói o worker de IA.
func NewWorker(svc *Service, q queue.Queue, jobTimeout time.Duration, logger *slog.Logger) *Worker {
	return &Worker{svc: svc, queue: q, jobTimeout: jobTimeout, logger: logger}
}

// Run consome a fila até ctx cancelar.
func (w *Worker) Run(ctx context.Context) {
	w.logger.Info("ai-worker iniciado", slog.Duration("job_timeout", w.jobTimeout))
	for ctx.Err() == nil {
		// O lease cobre o timeout da execução com folga: outro worker só assume se este morrer.
		job, err := w.queue.Claim(ctx, w.jobTimeout+2*time.Minute, queueKind)
		if err != nil {
			w.logger.Error("ai: claim falhou", slog.String("err", err.Error()))
		}
		if job == nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(emptyPollDelay):
			}
			continue
		}
		w.process(ctx, *job)
	}
}

func (w *Worker) process(ctx context.Context, job queue.Job) {
	jobCtx, cancel := context.WithTimeout(ctx, w.jobTimeout)
	err := w.svc.Handle(jobCtx, job)
	cancel()

	bg, bgCancel := context.WithTimeout(context.WithoutCancel(ctx), bookkeeping)
	defer bgCancel()
	if err == nil {
		if cerr := w.queue.Complete(bg, job.ID, job.LeaseToken); cerr != nil && !errors.Is(cerr, queue.ErrLeaseLost) {
			w.logger.Error("ai: complete falhou", slog.String("err", cerr.Error()))
		}
		metrics.JobProcessed(job.Kind, "ok")
		return
	}
	result := "retry"
	if queue.IsPermanent(err) || job.Attempts >= job.MaxAttempts {
		result = "dead"
	}
	w.logger.Warn("ai: job falhou", slog.String("kind", job.Kind), slog.String("job_id", job.ID),
		slog.String("result", result), slog.String("err", err.Error()))
	if ferr := w.queue.Fail(bg, job.ID, job.LeaseToken, err); ferr != nil && !errors.Is(ferr, queue.ErrLeaseLost) {
		w.logger.Error("ai: fail falhou", slog.String("err", ferr.Error()))
	}
	// Tentativas esgotadas por erro transitório (ex.: limite de uso persistente): encerra a stream.
	if result == "dead" && !queue.IsPermanent(err) {
		w.svc.giveUp(bg, job)
	}
	metrics.JobProcessed(job.Kind, result)
}
