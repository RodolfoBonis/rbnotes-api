// Command ai-worker executa os pedidos de IA do RbNotes com a CLI do Claude Code (headless),
// consumindo os jobs ai_job da fila e transmitindo a resposta por Redis Streams. Roda em imagem
// própria (node:22-slim + @anthropic-ai/claude-code; ver Dockerfile.ai-worker). Expõe /metrics e
// /healthz em METRICS_ADDR.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	rbnotes "github.com/RodolfoBonis/rbnotes-api"
	dbgen "github.com/RodolfoBonis/rbnotes-api/db/gen"
	"github.com/RodolfoBonis/rbnotes-api/internal/ai"
	"github.com/RodolfoBonis/rbnotes-api/internal/ai/claude"
	"github.com/RodolfoBonis/rbnotes-api/internal/platform/config"
	"github.com/RodolfoBonis/rbnotes-api/internal/platform/db"
	"github.com/RodolfoBonis/rbnotes-api/internal/platform/httpserver"
	"github.com/RodolfoBonis/rbnotes-api/internal/platform/metrics"
	"github.com/RodolfoBonis/rbnotes-api/internal/platform/queue"
	redisplat "github.com/RodolfoBonis/rbnotes-api/internal/platform/redis"
	"github.com/RodolfoBonis/rbnotes-api/internal/platform/telemetry"
)

const stateInterval = time.Minute

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", slog.String("err", err.Error()))
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logger := telemetry.NewLogger(cfg.Telemetry)
	slog.SetDefault(logger)
	logger.Info("iniciando ai-worker", slog.String("version", rbnotes.Version()), slog.String("model", cfg.AI.Model))
	if cfg.AI.OAuthToken == "" {
		return errors.New("CLAUDE_CODE_OAUTH_TOKEN ausente (gere com `claude setup-token`)")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// As migrations são aplicadas pela API; o ai-worker só usa o schema.
	pool, err := db.NewPool(ctx, cfg.DB)
	if err != nil {
		return err
	}
	defer pool.Close()

	// Redis é obrigatório aqui: é o canal da resposta em tempo real.
	rdb, err := redisplat.New(ctx, cfg.Redis)
	if err != nil {
		return err
	}
	defer func() { _ = rdb.Close() }()

	queries := dbgen.New(pool)
	jobs := queue.NewPostgres(pool, queue.DefaultBackoff)
	svc := ai.New(ai.Deps{
		Queries: queries, Pool: pool, Queue: jobs, Redis: rdb,
		Runner: claude.New(cfg.AI.ClaudeBin, cfg.AI.OAuthToken, cfg.AI.Home, logger),
		Model:  cfg.AI.Model, Workdir: cfg.AI.Workdir, DailyLimit: cfg.AI.DailyLimit, Logger: logger,
	})
	worker := ai.NewWorker(svc, jobs, cfg.AI.JobTimeout, logger)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		worker.Run(ctx)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		publishState(ctx, queries, logger)
	}()

	mux := http.NewServeMux()
	mux.Handle("/metrics", metrics.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	srv := httpserver.NewAddr(cfg.Metrics.Addr, mux, logger)
	wg.Add(1)
	go func() {
		defer wg.Done()
		if merr := srv.Run(ctx); merr != nil {
			logger.Error("metrics server parou com erro", slog.String("err", merr.Error()))
		}
	}()

	<-ctx.Done()
	wg.Wait()
	return nil
}

// publishState publica o retrato da fila (gauge jobs_queue) a cada minuto.
func publishState(ctx context.Context, q *dbgen.Queries, logger *slog.Logger) {
	t := time.NewTicker(stateInterval)
	defer t.Stop()
	emit := func() {
		rows, err := q.CountJobsByKindStatus(ctx)
		if err != nil {
			logger.Warn("ai: contar fila falhou", slog.String("err", err.Error()))
			return
		}
		counts := make([]metrics.JobQueueCount, 0, len(rows))
		for _, r := range rows {
			counts = append(counts, metrics.JobQueueCount{Kind: r.Kind, Status: r.Status, N: r.N})
		}
		metrics.SetState(counts)
	}
	emit()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			emit()
		}
	}
}
