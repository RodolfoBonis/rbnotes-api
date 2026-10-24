package api

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/RodolfoBonis/rbnotes-api/internal/api/middleware"
	"github.com/RodolfoBonis/rbnotes-api/internal/platform/auth"
)

// maxJSONBody é o teto de corpo JSON das rotas de sync/IA: transcrições longas e lotes de sync
// passam disto facilmente dos 64 KiB padrão.
const maxJSONBody = 16 << 20 // 16 MiB

// RouterDeps são as dependências para montar o router.
type RouterDeps struct {
	Handlers     *Handlers
	Auth         *auth.Authenticator
	Logger       *slog.Logger
	ServiceName  string
	WriteTimeout time.Duration
	// CORSAllowedOrigins: origens do desktop/web autorizadas (a API vive em api.notes.<domínio>).
	CORSAllowedOrigins []string
}

// requestTimeout deriva o timeout de request do WriteTimeout do servidor: fica ABAIXO dele para o
// handler cancelar e responder antes de o servidor matar a conexão.
func requestTimeout(writeTimeout time.Duration) time.Duration {
	const margin = 2 * time.Second
	if writeTimeout <= 0 {
		return 28 * time.Second
	}
	if writeTimeout <= margin {
		return writeTimeout
	}
	return writeTimeout - margin
}

// NewRouter monta o chi router. /healthz e /readyz são abertos (probes). /metrics NÃO é servido
// aqui: fica só no listener interno (METRICS_ADDR). O resto fica sob /v1 exigindo autenticação.
func NewRouter(d RouterDeps) http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(middleware.RequestLogger(d.Logger))
	r.Use(middleware.Metrics)
	r.Use(middleware.Recoverer)
	r.Use(middleware.SecurityHeaders)
	r.Use(middleware.CORS(d.CORSAllowedOrigins))
	r.Use(middleware.Tracing(d.ServiceName))

	h := d.Handlers

	// Infra (aberto).
	r.Get("/healthz", h.health)
	r.Get("/readyz", h.ready)

	// API autenticada.
	r.Route("/v1", func(r chi.Router) {
		r.Use(d.Auth.Middleware)
		r.Use(h.resolveUser) // usuário do app (nasce no primeiro request)

		// SSE da IA: conexão longa (heartbeat + deadline de escrita renovado), FORA do timeout.
		r.Get("/ai/streams/{id}", h.aiStream)

		// Áudio: octet-stream no upload e streaming no download — sem JSONBody e sem timeout curto.
		r.Put("/notes/{id}/audio", h.putAudio)
		r.Get("/notes/{id}/audio", h.getAudio)

		// Demais rotas: JSON (limite maior p/ transcrições/lotes) + timeout de request.
		r.Group(func(r chi.Router) {
			r.Use(middleware.JSONBody(maxJSONBody))
			r.Use(middleware.Timeout(requestTimeout(d.WriteTimeout)))

			r.Get("/me", h.getMe)

			r.Post("/sync/push", h.pushChanges)
			r.Get("/sync/pull", h.pullChanges)

			r.Post("/ai/notes/{id}/enrich", h.enrichNote)
			r.Post("/ai/notes/{id}/chat", h.chatNote)
			r.Post("/ai/ask", h.ask)
			r.Post("/ai/collections/rule", h.collectionRule)
			r.Post("/ai/diary/month", h.diaryMonth)
			r.Get("/ai/diary/prompt", h.dailyPrompt)
			r.Post("/ai/weekly-summary", h.weeklySummary)
			r.Get("/ai/usage", h.aiUsage)

			// Apagar o áudio de uma nota (rápido): fica no grupo com timeout de request.
			r.Delete("/notes/{id}/audio", h.deleteAudio)
		})
	})

	return r
}
