package api_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	dbgen "github.com/RodolfoBonis/rbnotes-api/db/gen"
	"github.com/RodolfoBonis/rbnotes-api/internal/api"
	"github.com/RodolfoBonis/rbnotes-api/internal/platform/auth"
	"github.com/RodolfoBonis/rbnotes-api/internal/platform/config"
	syncpkg "github.com/RodolfoBonis/rbnotes-api/internal/sync"
	"github.com/RodolfoBonis/rbnotes-api/internal/testsupport"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	pool := testsupport.NewPostgres(t)
	q := dbgen.New(pool)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	authn := auth.New(context.Background(), config.Auth{Mode: config.AuthModeDev}, logger)
	h := api.NewHandlers(api.HandlerDeps{
		Queries: q, Pool: pool, Sync: syncpkg.New(q, pool), Logger: logger, AudioChunkLimit: 1 << 20,
	})
	router := api.NewRouter(api.RouterDeps{Handlers: h, Auth: authn, Logger: logger, WriteTimeout: 30 * time.Second})
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	return srv
}

func do(t *testing.T, srv *httptest.Server, method, path, user, body string) (int, string) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, srv.URL+path, rd)
	require.NoError(t, err)
	if user != "" {
		req.Header.Set("X-Debug-User", user)
		req.Header.Set("X-Debug-Email", user+"@x")
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestMeRequiresAuth(t *testing.T) {
	srv := newTestServer(t)
	status, _ := do(t, srv, http.MethodGet, "/v1/me", "", "")
	require.Equal(t, http.StatusUnauthorized, status)

	status, body := do(t, srv, http.MethodGet, "/v1/me", "alice", "")
	require.Equal(t, http.StatusOK, status)
	require.Contains(t, body, "alice@x")
}

func TestSyncIsolationBetweenUsers(t *testing.T) {
	srv := newTestServer(t)
	// alice e bob existem (primeiro request cria o usuário).
	do(t, srv, http.MethodGet, "/v1/me", "alice", "")
	do(t, srv, http.MethodGet, "/v1/me", "bob", "")

	push := `{"changes":[{"entity":"note","id":"n1","updatedAt":"2026-10-20T12:00:00Z","deleted":false,"data":{"title":"segredo da alice"}}]}`
	status, body := do(t, srv, http.MethodPost, "/v1/sync/push", "alice", push)
	require.Equal(t, http.StatusOK, status, body)

	// alice vê a própria nota.
	status, body = do(t, srv, http.MethodGet, "/v1/sync/pull?since=0", "alice", "")
	require.Equal(t, http.StatusOK, status)
	var pull syncpkg.PullResult
	require.NoError(t, json.Unmarshal([]byte(body), &pull))
	require.Len(t, pull.Changes, 1)
	require.Contains(t, string(pull.Changes[0].Data), "segredo da alice")

	// bob NÃO vê nada da alice (isolamento por usuário).
	status, body = do(t, srv, http.MethodGet, "/v1/sync/pull?since=0", "bob", "")
	require.Equal(t, http.StatusOK, status)
	require.NoError(t, json.Unmarshal([]byte(body), &pull))
	require.Empty(t, pull.Changes, "bob não pode ver notas da alice")
}
