package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	dbgen "github.com/RodolfoBonis/rbnotes-api/db/gen"
	"github.com/RodolfoBonis/rbnotes-api/internal/ai/claude"
	"github.com/RodolfoBonis/rbnotes-api/internal/ai/stream"
	"github.com/RodolfoBonis/rbnotes-api/internal/platform/metrics"
)

// handleEnrich gera o NoteAi, persiste como entidade note_ai (sincroniza no próximo pull) e
// atualiza o índice FTS (título + tldr + transcrição).
func (s *Service) handleEnrich(ctx context.Context, aj dbgen.AiJob, pub *stream.Publisher) error {
	var in EnrichInput
	if err := json.Unmarshal(aj.Input, &in); err != nil {
		return s.fail(ctx, aj, pub, "input inválido")
	}
	req := claude.Request{Prompt: enrichPrompt(in), JSONSchema: NoteEnrichSchema, Workdir: s.dir("enrich")}
	content, err := s.run(ctx, aj, pub, req, func(raw []byte) ([]byte, error) {
		return ValidateNoteAi(raw, in.Transcript)
	})
	if err != nil || content == nil {
		return err
	}
	// Nota apagada enquanto o enrich rodava: não reindexa (some da busca/ask) nem mantém a citação.
	deleted, derr := s.noteDeleted(ctx, aj.UserID, aj.NoteID)
	if derr != nil {
		return derr
	}
	if deleted {
		content = stripQuote(content)
		if err := s.writeEntity(ctx, aj.UserID, "note_ai", aj.NoteID, content); err != nil {
			return err
		}
		return s.done(ctx, aj, pub, content)
	}
	if err := s.writeEntity(ctx, aj.UserID, "note_ai", aj.NoteID, content); err != nil {
		return err
	}
	if err := s.indexEnrich(ctx, aj.UserID, aj.NoteID, content, in.Transcript); err != nil {
		return err
	}
	return s.done(ctx, aj, pub, content)
}

// noteDeleted indica se a nota foi apagada (ou não existe mais) no momento do enrich.
func (s *Service) noteDeleted(ctx context.Context, userID pgtype.UUID, noteID string) (bool, error) {
	n, err := s.q.GetNote(ctx, dbgen.GetNoteParams{UserID: userID, ID: noteID})
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("ai: carregar nota: %w", err)
	}
	return n.Deleted, nil
}

// stripQuote remove a citação do enrich (usado quando a nota foi apagada: não citar trechos).
func stripQuote(content []byte) []byte {
	var n NoteAi
	if json.Unmarshal(content, &n) != nil {
		return content
	}
	n.Quote = nil
	out, err := json.Marshal(n)
	if err != nil {
		return content
	}
	return out
}

// handleChat responde ao chat (com --resume por thread) e persiste a resposta como chat_message.
func (s *Service) handleChat(ctx context.Context, aj dbgen.AiJob, pub *stream.Publisher) error {
	var in ChatInput
	if err := json.Unmarshal(aj.Input, &in); err != nil {
		return s.fail(ctx, aj, pub, "input inválido")
	}
	resume := ""
	if in.ThreadID != "" {
		if sess, serr := s.q.GetThreadSession(ctx, dbgen.GetThreadSessionParams{UserID: aj.UserID, ThreadID: in.ThreadID}); serr == nil {
			resume = sess
		}
	}
	allowed := startMsSet(in.Transcript)
	workdir := s.dir("chat", uuidString(aj.UserID), safeDir(in.ThreadID))

	req := claude.Request{Prompt: chatPrompt(in.Message, nil, in.Transcript, resume != ""), JSONSchema: NoteChatSchema, Workdir: workdir, ResumeID: resume}
	// Se a sessão existir, o prompt completo não é necessário; o run refaz sem resume se a sessão
	// se perder. Por simplicidade reusamos o mesmo prompt (a CLI ignora o excesso com --resume).
	res, err := s.runner.Run(ctx, withDefaults(req, aj, systemPrompt), forward(ctx, pub))
	if err == nil && res.NoConversation && resume != "" {
		req.ResumeID = ""
		req.Prompt = chatPrompt(in.Message, nil, in.Transcript, false)
		res, err = s.runner.Run(ctx, withDefaults(req, aj, systemPrompt), forward(ctx, pub))
	}
	content, derr := s.finishRun(ctx, aj, pub, res, err, func(raw []byte) ([]byte, error) {
		return ValidateChat(raw, allowed)
	})
	if content == nil {
		return derr
	}
	if res.SessionID != "" && in.ThreadID != "" {
		if serr := s.q.UpsertThreadSession(ctx, dbgen.UpsertThreadSessionParams{UserID: aj.UserID, ThreadID: in.ThreadID, ClaudeSessionID: res.SessionID}); serr != nil {
			s.logger.Warn("ai: gravar sessão falhou", "err", serr.Error())
		}
	}
	msg := chatMessageData(in.ThreadID, content)
	if err := s.writeEntity(ctx, aj.UserID, "chat_message", uuidString(aj.ID), msg); err != nil {
		return err
	}
	return s.done(ctx, aj, pub, content)
}

// handleAsk busca candidatas por FTS + recência e responde com fontes só dentre elas.
func (s *Service) handleAsk(ctx context.Context, aj dbgen.AiJob, pub *stream.Publisher) error {
	var in AskInput
	if err := json.Unmarshal(aj.Input, &in); err != nil {
		return s.fail(ctx, aj, pub, "input inválido")
	}
	cands, byID, err := s.candidates(ctx, aj.UserID, in.Question)
	if err != nil {
		return fmt.Errorf("ai: candidatas: %w", err)
	}
	req := claude.Request{Prompt: askPrompt(in.Question, cands), JSONSchema: AskSchema, Workdir: s.dir("ask")}
	content, rerr := s.run(ctx, aj, pub, req, func(raw []byte) ([]byte, error) {
		return ValidateAsk(raw, byID)
	})
	if content == nil {
		return rerr
	}
	return s.done(ctx, aj, pub, content)
}

// handleSimple cobre os kinds que só mandam um prompt e validam a saída (collection_rule).
func (s *Service) handleSimple(ctx context.Context, aj dbgen.AiJob, pub *stream.Publisher, schema, prompt string, validate func([]byte) ([]byte, error)) error {
	req := claude.Request{Prompt: prompt, JSONSchema: schema, Workdir: s.dir(aj.Kind)}
	content, err := s.run(ctx, aj, pub, req, validate)
	if content == nil {
		return err
	}
	return s.done(ctx, aj, pub, content)
}

// handleDiary resume o mês a partir das notas conhecidas (índice de busca como pool de contexto).
func (s *Service) handleDiary(ctx context.Context, aj dbgen.AiJob, pub *stream.Publisher) error {
	var in DiaryInput
	if err := json.Unmarshal(aj.Input, &in); err != nil {
		return s.fail(ctx, aj, pub, "input inválido")
	}
	notes, err := s.recent(ctx, aj.UserID, maxDiaryNotes)
	if err != nil {
		return fmt.Errorf("ai: notas do diário: %w", err)
	}
	req := claude.Request{Prompt: diaryMonthPrompt(in.Month, notes), JSONSchema: DiaryMonthSchema, Workdir: s.dir("diary")}
	content, rerr := s.run(ctx, aj, pub, req, ValidateDiaryMonth)
	if content == nil {
		return rerr
	}
	return s.done(ctx, aj, pub, content)
}

// handleDailyPrompt gera o disparador do dia (informado por notas recentes).
func (s *Service) handleDailyPrompt(ctx context.Context, aj dbgen.AiJob, pub *stream.Publisher) error {
	var in PromptInput
	if err := json.Unmarshal(aj.Input, &in); err != nil {
		return s.fail(ctx, aj, pub, "input inválido")
	}
	notes, err := s.recent(ctx, aj.UserID, maxDailyNotes)
	if err != nil {
		return fmt.Errorf("ai: notas recentes: %w", err)
	}
	req := claude.Request{Prompt: dailyPromptPrompt(in.Date, notes), JSONSchema: DailyPromptSchema, Workdir: s.dir("daily")}
	content, rerr := s.run(ctx, aj, pub, req, ValidateDailyPrompt)
	if content == nil {
		return rerr
	}
	return s.done(ctx, aj, pub, content)
}

// collectionRuleText monta o prompt de collection_rule a partir do input do job.
func collectionRuleText(aj dbgen.AiJob) string {
	var in RuleInput
	_ = json.Unmarshal(aj.Input, &in)
	return collectionRulePrompt(in.Description)
}

/* ── apoio ────────────────────────────────────────────────────────────────────────────── */

// finishRun trata o resultado de uma execução já rodada (usado pelo chat, que faz o run manualmente
// por causa do --resume). Devolve (content, err); content nil = parar (err já tratado).
func (s *Service) finishRun(ctx context.Context, aj dbgen.AiJob, pub *stream.Publisher, res claude.Result, runErr error, validate func([]byte) ([]byte, error)) ([]byte, error) {
	if runErr != nil {
		s.closeStream(ctx, pub, stream.TypeStatus, "Falha ao iniciar a IA — tentando de novo")
		return nil, runErr
	}
	if res.RateLimited {
		metrics.AIJob(aj.Kind, "rate_limited")
		s.closeStream(ctx, pub, stream.TypeStatus, "Limite de uso da IA atingido — nova tentativa em breve")
		return nil, fmt.Errorf("ai: limite de uso")
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

// withDefaults preenche os campos fixos da request (system prompt, modelo, tools).
func withDefaults(req claude.Request, aj dbgen.AiJob, system string) claude.Request {
	req.SystemPrompt = system
	req.Model = aj.Model
	req.AllowedTools = AllowedTools
	return req
}

// writeEntity grava uma entidade sincronizável produzida pela IA. A alocação do change_seq
// (NextChangeSeq) e o upsert rodam na MESMA transação: o lock da linha em sync_cursors fica retido
// até o upsert commitar, serializando contra o Push do sync. Sem isso, um Push concorrente poderia
// alocar um change_seq MAIOR e commitar antes deste upsert, fazendo o cursor de um pull avançar além
// desta linha — que o pull então pularia para sempre.
func (s *Service) writeEntity(ctx context.Context, userID pgtype.UUID, entity, id string, data []byte) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("ai: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)
	seq, err := q.NextChangeSeq(ctx, userID)
	if err != nil {
		return fmt.Errorf("ai: change_seq: %w", err)
	}
	ts := pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}
	switch entity {
	case "note_ai":
		err = q.UpsertNoteAi(ctx, dbgen.UpsertNoteAiParams{UserID: userID, ID: id, UpdatedAt: ts, Deleted: false, Data: data, ChangeSeq: seq})
	case "chat_message":
		err = q.UpsertChatMessage(ctx, dbgen.UpsertChatMessageParams{UserID: userID, ID: id, UpdatedAt: ts, Deleted: false, Data: data, ChangeSeq: seq})
	default:
		return fmt.Errorf("ai: entidade inesperada %q", entity)
	}
	if err != nil {
		return fmt.Errorf("ai: upsert %s: %w", entity, err)
	}
	return tx.Commit(ctx)
}

// indexEnrich atualiza o índice FTS a partir do enrich (título + tldr + transcrição).
func (s *Service) indexEnrich(ctx context.Context, userID pgtype.UUID, noteID string, content []byte, transcript []Segment) error {
	var n NoteAi
	if err := json.Unmarshal(content, &n); err != nil {
		return nil // conteúdo já validado; não bloqueia o fluxo
	}
	var tb strings.Builder
	for _, seg := range transcript {
		tb.WriteString(seg.Text)
		tb.WriteByte(' ')
	}
	return s.q.UpsertNoteSearchEnrich(ctx, dbgen.UpsertNoteSearchEnrichParams{
		UserID: userID, NoteID: noteID, Title: n.Title, Tldr: n.Tldr, Transcript: strings.TrimSpace(tb.String()),
	})
}

// candidates seleciona as notas candidatas do `ask` (FTS primeiro, completando com recência).
func (s *Service) candidates(ctx context.Context, userID pgtype.UUID, question string) ([]askCandidate, map[string]string, error) {
	rows, err := s.q.SearchNotesFTS(ctx, dbgen.SearchNotesFTSParams{Q: question, Uid: userID, Lim: maxAskCandidates})
	if err != nil {
		return nil, nil, err
	}
	out := make([]askCandidate, 0, maxAskCandidates)
	byID := make(map[string]string, maxAskCandidates)
	add := func(id, title, tldr string) {
		if _, ok := byID[id]; ok || len(out) >= maxAskCandidates {
			return
		}
		byID[id] = title
		out = append(out, askCandidate{NoteID: id, Title: title, Tldr: tldr})
	}
	for _, r := range rows {
		add(r.NoteID, r.Title, r.Tldr)
	}
	if len(out) < maxAskCandidates {
		recent, rerr := s.q.RecentNotes(ctx, dbgen.RecentNotesParams{UserID: userID, Lim: maxAskCandidates})
		if rerr != nil {
			return nil, nil, rerr
		}
		for _, r := range recent {
			add(r.NoteID, r.Title, r.Tldr)
		}
	}
	return out, byID, nil
}

// recent devolve as notas mais recentes como pool de contexto (diário / disparador diário).
func (s *Service) recent(ctx context.Context, userID pgtype.UUID, limit int) ([]askCandidate, error) {
	//nolint:gosec // limit é constante do pacote.
	rows, err := s.q.RecentNotes(ctx, dbgen.RecentNotesParams{UserID: userID, Lim: int32(limit)})
	if err != nil {
		return nil, err
	}
	out := make([]askCandidate, 0, len(rows))
	for _, r := range rows {
		out = append(out, askCandidate{NoteID: r.NoteID, Title: r.Title, Tldr: r.Tldr})
	}
	return out, nil
}

// startMsSet indexa os startMs reais dos segmentos (ancoragem das citações do chat).
func startMsSet(segs []Segment) map[int64]bool {
	if len(segs) == 0 {
		return nil
	}
	m := make(map[int64]bool, len(segs))
	for _, s := range segs {
		m[s.StartMs] = true
	}
	return m
}

// chatMessageData monta o data da entidade chat_message do assistente.
func chatMessageData(threadID string, content []byte) []byte {
	var a ChatAnswer
	_ = json.Unmarshal(content, &a)
	out, _ := json.Marshal(map[string]any{
		"threadId":  threadID,
		"role":      "assistant",
		"text":      a.Text,
		"cites":     a.Cites,
		"createdAt": time.Now().UTC().Format(time.RFC3339),
	})
	return out
}

// safeDir deixa o threadId seguro como nome de diretório (o --resume exige um cwd estável por thread).
func safeDir(s string) string {
	if s == "" {
		return "default"
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}
