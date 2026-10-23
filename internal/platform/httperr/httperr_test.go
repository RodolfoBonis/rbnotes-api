package httperr

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestWriteLogsInternalCause garante que um 500 nunca vaza o detalhe interno no corpo, mas SEMPRE
// loga a causa (para correlacionar com o request_id/trace), enquanto erros não-internos não logam
// como 500.
func TestWriteLogsInternalCause(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(old)

	req := httptest.NewRequest("GET", "/v1/notes", nil)

	rec := httptest.NewRecorder()
	Write(rec, req, Wrap(KindInternal, "Internal error", errors.New("conexão com o banco caiu")))
	require.Equal(t, 500, rec.Code)
	require.NotContains(t, rec.Body.String(), "conexão com o banco caiu", "detalhe interno não pode vazar")
	require.Contains(t, buf.String(), "erro interno (500)")
	require.Contains(t, buf.String(), "conexão com o banco caiu", "a causa deve ser logada")

	buf.Reset()
	Write(httptest.NewRecorder(), req, Validation("campo obrigatório"))
	require.NotContains(t, buf.String(), "erro interno (500)", "um 422 não loga como 500")
}
