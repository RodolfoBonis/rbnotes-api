package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCORS(t *testing.T) {
	var reached bool
	h := CORS([]string{"https://trophy.rb.lab", " http://localhost:5173/ "})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))

	tests := []struct {
		name        string
		method      string
		origin      string
		preflight   bool
		wantStatus  int
		wantACAO    string
		wantReached bool
	}{
		{"sem Origin (server-to-server)", http.MethodGet, "", false, http.StatusOK, "", true},
		{"origem permitida", http.MethodGet, "https://trophy.rb.lab", false, http.StatusOK, "https://trophy.rb.lab", true},
		{"origem normalizada (espaço e barra final)", http.MethodGet, "http://localhost:5173", false, http.StatusOK, "http://localhost:5173", true},
		{"origem não permitida segue sem CORS", http.MethodGet, "https://evil.example", false, http.StatusOK, "", true},
		{"subdomínio parecido não casa", http.MethodGet, "https://trophy.rb.lab.evil.example", false, http.StatusOK, "", true},
		{"preflight permitido responde 204 sem chegar no handler", http.MethodOptions, "https://trophy.rb.lab", true, http.StatusNoContent, "https://trophy.rb.lab", false},
		{"preflight negado", http.MethodOptions, "https://evil.example", true, http.StatusForbidden, "", false},
		{"OPTIONS sem Request-Method não é preflight", http.MethodOptions, "https://trophy.rb.lab", false, http.StatusOK, "https://trophy.rb.lab", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reached = false
			req := httptest.NewRequest(tc.method, "/api/v1/games", nil)
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			if tc.preflight {
				req.Header.Set("Access-Control-Request-Method", http.MethodPut)
				req.Header.Set("Access-Control-Request-Headers", "authorization,content-type")
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			assert.Equal(t, tc.wantStatus, rec.Code)
			assert.Equal(t, tc.wantACAO, rec.Header().Get("Access-Control-Allow-Origin"))
			assert.Equal(t, tc.wantReached, reached)
			assert.Empty(t, rec.Header().Get("Access-Control-Allow-Credentials"))
			if tc.preflight && tc.wantStatus == http.StatusNoContent {
				assert.Contains(t, rec.Header().Get("Access-Control-Allow-Headers"), "Authorization")
			}
		})
	}
}
