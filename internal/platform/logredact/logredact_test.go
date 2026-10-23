package logredact_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/RodolfoBonis/rbnotes-api/internal/platform/logredact"
)

func newLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{ReplaceAttr: logredact.ReplaceAttr}))
}

func TestRedactsSecretKeys(t *testing.T) {
	var buf bytes.Buffer
	log := newLogger(&buf)

	log.Info("connect",
		slog.String("api_key", "SECRETKEY123"),
		slog.String("npsso", "NPSSO_TOKEN_XYZ"),
		slog.String("authorization", "Bearer abc.def.ghi"),
		slog.String("password", "hunter2"),
		slog.String("game", "Hollow Knight"),
	)

	out := buf.String()
	require.NotContains(t, out, "SECRETKEY123")
	require.NotContains(t, out, "NPSSO_TOKEN_XYZ")
	require.NotContains(t, out, "abc.def.ghi")
	require.NotContains(t, out, "hunter2")
	require.Equal(t, 4, strings.Count(out, "[REDACTED]"))
	// Campos benignos permanecem.
	require.Contains(t, out, "Hollow Knight")
}

func TestRedactsNestedGroup(t *testing.T) {
	var buf bytes.Buffer
	log := newLogger(&buf)
	log.LogAttrs(context.Background(), slog.LevelInfo, "msg",
		slog.Group("creds", slog.String("token", "T0P"), slog.String("user", "rb")),
	)
	out := buf.String()
	require.NotContains(t, out, "T0P")
	require.Contains(t, out, "[REDACTED]")
	require.Contains(t, out, "rb")
}

type credential struct {
	APIKey string
	logredact.Redact
}

func TestRedactLogValuerHidesStruct(t *testing.T) {
	var buf bytes.Buffer
	log := newLogger(&buf)

	// Logar a credencial via slog.Any não deve vazar o campo, pois o tipo embute Redact
	// (LogValuer vence o despejo de struct).
	cred := credential{APIKey: "LEAKME"}
	log.Info("have cred", slog.Any("cred", cred))

	out := buf.String()
	require.NotContains(t, out, "LEAKME")
	require.Contains(t, out, "[REDACTED]")
}
