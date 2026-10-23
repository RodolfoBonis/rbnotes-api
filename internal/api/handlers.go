// Package api monta a borda HTTP: router, middlewares e handlers. Recebe os serviços por injeção;
// não conhece detalhes de persistência além das queries geradas (dbgen) que consome.
package api

import (
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	dbgen "github.com/RodolfoBonis/rbnotes-api/db/gen"
	"github.com/RodolfoBonis/rbnotes-api/internal/ai"
	"github.com/RodolfoBonis/rbnotes-api/internal/audio"
	syncpkg "github.com/RodolfoBonis/rbnotes-api/internal/sync"
)

// Handlers agrega as dependências consumidas pelas rotas.
type Handlers struct {
	q               *dbgen.Queries
	pool            *pgxpool.Pool
	sync            *syncpkg.Service
	ai              *ai.Service
	audio           *audio.Service
	rdb             *redis.Client
	logger          *slog.Logger
	authReady       func() bool
	audioChunkLimit int64
}

// HandlerDeps injeta as dependências dos handlers.
type HandlerDeps struct {
	Queries *dbgen.Queries
	Pool    *pgxpool.Pool
	Sync    *syncpkg.Service
	// AI enfileira pedidos; Redis é o canal da resposta em tempo real (SSE). nil = IA desligada.
	AI    *ai.Service
	Redis *redis.Client
	// Audio guarda/serve o áudio no MinIO. nil = áudio desligado (501).
	Audio  *audio.Service
	Logger *slog.Logger
	// AuthReady indica se o autenticador já carregou o provider OIDC (readyz). nil = sempre pronto.
	AuthReady func() bool
	// AudioChunkLimit é o teto de bytes por chunk de áudio.
	AudioChunkLimit int64
}

// NewHandlers constrói os handlers.
func NewHandlers(d HandlerDeps) *Handlers {
	authReady := d.AuthReady
	if authReady == nil {
		authReady = func() bool { return true }
	}
	return &Handlers{
		q:               d.Queries,
		pool:            d.Pool,
		sync:            d.Sync,
		ai:              d.AI,
		audio:           d.Audio,
		rdb:             d.Redis,
		logger:          d.Logger,
		authReady:       authReady,
		audioChunkLimit: d.AudioChunkLimit,
	}
}

// health responde 200 (liveness).
func (h *Handlers) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ready responde 200 se o Postgres responde ao ping E o provider de auth já carregou; 503 caso
// contrário (o k8s mantém o pod fora do Service até ficar pronto).
func (h *Handlers) ready(w http.ResponseWriter, r *http.Request) {
	if err := h.pool.Ping(r.Context()); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable", "reason": "db"})
		return
	}
	if !h.authReady() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable", "reason": "auth"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}
