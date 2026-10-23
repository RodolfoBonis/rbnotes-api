package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/RodolfoBonis/rbnotes-api/internal/platform/httperr"
	syncpkg "github.com/RodolfoBonis/rbnotes-api/internal/sync"
)

// pushChanges — POST /sync/push {changes:[...]} → {applied, conflicts}. LWW por updatedAt; conflito
// devolve a versão do servidor.
func (h *Handlers) pushChanges(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Changes []syncpkg.Change `json:"changes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httperr.Write(w, r, httperr.New(httperr.KindBadRequest, "Bad request", "corpo JSON inválido"))
		return
	}
	res, err := h.sync.Push(r.Context(), currentUser(r).ID, body.Changes)
	switch {
	case errors.Is(err, syncpkg.ErrTooManyChanges):
		httperr.Write(w, r, httperr.New(httperr.KindBadRequest, "Bad request", err.Error()))
		return
	case err != nil:
		writeInternal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// pullChanges — GET /sync/pull?since=<cursor> → {changes, cursor}.
func (h *Handlers) pullChanges(w http.ResponseWriter, r *http.Request) {
	res, err := h.sync.Pull(r.Context(), currentUser(r).ID, r.URL.Query().Get("since"))
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
