package ai

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	dbgen "github.com/RodolfoBonis/rbnotes-api/db/gen"
	"github.com/RodolfoBonis/rbnotes-api/internal/ai/stream"
)

const (
	// stuckJobTimeout: um ai_jobs queued/running parado além disso, sem job vivo na fila, é
	// considerado órfão (o worker morreu sem fechar a stream).
	stuckJobTimeout = 15 * time.Minute
	// defaultRetention é usado quando nenhum valor é configurado.
	defaultRetention = 30 * 24 * time.Hour
)

// ReapStuckJobs marca como erro — e fecha a stream — os ai_jobs presos que não têm mais um job vivo
// na fila. Devolve quantos encerrou.
func (s *Service) ReapStuckJobs(ctx context.Context) (int, error) {
	const msg = "A IA não conseguiu concluir o pedido (o processamento foi interrompido). Tente de novo."
	cutoff := pgtype.Timestamptz{Time: time.Now().Add(-stuckJobTimeout), Valid: true}
	rows, err := s.q.ReapStuckAIJobs(ctx, dbgen.ReapStuckAIJobsParams{UpdatedAt: cutoff, Error: msg})
	if err != nil {
		return 0, fmt.Errorf("ai: reaper: %w", err)
	}
	for _, row := range rows {
		if s.rdb == nil {
			continue
		}
		// Job CIFRADO: fecha a stream com TTL curto (os deltas carregam texto claro e o reaper já
		// esvaziou input/result no banco).
		var opts []stream.Option
		if row.Ephemeral {
			opts = append(opts, stream.Ephemeral())
		}
		s.closeStream(ctx, stream.NewPublisher(ctx, s.rdb, uuidString(row.ID), opts...), stream.TypeError, msg)
	}
	return len(rows), nil
}

// Cleanup faz o housekeeping: apaga jobs terminais antigos da fila e purga os dados pesados antigos
// de ai_requests/ai_jobs (retenção).
func (s *Service) Cleanup(ctx context.Context) error {
	ret := s.retention
	if ret <= 0 {
		ret = defaultRetention
	}
	if _, err := s.queue.Cleanup(ctx, ret, ret); err != nil {
		return fmt.Errorf("ai: cleanup fila: %w", err)
	}
	cutoff := pgtype.Timestamptz{Time: time.Now().Add(-ret), Valid: true}
	if err := s.q.DeleteOldAIRequests(ctx, cutoff); err != nil {
		return fmt.Errorf("ai: limpar ai_requests: %w", err)
	}
	if err := s.q.ClearOldAIJobData(ctx, cutoff); err != nil {
		return fmt.Errorf("ai: limpar ai_jobs: %w", err)
	}
	// Rede de segurança: nenhum input de nota CIFRADA (efêmera) pode sobreviver em claro, mesmo antes
	// do cutoff de retenção.
	if err := s.q.ClearEphemeralAIJobData(ctx); err != nil {
		return fmt.Errorf("ai: limpar ai_jobs efêmeros: %w", err)
	}
	return nil
}

// RunMaintenance roda o reaper e o cleanup periodicamente até ctx cancelar (chamado pelo ai-worker).
func (s *Service) RunMaintenance(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if n, err := s.ReapStuckJobs(ctx); err != nil {
			s.logger.Warn("ai: reaper falhou", slog.String("err", err.Error()))
		} else if n > 0 {
			s.logger.Info("ai: jobs presos encerrados", slog.Int("n", n))
		}
		if err := s.Cleanup(ctx); err != nil {
			s.logger.Warn("ai: cleanup falhou", slog.String("err", err.Error()))
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
