package claude

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestParseStreamStructured(t *testing.T) {
	f, err := os.Open("testdata/structured.jsonl")
	require.NoError(t, err)
	defer func() { _ = f.Close() }()

	var events []Event
	res, ok := parseStream(f, func(e Event) { events = append(events, e) })
	require.True(t, ok)
	require.Empty(t, res.Err)
	require.Equal(t, "58ea8a5d-1e36-4132-a4c8-53a557f97780", res.SessionID)

	var partial strings.Builder
	var tools []string
	for _, e := range events {
		switch e.Type {
		case EventJSON:
			partial.WriteString(e.Text)
		case EventTool:
			tools = append(tools, e.Text)
		case EventText:
			t.Fatalf("saída estruturada não deve gerar text: %q", e.Text)
		}
	}
	require.Equal(t, []string{"Pesquisando: Hades platinum trophy guide"}, tools)
	// Os deltas remontam exatamente o JSON final.
	var fromDeltas, final map[string]any
	require.NoError(t, json.Unmarshal([]byte(partial.String()), &fromDeltas))
	require.NoError(t, json.Unmarshal(res.Structured, &final))
	require.Equal(t, final, fromDeltas)
	require.InDelta(t, 0.85, final["confidence"], 1e-9)
}

func TestParseStreamTextAndErrors(t *testing.T) {
	lines := strings.Join([]string{
		`{"type":"stream_event","event":{"type":"content_block_start","content_block":{"type":"text"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"Olá, "}}}`,
		`não é json`,
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"caçador."}}}`,
		`{"type":"result","subtype":"success","is_error":false,"result":"Olá, caçador.","session_id":"s1"}`,
	}, "\n")
	var text strings.Builder
	res, ok := parseStream(strings.NewReader(lines), func(e Event) { text.WriteString(e.Text) })
	require.True(t, ok)
	require.Equal(t, "Olá, caçador.", text.String())
	require.Equal(t, "Olá, caçador.", res.Text)

	res, ok = parseStream(strings.NewReader(`{"type":"result","is_error":true,"result":"Claude AI usage limit reached|1760000000"}`), nil)
	require.True(t, ok)
	require.True(t, res.RateLimited)

	_, ok = parseStream(strings.NewReader(""), nil)
	require.False(t, ok)
}

func TestBuildArgs(t *testing.T) {
	r := New("", "tok", "/home", slog.New(slog.NewTextHandler(io.Discard, nil)))
	args := r.buildArgs(Request{Model: "sonnet", JSONSchema: `{}`, ResumeID: "--evil", AllowedTools: "WebSearch,WebFetch"})
	joined := strings.Join(args, " ")
	require.Contains(t, joined, "--output-format stream-json")
	require.Contains(t, joined, "--json-schema {}")
	require.NotContains(t, joined, "--resume", "resume id que não é UUID é descartado")
	// Contenção de prompt injection: o conjunto de ferramentas (--tools) e a allowlist
	// (--allowedTools) ficam alinhados, e a CLI não aceita MCP herdado nem slash commands.
	require.Contains(t, joined, "--tools WebSearch,WebFetch")
	require.Contains(t, joined, "--allowedTools WebSearch,WebFetch")
	require.Contains(t, joined, "--strict-mcp-config")
	require.Contains(t, joined, "--disable-slash-commands")
	args = r.buildArgs(Request{ResumeID: "58ea8a5d-1e36-4132-a4c8-53a557f97780"})
	require.Contains(t, strings.Join(args, " "), "--resume 58ea8a5d-1e36-4132-a4c8-53a557f97780")

	// Default (AllowedTools vazio): --tools e --allowedTools são passados como argumento vazio, de
	// modo que a CLI só expõe a StructuredOutput do --json-schema.
	def := r.buildArgs(Request{})
	require.Contains(t, def, "--tools")
	require.Contains(t, def, "--allowedTools")
	require.Contains(t, def, "--strict-mcp-config")
	require.Contains(t, def, "--disable-slash-commands")
	for i, a := range def {
		if a == "--tools" || a == "--allowedTools" {
			require.Equal(t, "", def[i+1], "default do conjunto/allowlist de ferramentas é vazio")
		}
	}
}

// fakeCLI grava um script que imita a CLI (lê o prompt do stdin, confere o ambiente).
func fakeCLI(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "claude")
	require.NoError(t, os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755)) //nolint:gosec // script de teste
	return p
}

func TestRunWithFakeCLI(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	t.Setenv("SECRET_FROM_WORKER", "nao-pode-vazar")
	bin := fakeCLI(t, `prompt=$(cat)
[ -n "$SECRET_FROM_WORKER" ] && { echo "vazou ambiente" >&2; exit 3; }
[ "$CLAUDE_CODE_OAUTH_TOKEN" = "tok" ] || { echo "sem token" >&2; exit 4; }
echo '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"eco: '"$prompt"'"}}}'
echo '{"type":"result","is_error":false,"result":"eco: '"$prompt"'","session_id":"s2"}'
`)
	r := New(bin, "tok", t.TempDir(), logger)
	var got strings.Builder
	res, err := r.Run(context.Background(), Request{Prompt: "-p oi", Workdir: t.TempDir()}, func(e Event) { got.WriteString(e.Text) })
	require.NoError(t, err)
	require.Empty(t, res.Err)
	require.Equal(t, "eco: -p oi", got.String(), "prompt vai por stdin, não vira flag")
	require.Equal(t, "s2", res.SessionID)

	gone := New(fakeCLI(t, `echo "No conversation found with session ID: x" >&2; exit 1`), "tok", "", logger)
	res, err = gone.Run(context.Background(), Request{Workdir: t.TempDir()}, nil)
	require.NoError(t, err)
	require.True(t, res.NoConversation)

	limited := New(fakeCLI(t, `echo "Claude usage limit reached" >&2; exit 1`), "tok", "", logger)
	res, err = limited.Run(context.Background(), Request{Workdir: t.TempDir()}, nil)
	require.NoError(t, err)
	require.True(t, res.RateLimited)

	slow := New(fakeCLI(t, `sleep 5`), "tok", "", logger)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	res, err = slow.Run(ctx, Request{Workdir: t.TempDir()}, nil)
	require.NoError(t, err)
	require.Contains(t, res.Err, "tempo limite")
	require.Less(t, time.Since(start), 3*time.Second, "o grupo de processo é morto no timeout")

	_, err = New(filepath.Join(t.TempDir(), "nao-existe"), "tok", "", logger).Run(context.Background(), Request{Workdir: t.TempDir()}, nil)
	require.Error(t, err, "binário ausente é falha de infra")
}

func TestParseResetUnix(t *testing.T) {
	future := time.Now().Add(time.Hour).Unix()
	got := parseResetUnix("Claude AI usage limit reached|" + strconv.FormatInt(future, 10))
	require.Equal(t, future, got.Unix())
	require.True(t, parseResetUnix("sem reset").IsZero())
	require.True(t, parseResetUnix("limit|100").IsZero(), "reset no passado é ignorado")
}

// TestParseStreamUsage lê a fixture nova capturada da CLI real (saída estruturada com consumo) e
// confere que a linha `result` é decodificada em Usage/DurationMs/NumTurns/ResolvedModel.
func TestParseStreamUsage(t *testing.T) {
	f, err := os.Open("testdata/usage.jsonl")
	require.NoError(t, err)
	defer func() { _ = f.Close() }()

	res, ok := parseStream(f, nil)
	require.True(t, ok)
	require.Empty(t, res.Err)

	require.Equal(t, int64(9), res.Usage.InputTokens)
	require.Equal(t, int64(158), res.Usage.OutputTokens)
	require.Equal(t, int64(7915), res.Usage.CacheCreationTokens)
	require.Equal(t, int64(0), res.Usage.CacheReadTokens)
	require.Equal(t, int64(3033), res.DurationMs)
	require.Equal(t, 2, res.NumTurns)
	require.Equal(t, "claude-haiku-4-5-20251001", res.ResolvedModel)
	require.InDelta(t, 0.016629, res.CostUSD, 1e-9)
	require.NotEmpty(t, res.Structured, "a fixture traz saída estruturada")
}

func TestFirstJSONKey(t *testing.T) {
	require.Equal(t, "claude-haiku-4-5-20251001",
		firstJSONKey([]byte(`{"claude-haiku-4-5-20251001":{"x":1},"outro":{}}`)))
	require.Empty(t, firstJSONKey([]byte(`{}`)))
	require.Empty(t, firstJSONKey(nil))
	require.Empty(t, firstJSONKey([]byte(`"nao-objeto"`)))
	require.Empty(t, firstJSONKey([]byte(`[1,2]`)))
}
