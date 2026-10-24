package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	dbgen "github.com/RodolfoBonis/rbnotes-api/db/gen"
	"github.com/RodolfoBonis/rbnotes-api/internal/ai"
	"github.com/RodolfoBonis/rbnotes-api/internal/ai/stream"
	"github.com/RodolfoBonis/rbnotes-api/internal/platform/httperr"
)

func (h *Handlers) aiEnabled(w http.ResponseWriter, r *http.Request) bool {
	if h.ai == nil || h.rdb == nil {
		httperr.Write(w, r, httperr.New(httperr.KindNotImplemented, "IA indisponível", "a IA não está configurada"))
		return false
	}
	return true
}

// acceptedJob responde 202 { id, status: "queued" }.
func acceptedJob(w http.ResponseWriter, job dbgen.AiJob) {
	writeJSON(w, http.StatusAccepted, map[string]string{"id": uuidStr(job.ID), "status": "queued"})
}

// writeAIRequestErr traduz os erros de pedido (cota, modelo, entrada).
func (h *Handlers) writeAIRequestErr(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ai.ErrDailyLimit):
		httperr.Write(w, r, httperr.New(httperr.KindRateLimited, "Limite diário de IA",
			"você atingiu o limite de pedidos de IA de hoje").WithCode("ai_daily_limit"))
	case errors.Is(err, ai.ErrBadModel):
		httperr.Write(w, r, httperr.Validation(err.Error(), httperr.FieldError{Field: "model", Code: "invalid"}))
	case errors.Is(err, ai.ErrBadInput):
		httperr.Write(w, r, httperr.Validation(err.Error()))
	default:
		writeInternal(w, r, err)
	}
}

func decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		httperr.Write(w, r, httperr.New(httperr.KindBadRequest, "Bad request", "corpo JSON inválido"))
		return false
	}
	return true
}

// enrichNote — POST /ai/notes/{id}/enrich.
func (h *Handlers) enrichNote(w http.ResponseWriter, r *http.Request) {
	if !h.aiEnabled(w, r) {
		return
	}
	noteID, ok := h.safeNoteID(w, r)
	if !ok {
		return
	}
	var in ai.EnrichInput
	if !decodeBody(w, r, &in) {
		return
	}
	job, err := h.ai.RequestEnrich(r.Context(), currentUser(r).ID, noteID, in)
	if err != nil {
		h.writeAIRequestErr(w, r, err)
		return
	}
	acceptedJob(w, job)
}

// chatNote — POST /ai/notes/{id}/chat.
func (h *Handlers) chatNote(w http.ResponseWriter, r *http.Request) {
	if !h.aiEnabled(w, r) {
		return
	}
	noteID, ok := h.safeNoteID(w, r)
	if !ok {
		return
	}
	var in ai.ChatInput
	if !decodeBody(w, r, &in) {
		return
	}
	if in.ThreadID == "" {
		in.ThreadID = noteID // uma conversa por nota por padrão
	}
	job, err := h.ai.RequestChat(r.Context(), currentUser(r).ID, in)
	if err != nil {
		h.writeAIRequestErr(w, r, err)
		return
	}
	acceptedJob(w, job)
}

// ask — POST /ai/ask.
func (h *Handlers) ask(w http.ResponseWriter, r *http.Request) {
	if !h.aiEnabled(w, r) {
		return
	}
	var in ai.AskInput
	if !decodeBody(w, r, &in) {
		return
	}
	job, err := h.ai.RequestAsk(r.Context(), currentUser(r).ID, in)
	if err != nil {
		h.writeAIRequestErr(w, r, err)
		return
	}
	acceptedJob(w, job)
}

// collectionRule — POST /ai/collections/rule.
func (h *Handlers) collectionRule(w http.ResponseWriter, r *http.Request) {
	if !h.aiEnabled(w, r) {
		return
	}
	var in ai.RuleInput
	if !decodeBody(w, r, &in) {
		return
	}
	job, err := h.ai.RequestCollectionRule(r.Context(), currentUser(r).ID, in)
	if err != nil {
		h.writeAIRequestErr(w, r, err)
		return
	}
	acceptedJob(w, job)
}

// diaryMonth — POST /ai/diary/month.
func (h *Handlers) diaryMonth(w http.ResponseWriter, r *http.Request) {
	if !h.aiEnabled(w, r) {
		return
	}
	var in ai.DiaryInput
	if !decodeBody(w, r, &in) {
		return
	}
	job, err := h.ai.RequestDiaryMonth(r.Context(), currentUser(r).ID, in)
	if err != nil {
		h.writeAIRequestErr(w, r, err)
		return
	}
	acceptedJob(w, job)
}

// dailyPrompt — GET /ai/diary/prompt?date=. Como a API não executa a CLI, o disparador é gerado de
// forma assíncrona (202 + stream), igual aos demais pedidos de IA.
func (h *Handlers) dailyPrompt(w http.ResponseWriter, r *http.Request) {
	if !h.aiEnabled(w, r) {
		return
	}
	job, err := h.ai.RequestDailyPrompt(r.Context(), currentUser(r).ID, ai.PromptInput{Date: r.URL.Query().Get("date")})
	if err != nil {
		h.writeAIRequestErr(w, r, err)
		return
	}
	acceptedJob(w, job)
}

const (
	// sseBlock é o intervalo máximo sem bytes na conexão (heartbeat), abaixo dos timeouts de proxy.
	sseBlock = 5 * time.Second
	// sseMaxDuration encerra streams esquecidas; o cliente reconecta com Last-Event-ID.
	sseMaxDuration = 15 * time.Minute
)

// lastEventIDRe valida o Last-Event-ID (formato de id do Redis Streams: "<ms>-<seq>").
var lastEventIDRe = regexp.MustCompile(`^\d+-\d+$`)

// aiStream — GET /ai/streams/{id} (SSE): a resposta da IA em tempo real. Eventos: status/text/json/
// tool (parciais) e done/error (finais). Retoma do Last-Event-ID. Se a resposta já terminou e a
// stream do Redis expirou, manda só o evento final (do ai_jobs).
func (h *Handlers) aiStream(w http.ResponseWriter, r *http.Request) {
	if !h.aiEnabled(w, r) {
		return
	}
	id, err := parseUUIDParam(chi.URLParam(r, "id"))
	if err != nil {
		httperr.Write(w, r, httperr.New(httperr.KindBadRequest, "Bad request", "id inválido"))
		return
	}
	// Valida o Last-Event-ID ANTES de comprometer o 200 do SSE (um valor malformado é 400).
	if le := r.Header.Get("Last-Event-ID"); le != "" && !lastEventIDRe.MatchString(le) {
		httperr.Write(w, r, httperr.New(httperr.KindBadRequest, "Bad request", "Last-Event-ID inválido"))
		return
	}
	final, ok := h.streamFinalState(w, r, id)
	if !ok {
		return
	}
	key := uuidStr(id)

	rc := http.NewResponseController(w)
	hd := w.Header()
	hd.Set("Content-Type", "text/event-stream")
	hd.Set("Cache-Control", "no-cache, no-transform")
	hd.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	sendEvent := func(evID, typ, data, code string) bool {
		_ = rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
		m := map[string]string{"data": data}
		if code != "" {
			m["code"] = code
		}
		payload, _ := json.Marshal(m)
		var b strings.Builder
		if evID != "" {
			fmt.Fprintf(&b, "id: %s\n", evID)
		}
		fmt.Fprintf(&b, "event: %s\ndata: %s\n\n", typ, payload)
		if _, werr := w.Write([]byte(b.String())); werr != nil {
			return false
		}
		return rc.Flush() == nil
	}
	send := func(evID, typ, data string) bool { return sendEvent(evID, typ, data, "") }

	ctx := r.Context()
	last := r.Header.Get("Last-Event-ID")
	if last == "" {
		last = "0"
	}
	deadline := time.Now().Add(sseMaxDuration)
	first := true
	for ctx.Err() == nil && time.Now().Before(deadline) {
		evs, err := stream.Read(ctx, h.rdb, key, last, sseBlock)
		if err != nil {
			if ctx.Err() == nil {
				h.logger.WarnContext(ctx, "ai stream: leitura falhou", "err", err.Error())
			}
			return
		}
		if len(evs) == 0 {
			if first && final != nil {
				send("", final.typ, final.data)
				return
			}
			if !send("", "ping", "") {
				return
			}
			first = false
			continue
		}
		first = false
		for _, e := range evs {
			last = e.ID
			if !send(e.ID, e.Type, e.Data) {
				return
			}
			if e.Final() {
				return
			}
		}
	}
	// Saiu por duração máxima (não por desconexão do cliente): manda um evento final para o cliente
	// parar de reconectar para sempre.
	if ctx.Err() == nil {
		sendEvent("", stream.TypeError, "A resposta demorou demais. Recarregue para tentar de novo.", "stream_timeout")
	}
}

type finalEvent struct{ typ, data string }

// streamFinalState confere que o id é um ai_job do usuário e, se já terminou, devolve o evento
// final equivalente (para quando a stream do Redis já expirou).
func (h *Handlers) streamFinalState(w http.ResponseWriter, r *http.Request, id pgtype.UUID) (*finalEvent, bool) {
	aj, err := h.q.GetAIJob(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && aj.UserID != currentUser(r).ID) {
		httperr.Write(w, r, httperr.New(httperr.KindNotFound, "Not found", "resposta não encontrada"))
		return nil, false
	}
	if err != nil {
		writeInternal(w, r, err)
		return nil, false
	}
	switch aj.Status {
	case "done":
		// A stream do Redis pode já ter expirado (os `json` parciais se perderam): o `done` carrega
		// o resultado canônico salvo no banco, para o cliente ter a resposta íntegra mesmo assim.
		return &finalEvent{stream.TypeDone, string(aj.Result)}, true
	case "error":
		return &finalEvent{stream.TypeError, aj.Error}, true
	}
	return nil, true
}
