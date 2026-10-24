package api_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	dbgen "github.com/RodolfoBonis/rbnotes-api/db/gen"
	"github.com/RodolfoBonis/rbnotes-api/internal/ai"
	"github.com/RodolfoBonis/rbnotes-api/internal/api"
	"github.com/RodolfoBonis/rbnotes-api/internal/platform/auth"
	"github.com/RodolfoBonis/rbnotes-api/internal/platform/config"
	syncpkg "github.com/RodolfoBonis/rbnotes-api/internal/sync"
	"github.com/RodolfoBonis/rbnotes-api/internal/testsupport"
)

// newUsageServer monta um servidor com o serviço de IA ligado (sem Redis/fila — GET /ai/usage não
// usa SSE). DailyLimit fixo para checar quota.limit.
func newUsageServer(t *testing.T) *httptest.Server {
	t.Helper()
	pool := testsupport.NewPostgres(t)
	q := dbgen.New(pool)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	authn := auth.New(context.Background(), config.Auth{Mode: config.AuthModeDev}, logger)
	aiSvc := ai.New(ai.Deps{Queries: q, Pool: pool, DailyLimit: 200, Logger: logger})
	h := api.NewHandlers(api.HandlerDeps{
		Queries: q, Pool: pool, Sync: syncpkg.New(q, pool), AI: aiSvc, Logger: logger, AudioChunkLimit: 1 << 20,
	})
	router := api.NewRouter(api.RouterDeps{Handlers: h, Auth: authn, Logger: logger, WriteTimeout: 30 * time.Second})
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	return srv
}

func TestAIUsageEndpoint(t *testing.T) {
	srv := newUsageServer(t)
	do(t, srv, http.MethodGet, "/v1/me", "alice", "") // cria o usuário

	// Happy path: janela padrão (30 dias), fuso válido.
	status, body := do(t, srv, http.MethodGet, "/v1/ai/usage?tz=America/Sao_Paulo", "alice", "")
	require.Equal(t, http.StatusOK, status, body)
	var rep ai.UsageReport
	require.NoError(t, json.Unmarshal([]byte(body), &rep))
	require.Equal(t, "USD", rep.Currency)
	require.True(t, rep.CostIsEstimate)
	require.Equal(t, "America/Sao_Paulo", rep.TZ)
	require.Equal(t, 200, rep.Quota.Limit)
	require.NotNil(t, rep.ByKind)
	require.NotNil(t, rep.Days)

	// tz inválido → 400.
	status, _ = do(t, srv, http.MethodGet, "/v1/ai/usage?tz=Marte/Olympus", "alice", "")
	require.Equal(t, http.StatusBadRequest, status)

	// from depois de to → 400.
	status, _ = do(t, srv, http.MethodGet, "/v1/ai/usage?from=2026-10-20&to=2026-10-10", "alice", "")
	require.Equal(t, http.StatusBadRequest, status)

	// Intervalo acima de 366 dias → 400.
	status, _ = do(t, srv, http.MethodGet, "/v1/ai/usage?from=2024-01-01&to=2026-10-01", "alice", "")
	require.Equal(t, http.StatusBadRequest, status)

	// Data malformada → 400.
	status, _ = do(t, srv, http.MethodGet, "/v1/ai/usage?from=20-10-2026", "alice", "")
	require.Equal(t, http.StatusBadRequest, status)

	// Exige autenticação.
	status, _ = do(t, srv, http.MethodGet, "/v1/ai/usage", "", "")
	require.Equal(t, http.StatusUnauthorized, status)
}
