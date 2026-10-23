package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/redis/go-redis/v9"

	dbgen "github.com/RodolfoBonis/rbnotes-api/db/gen"
	"github.com/RodolfoBonis/rbnotes-api/internal/ai/claude"
	"github.com/RodolfoBonis/rbnotes-api/internal/ai/stream"
	"github.com/RodolfoBonis/rbnotes-api/internal/platform/config"
	"github.com/RodolfoBonis/rbnotes-api/internal/platform/metrics"
	"github.com/RodolfoBonis/rbnotes-api/internal/platform/queue"
)

// Kinds de pedido de IA (também o ai_jobs.kind).
const (
	KindEnrich         = "note_enrich"
	KindChat           = "note_chat"
	KindAsk            = "ask"
	KindCollectionRule = "collection_rule"
	KindDiaryMonth     = "diary_month"
	KindDailyPrompt    = "daily_prompt"
)

// queueKind é o kind na fila (jobs). Todos os pedidos de IA usam o mesmo; ai_jobs.kind distingue.
const queueKind = "ai_job"

// AllowedTools é vazio: o assistente trabalha só com o conteúdo do prompt (sem web/arquivos).
const AllowedTools = ""

// Limites.
const (
	maxAskCandidates = 12
	maxDiaryNotes    = 60
	maxDailyNotes    = 8
	maxChatMessage   = 8000
)

// Erros de domínio expostos à API.
var (
	ErrDailyLimit = errors.New("limite diário de IA atingido")
	ErrBadModel   = errors.New("modelo não permitido")
	ErrBadInput   = errors.New("entrada inválida")
)

// Runner executa o modelo (claude.Runner em produção; fake nos testes).
type Runner interface {
	Run(ctx context.Context, req claude.Request, onEvent func(claude.Event)) (claude.Result, error)
}

// Deps injeta as dependências do serviço. Runner/Workdir só são necessários no ai-worker.
type Deps struct {
	Queries    *dbgen.Queries
	Queue      queue.Queue
	Redis      *redis.Client
	Runner     Runner
	Model      string
	Workdir    string
	DailyLimit int
	Logger     *slog.Logger
}

// Service coordena pedidos (API) e execuções (ai-worker).
type Service struct {
	q          *dbgen.Queries
	queue      queue.Queue
	rdb        *redis.Client
	runner     Runner
	model      string
	workdir    string
	dailyLimit int
	logger     *slog.Logger
}

// New constrói o serviço.
func New(d Deps) *Service {
	m := d.Model
	if m == "" {
		m = "sonnet"
	}
	return &Service{q: d.Queries, queue: d.Queue, rdb: d.Redis, runner: d.Runner, model: m, workdir: d.Workdir, dailyLimit: d.DailyLimit, logger: d.Logger}
}

// jobPayload é o payload do job na fila.
type jobPayload struct {
	JobID string `json:"job_id"`
}

/* ── Entradas (decodificadas pela API) ─────────────────────────────────────────────────── */

// EnrichInput é o corpo de POST /ai/notes/{id}/enrich.
type EnrichInput struct {
	Transcript   []Segment `json:"transcript"`
	Speakers     []string  `json:"speakers"`
	Folder       string    `json:"folder"`
	Style        string    `json:"style"`
	Length       string    `json:"length"`
	Model        string    `json:"model"`
	ExtractTasks bool      `json:"extractTasks"`
}

// ChatInput é o corpo de POST /ai/notes/{id}/chat. Transcript é opcional (ancora as citações).
type ChatInput struct {
	ThreadID   string    `json:"threadId"`
	Message    string    `json:"message"`
	Transcript []Segment `json:"transcript"`
	Model      string    `json:"model"`
}

// AskInput é o corpo de POST /ai/ask.
type AskInput struct {
	Question string `json:"question"`
	Model    string `json:"model"`
}

// RuleInput é o corpo de POST /ai/collections/rule.
type RuleInput struct {
	Description string `json:"description"`
	Model       string `json:"model"`
}

// DiaryInput é o corpo de POST /ai/diary/month.
type DiaryInput struct {
	Month string `json:"month"`
	Model string `json:"model"`
}

// PromptInput é a query de GET /ai/diary/prompt.
type PromptInput struct {
	Date  string `json:"date"`
	Model string `json:"model"`
}

/* ── Pedidos (API) ─────────────────────────────────────────────────────────────────────── */

// DailyLimit é o teto diário de pedidos por usuário (0 = sem teto).
func (s *Service) DailyLimit() int { return s.dailyLimit }

// RequestEnrich enfileira o enriquecimento da nota.
func (s *Service) RequestEnrich(ctx context.Context, userID pgtype.UUID, noteID string, in EnrichInput) (dbgen.AiJob, error) {
	if noteID == "" || len(in.Transcript) == 0 {
		return dbgen.AiJob{}, fmt.Errorf("%w: transcript obrigatório", ErrBadInput)
	}
	return s.request(ctx, userID, KindEnrich, noteID, "", in.Model, in)
}

// RequestChat enfileira uma resposta do chat.
func (s *Service) RequestChat(ctx context.Context, userID pgtype.UUID, in ChatInput) (dbgen.AiJob, error) {
	in.Message = strings.TrimSpace(in.Message)
	if in.Message == "" {
		return dbgen.AiJob{}, fmt.Errorf("%w: message obrigatório", ErrBadInput)
	}
	if len([]rune(in.Message)) > maxChatMessage {
		in.Message = string([]rune(in.Message)[:maxChatMessage])
	}
	return s.request(ctx, userID, KindChat, "", in.ThreadID, in.Model, in)
}

// RequestAsk enfileira uma pergunta sobre a base de notas.
func (s *Service) RequestAsk(ctx context.Context, userID pgtype.UUID, in AskInput) (dbgen.AiJob, error) {
	in.Question = strings.TrimSpace(in.Question)
	if in.Question == "" {
		return dbgen.AiJob{}, fmt.Errorf("%w: question obrigatório", ErrBadInput)
	}
	return s.request(ctx, userID, KindAsk, "", "", in.Model, in)
}

// RequestCollectionRule enfileira a geração de uma regra de coleção.
func (s *Service) RequestCollectionRule(ctx context.Context, userID pgtype.UUID, in RuleInput) (dbgen.AiJob, error) {
	in.Description = strings.TrimSpace(in.Description)
	if in.Description == "" {
		return dbgen.AiJob{}, fmt.Errorf("%w: description obrigatório", ErrBadInput)
	}
	return s.request(ctx, userID, KindCollectionRule, "", "", in.Model, in)
}

// RequestDiaryMonth enfileira o resumo de um mês ("YYYY-MM").
func (s *Service) RequestDiaryMonth(ctx context.Context, userID pgtype.UUID, in DiaryInput) (dbgen.AiJob, error) {
	if _, err := time.Parse("2006-01", in.Month); err != nil {
		return dbgen.AiJob{}, fmt.Errorf("%w: month deve ser YYYY-MM", ErrBadInput)
	}
	return s.request(ctx, userID, KindDiaryMonth, "", "", in.Model, in)
}

// RequestDailyPrompt enfileira o disparador de escrita de um dia ("YYYY-MM-DD").
func (s *Service) RequestDailyPrompt(ctx context.Context, userID pgtype.UUID, in PromptInput) (dbgen.AiJob, error) {
	if in.Date == "" {
		in.Date = time.Now().Format("2006-01-02")
	}
	if _, err := time.Parse("2006-01-02", in.Date); err != nil {
		return dbgen.AiJob{}, fmt.Errorf("%w: date deve ser YYYY-MM-DD", ErrBadInput)
	}
	return s.request(ctx, userID, KindDailyPrompt, "", "", in.Model, in)
}

// request valida o modelo, aplica a cota, cria o ai_jobs e enfileira o job.
func (s *Service) request(ctx context.Context, userID pgtype.UUID, kind, noteID, threadID, model string, input any) (dbgen.AiJob, error) {
	m, err := s.pickModel(model)
	if err != nil {
		return dbgen.AiJob{}, err
	}
	if err := s.checkQuota(ctx, userID, kind); err != nil {
		return dbgen.AiJob{}, err
	}
	inputJSON, err := json.Marshal(input)
	if err != nil {
		return dbgen.AiJob{}, fmt.Errorf("ai: serializar input: %w", err)
	}
	job, err := s.q.CreateAIJob(ctx, dbgen.CreateAIJobParams{
		UserID: userID, Kind: kind, NoteID: noteID, ThreadID: threadID, Model: m, Input: inputJSON,
	})
	if err != nil {
		return job, fmt.Errorf("ai: criar job: %w", err)
	}
	id := uuidString(job.ID)
	if _, _, err := s.queue.Enqueue(ctx, queue.EnqueueParams{
		Kind: queueKind, Payload: jobPayload{JobID: id}, DedupeKey: queueKind + ":" + id, MaxAttempts: 5,
	}); err != nil {
		return job, fmt.Errorf("ai: enfileirar: %w", err)
	}
	s.status(ctx, id, "Na fila")
	return job, nil
}

// pickModel resolve o modelo pedido contra a whitelist (vazio = padrão do serviço).
func (s *Service) pickModel(model string) (string, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return s.model, nil
	}
	if !config.AllowedModels[model] {
		return "", fmt.Errorf("%w: %q (use sonnet, opus ou haiku)", ErrBadModel, model)
	}
	return model, nil
}

// checkQuota aplica o teto diário e registra o pedido.
func (s *Service) checkQuota(ctx context.Context, userID pgtype.UUID, kind string) error {
	if s.dailyLimit > 0 {
		n, err := s.q.CountAIRequestsSince(ctx, dbgen.CountAIRequestsSinceParams{
			UserID: userID, CreatedAt: pgtype.Timestamptz{Time: time.Now().Add(-24 * time.Hour), Valid: true},
		})
		if err != nil {
			return fmt.Errorf("ai: contar cota: %w", err)
		}
		if n >= int64(s.dailyLimit) {
			return ErrDailyLimit
		}
	}
	return s.q.InsertAIRequest(ctx, dbgen.InsertAIRequestParams{UserID: userID, Kind: kind})
}

// status publica um aviso avulso na stream (ex.: "Na fila"), sem encerrá-la.
func (s *Service) status(ctx context.Context, id, msg string) {
	if s.rdb == nil {
		return
	}
	if err := s.rdb.XAdd(ctx, &redis.XAddArgs{Stream: stream.Key(id), Values: map[string]any{"t": stream.TypeStatus, "d": msg}}).Err(); err != nil {
		s.logger.Warn("ai: publicar status falhou", slog.String("err", err.Error()))
	}
}

/* ── Execução (ai-worker) ──────────────────────────────────────────────────────────────── */

// Handle executa um job de IA. Erro não-permanente = reagendar (ex.: limite de uso).
func (s *Service) Handle(ctx context.Context, job queue.Job) error {
	var p jobPayload
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		return queue.Permanent(err)
	}
	id, err := parseUUID(p.JobID)
	if err != nil {
		return queue.Permanent(err)
	}
	aj, err := s.q.GetAIJob(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // apagado nesse meio tempo
	}
	if err != nil {
		return fmt.Errorf("ai: carregar job: %w", err)
	}
	if aj.Status == "done" || aj.Status == "error" {
		return nil // já resolvido
	}
	if err := s.q.SetAIJobRunning(ctx, id); err != nil {
		return fmt.Errorf("ai: marcar running: %w", err)
	}
	pub := stream.NewPublisher(ctx, s.rdb, p.JobID)
	switch aj.Kind {
	case KindEnrich:
		return s.handleEnrich(ctx, aj, pub)
	case KindChat:
		return s.handleChat(ctx, aj, pub)
	case KindAsk:
		return s.handleAsk(ctx, aj, pub)
	case KindCollectionRule:
		return s.handleSimple(ctx, aj, pub, CollectionRuleSchema, collectionRuleText(aj), ValidateCollectionRule)
	case KindDiaryMonth:
		return s.handleDiary(ctx, aj, pub)
	case KindDailyPrompt:
		return s.handleDailyPrompt(ctx, aj, pub)
	default:
		return queue.Permanent(fmt.Errorf("ai: kind desconhecido %q", aj.Kind))
	}
}

// run executa a CLI repassando os eventos à stream e tratando os resultados comuns. Devolve o JSON
// estruturado validado por validate, ou um erro (permanente para falha de modelo; não-permanente
// para rate limit / falha de infra).
func (s *Service) run(ctx context.Context, aj dbgen.AiJob, pub *stream.Publisher, req claude.Request, validate func([]byte) ([]byte, error)) ([]byte, error) {
	req.SystemPrompt = systemPrompt
	req.Model = aj.Model
	req.AllowedTools = AllowedTools
	res, err := s.runner.Run(ctx, req, forward(ctx, pub))
	if err == nil && res.NoConversation && req.ResumeID != "" {
		// Sessão perdida (pod recriado): refaz sem resume.
		req.ResumeID = ""
		res, err = s.runner.Run(ctx, req, forward(ctx, pub))
	}
	if err != nil {
		s.closeStream(ctx, pub, stream.TypeStatus, "Falha ao iniciar a IA — tentando de novo")
		return nil, err
	}
	if res.RateLimited {
		metrics.AIJob(aj.Kind, "rate_limited")
		s.closeStream(ctx, pub, stream.TypeStatus, "Limite de uso da IA atingido — nova tentativa em breve")
		return nil, errors.New("ai: limite de uso")
	}
	if res.Err != "" {
		return nil, s.fail(ctx, aj, pub, res.Err)
	}
	content, verr := validate(res.Structured)
	if verr != nil {
		return nil, s.fail(ctx, aj, pub, verr.Error())
	}
	return content, nil
}

// fail marca o job com erro, encerra a stream e devolve um erro permanente (não reprocessa). O
// bookkeeping (FailAIJob + fechar a stream) roda num contexto WithoutCancel: um deadline do job
// (jobCtx cancelado) não pode impedir a marcação do erro nem a notificação da UI.
func (s *Service) fail(ctx context.Context, aj dbgen.AiJob, pub *stream.Publisher, cause string) error {
	metrics.AIJob(aj.Kind, "error")
	s.logger.Warn("ai: job falhou", slog.String("kind", aj.Kind), slog.String("job_id", uuidString(aj.ID)), slog.String("err", truncate(cause, 300)))
	msg := userMessage(cause)
	bg, cancel := bookkeepingCtx(ctx)
	defer cancel()
	if err := s.q.FailAIJob(bg, dbgen.FailAIJobParams{ID: aj.ID, Error: msg}); err != nil {
		s.logger.ErrorContext(bg, "ai: marcar job como erro falhou", slog.String("job_id", uuidString(aj.ID)), slog.String("err", err.Error()))
	}
	s.closeStream(bg, pub, stream.TypeError, msg)
	return queue.Permanent(errors.New(cause))
}

// done persiste o resultado, fecha a stream com sucesso e conta a métrica. O bookkeeping roda em
// WithoutCancel para sobreviver a um cancelamento tardio do pai.
func (s *Service) done(ctx context.Context, aj dbgen.AiJob, pub *stream.Publisher, content []byte) error {
	bg, cancel := bookkeepingCtx(ctx)
	defer cancel()
	if err := s.q.CompleteAIJob(bg, dbgen.CompleteAIJobParams{ID: aj.ID, Result: content, Model: aj.Model}); err != nil {
		return err
	}
	metrics.AIJob(aj.Kind, "ok")
	s.closeStream(bg, pub, stream.TypeDone, "")
	return nil
}

// giveUp encerra o pedido cujas tentativas acabaram por erro transitório (ex.: limite de uso
// persistente), para a UI não ficar esperando para sempre. Chamado pelo worker.
func (s *Service) giveUp(ctx context.Context, job queue.Job) {
	const msg = "A IA está indisponível agora (limite de uso). Tente de novo mais tarde."
	var p jobPayload
	if json.Unmarshal(job.Payload, &p) != nil {
		return
	}
	id, err := parseUUID(p.JobID)
	if err != nil {
		return
	}
	bg, cancel := bookkeepingCtx(ctx)
	defer cancel()
	if err := s.q.FailAIJob(bg, dbgen.FailAIJobParams{ID: id, Error: msg}); err != nil {
		s.logger.ErrorContext(bg, "ai: giveUp FailAIJob falhou", slog.String("job_id", p.JobID), slog.String("err", err.Error()))
	}
	s.closeStream(bg, stream.NewPublisher(bg, s.rdb, p.JobID), stream.TypeError, msg)
}

/* ── helpers ──────────────────────────────────────────────────────────────────────────── */

// bookkeepingTimeout é o teto dos contextos de pós-processamento (marcar erro/concluído, fechar a
// stream). Curto: só precisa de uma escrita no banco e um XADD.
const bookkeepingTimeout = 10 * time.Second

// bookkeepingCtx deriva um contexto que NÃO herda o cancelamento do pai (WithoutCancel) com um
// deadline curto: usado para o bookkeeping após um job, resistindo ao SIGTERM/deadline do job.
func bookkeepingCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), bookkeepingTimeout)
}

// closeStream fecha a stream publicando o evento final e LOGA a falha (antes era silenciosa).
func (s *Service) closeStream(ctx context.Context, pub *stream.Publisher, typ, data string) {
	if pub == nil {
		return
	}
	if err := pub.Close(ctx, typ, data); err != nil {
		s.logger.ErrorContext(ctx, "ai: fechar stream falhou", slog.String("type", typ), slog.String("err", err.Error()))
	}
}


// forward repassa os eventos da CLI para a stream (saída estruturada → eventos json).
func forward(ctx context.Context, pub *stream.Publisher) func(claude.Event) {
	return func(e claude.Event) {
		switch e.Type {
		case claude.EventText:
			pub.Emit(ctx, stream.TypeText, e.Text)
		case claude.EventJSON:
			pub.Emit(ctx, stream.TypeJSON, e.Text)
		case claude.EventTool:
			pub.Emit(ctx, stream.TypeTool, e.Text)
		}
	}
}

func (s *Service) dir(parts ...string) string {
	d := filepath.Join(append([]string{s.workdir}, parts...)...)
	if err := os.MkdirAll(d, 0o750); err != nil {
		s.logger.Warn("ai: workdir", slog.String("err", err.Error()))
	}
	return d
}

// userMessage traduz o erro técnico em algo exibível (sem detalhes internos).
func userMessage(msg string) string {
	switch {
	case strings.Contains(msg, "tempo limite"):
		return "A IA demorou demais para responder. Tente de novo."
	case strings.Contains(msg, ErrInvalidOutput.Error()) || strings.Contains(msg, "saída estruturada"):
		return "A IA devolveu um resultado incompleto. Tente de novo."
	default:
		return "A IA não conseguiu responder. Tente de novo."
	}
}

func parseUUID(s string) (pgtype.UUID, error) {
	var u pgtype.UUID
	if err := u.Scan(s); err != nil {
		return u, fmt.Errorf("uuid inválido %q: %w", s, err)
	}
	return u, nil
}

func uuidString(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	b := u.Bytes
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
