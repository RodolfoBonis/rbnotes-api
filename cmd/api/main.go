// Command api é o servidor HTTP do RbNotes: expõe a API REST (/v1), valida o JWT do Keycloak
// (realm rbnotes), faz o sync offline-first, guarda/serve o áudio e enfileira pedidos de IA.
// Não executa a CLI do Claude — isso é do cmd/ai-worker.
//
// Dois listeners: o público (HTTP_ADDR) só com a aplicação; o interno (METRICS_ADDR) com /metrics
// e /healthz (o /metrics NUNCA é exposto no host público).
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"

	rbnotes "github.com/RodolfoBonis/rbnotes-api"
	dbgen "github.com/RodolfoBonis/rbnotes-api/db/gen"
	migrations "github.com/RodolfoBonis/rbnotes-api/db/migrations"
	"github.com/RodolfoBonis/rbnotes-api/internal/ai"
	"github.com/RodolfoBonis/rbnotes-api/internal/api"
	"github.com/RodolfoBonis/rbnotes-api/internal/audio"
	"github.com/RodolfoBonis/rbnotes-api/internal/platform/auth"
	"github.com/RodolfoBonis/rbnotes-api/internal/platform/config"
	"github.com/RodolfoBonis/rbnotes-api/internal/platform/db"
	"github.com/RodolfoBonis/rbnotes-api/internal/platform/httpserver"
	"github.com/RodolfoBonis/rbnotes-api/internal/platform/metrics"
	"github.com/RodolfoBonis/rbnotes-api/internal/platform/queue"
	redisplat "github.com/RodolfoBonis/rbnotes-api/internal/platform/redis"
	"github.com/RodolfoBonis/rbnotes-api/internal/platform/telemetry"
	syncpkg "github.com/RodolfoBonis/rbnotes-api/internal/sync"
)

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
	logger.Info("iniciando api", slog.String("version", rbnotes.Version()))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	shutdownTracing, err := telemetry.SetupTracing(ctx, cfg.Telemetry)
	if err != nil {
		return err
	}
	defer func() { _ = shutdownTracing(context.WithoutCancel(ctx)) }()
	shutdownMetrics, err := telemetry.SetupMetrics(ctx, cfg.Telemetry)
	if err != nil {
		return err
	}
	defer func() { _ = shutdownMetrics(context.WithoutCancel(ctx)) }()

	if cfg.DB.MigrateOnStart {
		if err := db.Migrate(ctx, cfg.DB.URL, migrations.FS); err != nil {
			return err
		}
	}

	pool, err := db.NewPool(ctx, cfg.DB)
	if err != nil {
		return err
	}
	defer pool.Close()

	authenticator := auth.New(ctx, cfg.Auth, logger)
	queries := dbgen.New(pool)
	jobs := queue.NewPostgres(pool, queue.DefaultBackoff)

	// Redis: canal da resposta da IA em tempo real (SSE). Sem ele, a API sobe com a IA desligada.
	var aiSvc *ai.Service
	rdb, rerr := redisplat.New(ctx, cfg.Redis)
	if rerr != nil {
		logger.Warn("redis indisponível; IA desligada", slog.String("err", rerr.Error()))
		rdb = nil
		metrics.SetDependencyUp("redis", false)
	} else {
		defer func() { _ = rdb.Close() }()
		metrics.SetDependencyUp("redis", true)
		aiSvc = ai.New(ai.Deps{Queries: queries, Pool: pool, Queue: jobs, Redis: rdb, Model: cfg.AI.Model, DailyLimit: cfg.AI.DailyLimit, Logger: logger})
	}

	// Áudio (MinIO). Sem credenciais/endpoint acessível, sobe com áudio desligado (501 nas rotas).
	var audioSvc *audio.Service
	audioSvc, aerr := audio.New(ctx, cfg.MinIO, cfg.Audio.MaxBytes, queries, pool, logger)
	if aerr != nil {
		logger.Warn("minio indisponível; áudio desligado", slog.String("err", aerr.Error()))
		audioSvc = nil
		metrics.SetDependencyUp("minio", false)
	} else {
		metrics.SetDependencyUp("minio", true)
	}

	handlers := api.NewHandlers(api.HandlerDeps{
		Queries: queries, Pool: pool, Sync: syncpkg.New(queries, pool), AI: aiSvc, Redis: rdb,
		Audio: audioSvc, Logger: logger, AuthReady: authenticator.Ready, AudioChunkLimit: cfg.Audio.ChunkLimit,
	})
	router := api.NewRouter(api.RouterDeps{
		Handlers: handlers, Auth: authenticator, Logger: logger,
		ServiceName: cfg.Telemetry.ServiceName, WriteTimeout: cfg.HTTP.WriteTimeout,
		CORSAllowedOrigins: cfg.HTTP.CORSAllowedOrigins,
	})

	var wg sync.WaitGroup

	// Listener interno de métricas (/metrics + /healthz) — NUNCA no host público.
	mux := http.NewServeMux()
	mux.Handle("/metrics", metrics.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	metricsSrv := httpserver.NewAddr(cfg.Metrics.Addr, mux, logger)
	wg.Add(1)
	go func() {
		defer wg.Done()
		if merr := metricsSrv.Run(ctx); merr != nil {
			logger.Error("metrics server parou com erro", slog.String("err", merr.Error()))
		}
	}()

	srv := httpserver.New(cfg.HTTP, router, logger)
	runErr := srv.Run(ctx)
	wg.Wait()
	return runErr
}
