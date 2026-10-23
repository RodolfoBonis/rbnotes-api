// Package middleware reúne os middlewares HTTP transversais da borda da API: request id, recover,
// cabeçalhos de segurança, tracing OTel, log estruturado, timeout de request e limite/validação de
// corpo nas rotas de escrita.
package middleware

import (
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/RodolfoBonis/rbnotes-api/internal/platform/httperr"
	"github.com/RodolfoBonis/rbnotes-api/internal/platform/metrics"
)

// MaxBodyBytes é o teto do corpo aceito nas rotas de escrita (defesa contra payloads grandes).
const MaxBodyBytes int64 = 64 << 10 // 64 KiB

// RequestID injeta um id único por request (header X-Request-Id + contexto).
var RequestID = middleware.RequestID

// Recoverer converte panics em 500 (problem+json) sem derrubar o processo e loga o stack.
func Recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			if rec == http.ErrAbortHandler { //nolint:errorlint // sentinel de panic, não é error wrap
				panic(rec)
			}
			slog.ErrorContext(r.Context(), "panic recuperado",
				slog.Any("panic", rec),
				slog.String("request_id", middleware.GetReqID(r.Context())),
				slog.String("stack", string(debug.Stack())),
			)
			httperr.Write(w, r, httperr.New(httperr.KindInternal, "Internal error", ""))
		}()
		next.ServeHTTP(w, r)
	})
}

// Timeout limita a duração de um request (cancela o contexto).
func Timeout(d time.Duration) func(http.Handler) http.Handler {
	return middleware.Timeout(d)
}

// Tracing instrumenta cada request com um span OTel (no-op se tracing desligado).
func Tracing(service string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return otelhttp.NewHandler(next, service)
	}
}

// SecurityHeaders aplica cabeçalhos de segurança básicos (OWASP).
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		next.ServeHTTP(w, r)
	})
}

// EnforceJSONBody exige Content-Type application/json nas rotas com corpo (POST/PUT/PATCH) e limita
// o corpo a MaxBodyBytes (http.MaxBytesReader), protegendo os handlers de payloads enormes e de
// corpos não-JSON.
func EnforceJSONBody(next http.Handler) http.Handler {
	return JSONBody(MaxBodyBytes)(next)
}

// JSONBody é como EnforceJSONBody, mas com um limite de corpo configurável. Útil para rotas que
// aceitam payloads maiores (ex.: sync push com várias notas; enrich com transcrição longa).
func JSONBody(limit int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodPost, http.MethodPut, http.MethodPatch:
				if ct := r.Header.Get("Content-Type"); ct != "" {
					mediaType := strings.TrimSpace(strings.SplitN(ct, ";", 2)[0])
					if !strings.EqualFold(mediaType, "application/json") {
						httperr.Write(w, r, httperr.New(httperr.KindBadRequest, "Unsupported Media Type",
							"Content-Type deve ser application/json"))
						return
					}
				}
				r.Body = http.MaxBytesReader(w, r.Body, limit)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequestLogger registra cada request de forma estruturada (sem segredos/PII). Deve ficar POR FORA
// do Recoverer para que panics (convertidos em 500 pelo Recoverer interno) sejam logados com o
// status correto.
func RequestLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			start := time.Now()
			defer func() {
				logger.LogAttrs(r.Context(), slog.LevelInfo, "http_request",
					slog.String("method", r.Method),
					slog.String("path", r.URL.Path),
					slog.Int("status", ww.Status()),
					slog.Int("bytes", ww.BytesWritten()),
					slog.Duration("duration", time.Since(start)),
					slog.String("request_id", middleware.GetReqID(r.Context())),
				)
			}()
			next.ServeHTTP(ww, r)
		})
	}
}

// Metrics conta requisições e mede latência por rota (padrão do chi, ex. /api/v1/games/{id}) e
// classe de status. Probes e /metrics ficam de fora; a stream SSE da IA entra só na contagem.
func Metrics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/metrics", "/healthz", "/readyz":
			next.ServeHTTP(w, r)
			return
		}
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		start := time.Now()
		next.ServeHTTP(ww, r)
		route := "unmatched"
		if rc := chi.RouteContext(r.Context()); rc != nil && rc.RoutePattern() != "" {
			route = rc.RoutePattern()
		}
		status := ww.Status()
		if status == 0 {
			status = http.StatusOK
		}
		metrics.ObserveHTTP(r.Method, route, status, time.Since(start), !strings.HasSuffix(route, "/ai/streams/{id}"))
	})
}
