package api

import (
	"encoding/json"
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/RodolfoBonis/rbnotes-api/internal/platform/httperr"
)

// writeJSON serializa v como JSON com o status dado.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeInternal responde 500 (problem+json) sem vazar o detalhe interno.
func writeInternal(w http.ResponseWriter, r *http.Request, err error) {
	httperr.Write(w, r, httperr.Wrap(httperr.KindInternal, "Internal error", err))
}

// uuidStr converte um pgtype.UUID em string canônica (vazio quando inválido).
func uuidStr(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	b := u.Bytes
	const hex = "0123456789abcdef"
	buf := make([]byte, 36)
	j := 0
	for i := 0; i < 16; i++ {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			buf[j] = '-'
			j++
		}
		buf[j] = hex[b[i]>>4]
		buf[j+1] = hex[b[i]&0x0f]
		j += 2
	}
	return string(buf)
}
