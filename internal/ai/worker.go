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
	// bookkeeping é o teto do contexto de pós-processamento (Complete/Fail/giveUp): curto e
	// derivado de WithoutCancel para sobreviver ao cancelamento do pai (SIGTERM).
	bookkeeping = 10 * time.Second
)

// handler executa um job e encerra os que esgotaram as tentativas. *Service o satisfaz; os testes
// injetam um fake para exercitar o worker sem banco.
type handler interface {
	Handle(ctx context.Context, job queue.Job) error
	giveUp(ctx context.Context, job queue.Job)
}

// Worker consome os jobs de IA, UM POR VEZ: a CLI usa a cota da assinatura e cada execução é pesada
// (Node + modelo). Timeout por job; rate limit vira retry; tentativas esgotadas por erro transitório
// fecham a stream com erro (giveUp) para a UI não esperar para sempre.
//
// Shutdown (SIGTERM): o worker para de reivindicar jobs novos, mas deixa o job em andamento terminar
// até shutdownGrace (o pod tem terminationGracePeriodSeconds > AI_JOB_TIMEOUT). Se o job não cabe na
// janela, o cancelamento do pai é tratado como ERRO RETENTÁVEL: o job NÃO é marcado como falho e o
// lease expira, para ser retomado por outro worker depois do deploy.
type Worker struct {
	svc           handler
	queue         queue.Queue
	jobTimeout    time.Duration
	shutdownGrace time.Duration
	logger        *slog.Logger
}

// NewWorker constrói o worker de IA. A janela de graça do shutdown é o próprio jobTimeout: um job já
// iniciado pode usar todo o seu orçamento antes de o worker desistir.
func NewWorker(svc *Service, q queue.Queue, jobTimeout time.Duration, logger *slog.Logger) *Worker {
	return &Worker{svc: svc, queue: q, jobTimeout: jobTimeout, shutdownGrace: jobTimeout, logger: logger}
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

// jobContext monta o contexto da execução. Ele NÃO herda o cancelamento do pai diretamente: assim um
// SIGTERM não mata o job na hora. Em vez disso, o job tem seu próprio deadline (jobTimeout) e, quando
// o pai é cancelado, ganha mais shutdownGrace antes de ser forçado a parar.
func (w *Worker) jobContext(ctx context.Context) (context.Context, context.CancelFunc) {
	jobCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), w.jobTimeout)
	stop := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			select {
			case <-time.After(w.shutdownGrace):
				cancel() // graça esgotada no shutdown: força o fim do job.
			case <-stop:
			}
		case <-stop:
		}
	}()
	return jobCtx, func() { close(stop); cancel() }
}

func (w *Worker) process(ctx context.Context, job queue.Job) {
	jobCtx, cancel := w.jobContext(ctx)
	err := w.svc.Handle(jobCtx, job)
	deadline := errors.Is(jobCtx.Err(), context.DeadlineExceeded)
	cancel()

	bg, bgCancel := context.WithTimeout(context.WithoutCancel(ctx), bookkeeping)
	defer bgCancel()

	// Cancelamento do PAI (SIGTERM): não marca o job como falho. Se terminou com sucesso dentro da
	// graça, confirma; senão deixa o lease expirar para ser retomado depois do deploy.
	if ctx.Err() != nil {
		if err == nil {
			w.complete(bg, job)
			return
		}
		w.logger.Info("ai: shutdown durante o job; será retomado depois do deploy",
			slog.String("job_id", job.ID), slog.String("kind", job.Kind))
		metrics.JobProcessed(job.Kind, "interrupted")
		return
	}

	if err == nil {
		w.complete(bg, job)
		return
	}

	// Deadline do PRÓPRIO job (não shutdown): falha permanente — não adianta reprocessar. O serviço
	// já marcou o ai_jobs e fechou a stream com a mensagem ao usuário.
	if deadline && !queue.IsPermanent(err) {
		err = queue.Permanent(err)
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

func (w *Worker) complete(bg context.Context, job queue.Job) {
	if cerr := w.queue.Complete(bg, job.ID, job.LeaseToken); cerr != nil && !errors.Is(cerr, queue.ErrLeaseLost) {
		w.logger.Error("ai: complete falhou", slog.String("err", cerr.Error()))
	}
	metrics.JobProcessed(job.Kind, "ok")
}
