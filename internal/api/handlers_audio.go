package api

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/RodolfoBonis/rbnotes-api/internal/audio"
	"github.com/RodolfoBonis/rbnotes-api/internal/platform/httperr"
)

// putAudio — PUT /notes/{id}/audio?part=<n>&total=<n>: recebe um chunk (application/octet-stream)
// e o monta no MinIO via multipart. Quando a última parte chega, o objeto é finalizado.
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

	r.Body = http.MaxBytesReader(w, r.Body, h.audioChunkLimit)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			httperr.Write(w, r, httperr.New(httperr.KindBadRequest, "Payload too large", "chunk acima do limite").WithCode("chunk_too_large"))
			return
		}
		httperr.Write(w, r, httperr.New(httperr.KindBadRequest, "Bad request", "falha ao ler o corpo"))
		return
	}

	completed, err := h.audio.PutPart(r.Context(), currentUser(r).ID, noteID, part, total, r.Header.Get("Content-Type"), bytes.NewReader(data), int64(len(data)))
	switch {
	case errors.Is(err, audio.ErrNoUpload):
		httperr.Write(w, r, httperr.New(httperr.KindConflict, "Conflict", err.Error()).WithCode("audio_no_upload"))
		return
	case errors.Is(err, audio.ErrBadPart):
		httperr.Write(w, r, httperr.New(httperr.KindBadRequest, "Bad request", err.Error()))
		return
	case err != nil:
		writeInternal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"part": part, "completed": completed})
}

// getAudio — GET /notes/{id}/audio: baixa o áudio montado por streaming.
func (h *Handlers) getAudio(w http.ResponseWriter, r *http.Request) {
	if !h.audioEnabled(w, r) {
		return
	}
	noteID, ok := h.safeNoteID(w, r)
	if !ok {
		return
	}
	rc, info, err := h.audio.Download(r.Context(), currentUser(r).ID, noteID)
	if errors.Is(err, audio.ErrNotFound) {
		httperr.Write(w, r, httperr.New(httperr.KindNotFound, "Not found", "áudio não encontrado"))
		return
	}
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	defer func() { _ = rc.Close() }()
	ct := info.ContentType
	if ct == "" {
		ct = "audio/mp4"
	}
	w.Header().Set("Content-Type", ct)
	if info.Size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(info.Size, 10))
	}
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, rc); err != nil {
		h.logger.WarnContext(r.Context(), "audio: streaming interrompido", "err", err.Error())
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
