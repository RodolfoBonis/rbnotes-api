package api

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/RodolfoBonis/rbnotes-api/internal/audio"
	"github.com/RodolfoBonis/rbnotes-api/internal/platform/httperr"
)

// audioIOTimeout é o deadline de leitura/escrita aplicado por chunk no upload e no streaming do
// download, para um socket lento não prender um worker indefinidamente.
const audioIOTimeout = 2 * time.Minute

// putAudio — PUT /notes/{id}/audio?part=<n>&total=<n>: recebe um chunk e o monta no MinIO via
// multipart. O corpo é transmitido direto para o S3 (sem bufferizar em memória), com o Content-Length
// do request. Quando a última parte chega, o objeto é finalizado.
func (h *Handlers) putAudio(w http.ResponseWriter, r *http.Request) {
	if !h.audioEnabled(w, r) {
		return
	}
	noteID, ok := h.safeNoteID(w, r)
	if !ok {
		return
	}
	part, perr := strconv.Atoi(r.URL.Query().Get("part"))
	total, terr := strconv.Atoi(r.URL.Query().Get("total"))
	if perr != nil || terr != nil || part < 1 || total < 1 || part > total {
		httperr.Write(w, r, httperr.New(httperr.KindBadRequest, "Bad request", "part/total inválidos"))
		return
	}

	// Streaming exige o tamanho exato da parte (Content-Length): sem ele não dá para enviar ao S3.
	size := r.ContentLength
	if size <= 0 {
		httperr.Write(w, r, httperr.New(httperr.KindBadRequest, "Length required", "Content-Length obrigatório").WithCode("length_required"))
		return
	}
	if size > h.audioChunkLimit {
		httperr.Write(w, r, httperr.New(httperr.KindPayloadTooLarge, "Payload too large", "chunk acima do limite").WithCode("chunk_too_large"))
		return
	}

	// Deadline de leitura do corpo (best effort; httptest não suporta e devolve erro, que ignoramos).
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(audioIOTimeout))

	body := http.MaxBytesReader(w, r.Body, h.audioChunkLimit)
	completed, err := h.audio.PutPart(r.Context(), currentUser(r).ID, noteID, part, total, r.Header.Get("Content-Type"), body, size)
	switch {
	case errors.Is(err, audio.ErrNoUpload):
		httperr.Write(w, r, httperr.New(httperr.KindConflict, "Conflict", err.Error()).WithCode("audio_no_upload"))
		return
	case errors.Is(err, audio.ErrAlreadyCompleted):
		httperr.Write(w, r, httperr.New(httperr.KindConflict, "Conflict", err.Error()).WithCode("audio_completed"))
		return
	case errors.Is(err, audio.ErrTotalMismatch):
		httperr.Write(w, r, httperr.New(httperr.KindConflict, "Conflict", err.Error()).WithCode("audio_total_mismatch"))
		return
	case errors.Is(err, audio.ErrBadPart):
		httperr.Write(w, r, httperr.New(httperr.KindBadRequest, "Bad request", err.Error()))
		return
	case errors.Is(err, audio.ErrPartTooSmall):
		httperr.Write(w, r, httperr.Validation(err.Error()).WithCode("audio_part_too_small"))
		return
	case errors.Is(err, audio.ErrBadContentType):
		httperr.Write(w, r, httperr.Validation(err.Error()).WithCode("audio_bad_content_type"))
		return
	case errors.Is(err, audio.ErrTooLarge):
		httperr.Write(w, r, httperr.New(httperr.KindPayloadTooLarge, "Payload too large", err.Error()).WithCode("audio_too_large"))
		return
	case err != nil:
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			httperr.Write(w, r, httperr.New(httperr.KindPayloadTooLarge, "Payload too large", "chunk acima do limite").WithCode("chunk_too_large"))
			return
		}
		writeInternal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"part": part, "completed": completed})
}

// getAudio — GET /notes/{id}/audio: baixa o áudio montado por streaming, com suporte a Range.
func (h *Handlers) getAudio(w http.ResponseWriter, r *http.Request) {
	if !h.audioEnabled(w, r) {
		return
	}
	noteID, ok := h.safeNoteID(w, r)
	if !ok {
		return
	}
	content, err := h.audio.Open(r.Context(), currentUser(r).ID, noteID, r.Header.Get("Range"))
	switch {
	case errors.Is(err, audio.ErrNotFound):
		httperr.Write(w, r, httperr.New(httperr.KindNotFound, "Not found", "áudio não encontrado"))
		return
	case errors.Is(err, audio.ErrBadRange):
		w.Header().Set("Content-Range", "bytes */0")
		httperr.Write(w, r, httperr.New(httperr.KindRangeNotSatisfiable, "Range not satisfiable", "range inválido"))
		return
	case err != nil:
		writeInternal(w, r, err)
		return
	}
	defer func() { _ = content.Body.Close() }()

	w.Header().Set("Content-Type", content.ContentType)
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Length", strconv.FormatInt(content.Length(), 10))
	if content.Partial {
		w.Header().Set("Content-Range", "bytes "+strconv.FormatInt(content.Start, 10)+"-"+strconv.FormatInt(content.End, 10)+"/"+strconv.FormatInt(content.Size, 10))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	if err := streamWithDeadline(w, content.Body); err != nil {
		h.logger.WarnContext(r.Context(), "audio: streaming interrompido", "err", err.Error())
	}
}

// streamWithDeadline copia src para w renovando o deadline de escrita a cada bloco (best effort).
func streamWithDeadline(w http.ResponseWriter, src io.Reader) error {
	rc := http.NewResponseController(w)
	buf := make([]byte, 128<<10)
	for {
		_ = rc.SetWriteDeadline(time.Now().Add(audioIOTimeout))
		n, rerr := src.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
		}
		if rerr == io.EOF {
			return nil
		}
		if rerr != nil {
			return rerr
		}
	}
}

func (h *Handlers) audioEnabled(w http.ResponseWriter, r *http.Request) bool {
	if h.audio == nil {
		httperr.Write(w, r, httperr.New(httperr.KindNotImplemented, "Áudio indisponível", "o armazenamento de áudio não está configurado"))
		return false
	}
	return true
}

// safeNoteID valida o id da nota (vem do cliente e entra na chave do objeto): só alfanumérico,
// hífen e underscore (barra o path traversal).
func (h *Handlers) safeNoteID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := chi.URLParam(r, "id")
	if id == "" || len(id) > 128 || !isSafeID(id) {
		httperr.Write(w, r, httperr.New(httperr.KindBadRequest, "Bad request", "id da nota inválido"))
		return "", false
	}
	return id, true
}

func isSafeID(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}
