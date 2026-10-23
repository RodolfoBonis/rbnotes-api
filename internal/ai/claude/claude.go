// Package claude executa a CLI do Claude Code em modo headless (padrão do runner do Jarvis) e
// transmite a saída em tempo real:
//
//	claude -p --output-format stream-json --verbose --include-partial-messages
//	       [--json-schema <schema>] [--resume <id>] [--model <m>] --allowedTools WebSearch,WebFetch
//
// O prompt vai pelo STDIN (nunca vira flag). O processo roda com ambiente mínimo por allowlist (sem
// herdar as credenciais do worker), HOME estável (as sessões do --resume vivem em ~/.claude) e grupo
// de processo próprio, morto inteiro no timeout.
//
// Cada linha do stream-json vira, quando interessa à UI, um Event:
//   - text: pedaço de texto da resposta (chat);
//   - json: pedaço do JSON da saída estruturada (guias — a UI renderiza o JSON parcial);
//   - tool: o modelo usou uma ferramenta (ex.: pesquisa na web), com a consulta.
//
// Falhas da execução (timeout, saída vazia, erro da CLI, limite de uso) voltam em Result.Err /
// Result.RateLimited; erro de Go só em falha de infra (não conseguiu lançar o processo).
package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"

	"github.com/google/uuid"
)

// Request descreve uma execução.
type Request struct {
	Prompt string
	// SystemPrompt é anexado ao system prompt padrão (--append-system-prompt).
	SystemPrompt string
	Model        string
	// JSONSchema, quando não-vazio, exige saída estruturada (Result.Structured).
	JSONSchema string
	// ResumeID continua uma sessão anterior (só aceito se for UUID).
	ResumeID string
	// Workdir é o cwd da CLI. As sessões do --resume são guardadas por cwd: retomar exige o mesmo.
	Workdir string
	// AllowedTools liberadas sem prompt (vazio = nenhuma ferramenta).
	AllowedTools string
}

// EventType classifica um Event do stream.
type EventType string

// Tipos de Event.
const (
	EventText EventType = "text" // pedaço de texto (chat)
	EventJSON EventType = "json" // pedaço do JSON da saída estruturada
	EventTool EventType = "tool" // uso de ferramenta (pesquisa/leitura na web)
)

// Event é um pedaço da resposta em tempo real.
type Event struct {
	Type EventType
	Text string
}

// Result é o resultado final da execução.
type Result struct {
	Text       string
	Structured json.RawMessage
	SessionID  string
	CostUSD    float64
	// RateLimited: a assinatura atingiu o limite de uso (reagendar, não é erro do pedido).
	RateLimited bool
	// NoConversation: a sessão do --resume não existe mais (refazer sem resume).
	NoConversation bool
	Err            string
}

// Runner executa a CLI.
type Runner struct {
	bin        string
	oauthToken string
	home       string
	logger     *slog.Logger
}

// New constrói o runner. bin vazio usa "claude" no PATH; token é o CLAUDE_CODE_OAUTH_TOKEN
// (SEGREDO: só vai no ambiente do processo filho, nunca é logado); home é o HOME estável da CLI.
func New(bin, oauthToken, home string, logger *slog.Logger) *Runner {
	if bin == "" {
		bin = "claude"
	}
	return &Runner{bin: bin, oauthToken: oauthToken, home: home, logger: logger}
}

// Run executa a CLI, chamando onEvent a cada pedaço da resposta (na goroutine do chamador).
func (r *Runner) Run(ctx context.Context, req Request, onEvent func(Event)) (Result, error) {
	cmd := exec.CommandContext(ctx, r.bin, r.buildArgs(req)...) //nolint:gosec // G204: bin é config; args fixos; o prompt vai por stdin.
	cmd.Dir = req.Workdir
	cmd.Stdin = strings.NewReader(req.Prompt)
	cmd.Env = r.minimalEnv()
	setProcessGroup(cmd)

	var stderr strings.Builder
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, fmt.Errorf("claude: stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("claude: falha ao executar a CLI: %w", err)
	}

	res, sawResult := parseStream(stdout, onEvent)
	runErr := cmd.Wait()

	if ctx.Err() != nil {
		return Result{Err: "tempo limite da execução excedido"}, nil
	}
	var exitErr *exec.ExitError
	if runErr != nil && !errors.As(runErr, &exitErr) {
		return Result{}, fmt.Errorf("claude: %w", runErr)
	}
	errOut := stderr.String()
	if strings.Contains(errOut, "No conversation found") {
		return Result{NoConversation: true, Err: "sessão não encontrada"}, nil
	}
	if !sawResult {
		if isRateLimit(errOut) {
			return Result{RateLimited: true, Err: "limite de uso atingido"}, nil
		}
		r.logger.WarnContext(ctx, "claude: execução sem resultado", slog.Bool("exit_err", runErr != nil),
			slog.String("stderr", truncate(errOut, 500)))
		return Result{Err: "a CLI não produziu resultado"}, nil
	}
	if req.JSONSchema != "" && res.Err == "" && len(res.Structured) == 0 {
		res.Err = "a resposta não trouxe a saída estruturada"
	}
	return res, nil
}

// buildArgs monta os argumentos fixos. O prompt NÃO entra aqui (vai por stdin).
func (r *Runner) buildArgs(req Request) []string {
	args := []string{"-p", "--output-format", "stream-json", "--verbose", "--include-partial-messages",
		"--allowedTools", req.AllowedTools}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	if req.SystemPrompt != "" {
		args = append(args, "--append-system-prompt", req.SystemPrompt)
	}
	if req.JSONSchema != "" {
		args = append(args, "--json-schema", req.JSONSchema)
	}
	if req.ResumeID != "" {
		if _, err := uuid.Parse(req.ResumeID); err == nil {
			args = append(args, "--resume", req.ResumeID)
		}
	}
	return args
}

// minimalEnv: allowlist, sem os.Environ() (que vazaria os segredos do worker para a CLI).
func (r *Runner) minimalEnv() []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"CLAUDE_CODE_OAUTH_TOKEN=" + r.oauthToken, // SEGREDO: nunca logado.
		"DISABLE_AUTOUPDATER=1",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
	}
	if r.home != "" {
		env = append(env, "HOME="+r.home)
	}
	for _, key := range []string{"LANG", "TMPDIR"} {
		if v := os.Getenv(key); v != "" {
			env = append(env, key+"="+v)
		}
	}
	return env
}

// streamLine cobre os campos do stream-json que interessam.
type streamLine struct {
	Type  string `json:"type"`
	Event *struct {
		Type         string `json:"type"`
		ContentBlock *struct {
			Type string `json:"type"`
			Name string `json:"name"`
		} `json:"content_block"`
		Delta *struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			PartialJSON string `json:"partial_json"`
		} `json:"delta"`
	} `json:"event"`
	Message *struct {
		Content []struct {
			Type  string          `json:"type"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	} `json:"message"`
	// result
	Subtype          string          `json:"subtype"`
	IsError          bool            `json:"is_error"`
	Result           string          `json:"result"`
	StructuredOutput json.RawMessage `json:"structured_output"`
	SessionID        string          `json:"session_id"`
	TotalCostUSD     float64         `json:"total_cost_usd"`
}

// structuredTool é o nome da ferramenta pela qual a CLI entrega a saída do --json-schema.
const structuredTool = "StructuredOutput"

// parseStream consome o stream-json até o fim. Separada para teste.
func parseStream(rd io.Reader, onEvent func(Event)) (Result, bool) {
	if onEvent == nil {
		onEvent = func(Event) {}
	}
	sc := bufio.NewScanner(rd)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	var (
		res       Result
		sawResult bool
		inJSON    bool
	)
	for sc.Scan() {
		var ln streamLine
		if json.Unmarshal(sc.Bytes(), &ln) != nil {
			continue
		}
		switch ln.Type {
		case "stream_event":
			if ln.Event == nil {
				continue
			}
			switch ln.Event.Type {
			case "content_block_start":
				inJSON = ln.Event.ContentBlock != nil && ln.Event.ContentBlock.Type == "tool_use" &&
					ln.Event.ContentBlock.Name == structuredTool
			case "content_block_stop":
				inJSON = false
			case "content_block_delta":
				d := ln.Event.Delta
				switch {
				case d == nil:
				case d.Type == "text_delta" && d.Text != "":
					onEvent(Event{Type: EventText, Text: d.Text})
				case d.Type == "input_json_delta" && inJSON && d.PartialJSON != "":
					onEvent(Event{Type: EventJSON, Text: d.PartialJSON})
				}
			}
		case "assistant":
			if ln.Message == nil {
				continue
			}
			for _, c := range ln.Message.Content {
				if c.Type != "tool_use" || c.Name == structuredTool {
					continue
				}
				if desc := describeTool(c.Name, c.Input); desc != "" {
					onEvent(Event{Type: EventTool, Text: desc})
				}
			}
		case "result":
			sawResult = true
			res.SessionID = ln.SessionID
			res.CostUSD = ln.TotalCostUSD
			res.Text = ln.Result
			if len(ln.StructuredOutput) > 0 && string(ln.StructuredOutput) != "null" {
				res.Structured = ln.StructuredOutput
			}
			if ln.IsError {
				if isRateLimit(ln.Result) || isRateLimit(ln.Subtype) {
					res.RateLimited = true
					res.Err = "limite de uso atingido"
				} else {
					res.Err = firstNonEmpty(ln.Result, ln.Subtype, "a CLI retornou erro sem mensagem")
				}
			}
		}
	}
	return res, sawResult
}

// describeTool resume o uso de ferramenta para a UI ("Pesquisando: …", "Lendo: url"). Ferramentas
// internas da CLI (ex.: ToolSearch) não interessam ao usuário: vazio = não publicar.
func describeTool(name string, input json.RawMessage) string {
	var in struct {
		Query string `json:"query"`
		URL   string `json:"url"`
	}
	_ = json.Unmarshal(input, &in)
	switch name {
	case "WebSearch":
		if in.Query != "" {
			return "Pesquisando: " + truncate(in.Query, 120)
		}
		return "Pesquisando na web"
	case "WebFetch":
		if in.URL != "" {
			return "Lendo: " + truncate(in.URL, 120)
		}
		return "Lendo uma página"
	default:
		return ""
	}
}

func isRateLimit(s string) bool {
	l := strings.ToLower(s)
	return strings.Contains(l, "rate limit") || strings.Contains(l, "usage limit") ||
		strings.Contains(l, "limit reached") || strings.Contains(l, "too many requests")
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
