package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/RodolfoBonis/rbnotes-api/internal/platform/config"
)

const (
	testAudience = "rbnotes-api"
	testRole     = "rbnotes-user"
	testKID      = "test-key"
)

// fakeIssuer é um issuer OIDC mínimo (discovery + JWKS) que assina tokens com uma chave RSA.
type fakeIssuer struct {
	srv *httptest.Server
	key *rsa.PrivateKey
}

func newFakeIssuer(t *testing.T) *fakeIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	fi := &fakeIssuer{key: key}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                fi.srv.URL,
			"jwks_uri":                              fi.srv.URL + "/jwks",
			"authorization_endpoint":                fi.srv.URL + "/auth",
			"token_endpoint":                        fi.srv.URL + "/token",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
			{Key: &key.PublicKey, KeyID: testKID, Algorithm: "RS256", Use: "sig"},
		}})
	})
	fi.srv = httptest.NewServer(mux)
	t.Cleanup(fi.srv.Close)
	return fi
}

// sign assina um JWT com os claims dados, partindo de um conjunto válido que pode ser sobrescrito.
func (fi *fakeIssuer) sign(t *testing.T, override map[string]any) string {
	t.Helper()
	now := time.Now()
	claims := map[string]any{
		"iss":                fi.srv.URL,
		"sub":                "user-123",
		"aud":                []string{testAudience, "rbnotes-desktop"},
		"exp":                now.Add(5 * time.Minute).Unix(),
		"iat":                now.Unix(),
		"email":              "eu@rb.lab",
		"preferred_username": "rbonis",
		"realm_access":       map[string]any{"roles": []string{"offline_access", testRole}},
	}
	for k, v := range override {
		if v == nil {
			delete(claims, k)
			continue
		}
		claims[k] = v
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: fi.key},
		(&jose.SignerOptions{}).WithHeader("kid", testKID).WithType("JWT"))
	require.NoError(t, err)
	payload, err := json.Marshal(claims)
	require.NoError(t, err)
	obj, err := signer.Sign(payload)
	require.NoError(t, err)
	raw, err := obj.CompactSerialize()
	require.NoError(t, err)
	return raw
}

func newReadyOIDC(t *testing.T, fi *fakeIssuer) *Authenticator {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	a := New(ctx, config.Auth{Mode: config.AuthModeOIDC, Issuer: fi.srv.URL, Audience: testAudience, RequiredRole: testRole}, nil)
	require.Eventually(t, a.Ready, 5*time.Second, 10*time.Millisecond, "provider oidc não carregou")
	return a
}

// serve executa o middleware e devolve o status e o usuário visto pelo handler (se chegou nele).
func serve(a *Authenticator, header http.Header) (int, *UserContext) {
	var seen *UserContext
	h := a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uc, _ := UserFromContext(r.Context())
		seen = &uc
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, seen
}

func bearer(tok string) http.Header { return http.Header{"Authorization": {"Bearer " + tok}} }

func TestOIDCMiddleware(t *testing.T) {
	fi := newFakeIssuer(t)
	other := newFakeIssuer(t) // emissor não confiável (outra chave/iss)
	a := newReadyOIDC(t, fi)

	tests := []struct {
		name   string
		header http.Header
		want   int
	}{
		{"token válido", bearer(fi.sign(t, nil)), http.StatusNoContent},
		{"bearer case-insensitive", http.Header{"Authorization": {"bearer " + fi.sign(t, nil)}}, http.StatusNoContent},
		{"sem Authorization", http.Header{}, http.StatusUnauthorized},
		{"esquema Basic", http.Header{"Authorization": {"Basic Zm9vOmJhcg=="}}, http.StatusUnauthorized},
		{"token malformado", bearer("not-a-jwt"), http.StatusUnauthorized},
		{"audience errada", bearer(fi.sign(t, map[string]any{"aud": []string{"outra-api"}})), http.StatusUnauthorized},
		{"sem aud", bearer(fi.sign(t, map[string]any{"aud": nil})), http.StatusUnauthorized},
		{"issuer errado", bearer(fi.sign(t, map[string]any{"iss": "https://evil.example/realms/x"})), http.StatusUnauthorized},
		{"assinado por outra chave", bearer(other.sign(t, map[string]any{"iss": fi.srv.URL})), http.StatusUnauthorized},
		{"expirado", bearer(fi.sign(t, map[string]any{"exp": time.Now().Add(-time.Minute).Unix()})), http.StatusUnauthorized},
		{"sem a role exigida", bearer(fi.sign(t, map[string]any{"realm_access": map[string]any{"roles": []string{"offline_access"}}})), http.StatusForbidden},
		{"sem realm_access", bearer(fi.sign(t, map[string]any{"realm_access": nil})), http.StatusForbidden},
		{"X-Debug-User ignorado em oidc", http.Header{"X-Debug-User": {"intruso"}}, http.StatusUnauthorized},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, seen := serve(a, tc.header)
			assert.Equal(t, tc.want, code)
			if tc.want != http.StatusNoContent {
				assert.Nil(t, seen, "handler não deve ser chamado")
			}
		})
	}
}

func TestOIDCMiddlewareInjectsUser(t *testing.T) {
	fi := newFakeIssuer(t)
	a := newReadyOIDC(t, fi)

	code, seen := serve(a, bearer(fi.sign(t, map[string]any{"name": "Rodolfo"})))
	require.Equal(t, http.StatusNoContent, code)
	require.NotNil(t, seen)
	assert.Equal(t, "user-123", seen.Subject)
	assert.Equal(t, "eu@rb.lab", seen.Email)
	assert.Equal(t, "Rodolfo", seen.Name)
	assert.NotEmpty(t, seen.Issuer, "o issuer do token identifica o IdP (iss+sub)")

	_, seen = serve(a, bearer(fi.sign(t, nil)))
	require.NotNil(t, seen)
	assert.Equal(t, "rbonis", seen.Name, "sem name, usa preferred_username")
}

func TestOIDCNotReadyReturns503(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Issuer inalcançável: o provider nunca carrega.
	a := New(ctx, config.Auth{Mode: config.AuthModeOIDC, Issuer: "http://127.0.0.1:1", Audience: testAudience, RequiredRole: testRole}, nil)

	assert.False(t, a.Ready())
	code, seen := serve(a, bearer("qualquer"))
	assert.Equal(t, http.StatusServiceUnavailable, code)
	assert.Nil(t, seen)
}

func TestDevMode(t *testing.T) {
	a := New(context.Background(), config.Auth{Mode: config.AuthModeDev}, nil)
	assert.True(t, a.Ready())

	code, seen := serve(a, http.Header{"X-Debug-User": {"dev-user"}, "X-Debug-Email": {"dev@local"}})
	require.Equal(t, http.StatusNoContent, code)
	assert.Equal(t, UserContext{Issuer: "dev", Subject: "dev-user", Email: "dev@local", EmailVerified: true}, *seen)

	code, seen = serve(a, http.Header{})
	assert.Equal(t, http.StatusUnauthorized, code)
	assert.Nil(t, seen)
}

func newAllowlistOIDC(t *testing.T, fi *fakeIssuer, emails ...string) *Authenticator {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	a := New(ctx, config.Auth{Mode: config.AuthModeOIDC, Issuer: fi.srv.URL, Audience: testAudience,
		RequiredRole: testRole, AllowedEmails: emails}, nil)
	require.Eventually(t, a.Ready, 5*time.Second, 10*time.Millisecond, "provider oidc não carregou")
	return a
}

func TestAllowlist(t *testing.T) {
	fi := newFakeIssuer(t)
	a := newAllowlistOIDC(t, fi, "eu@rb.lab")

	tests := []struct {
		name     string
		override map[string]any
		want     int
	}{
		{"e-mail na lista e verificado", map[string]any{"email_verified": true}, http.StatusNoContent},
		{"case-insensitive", map[string]any{"email": "EU@RB.lab", "email_verified": true}, http.StatusNoContent},
		{"e-mail fora da lista", map[string]any{"email": "intruso@gmail.com", "email_verified": true}, http.StatusForbidden},
		{"e-mail não verificado", map[string]any{"email_verified": false}, http.StatusForbidden},
		{"sem claim email_verified", nil, http.StatusForbidden},
		{"sem e-mail", map[string]any{"email": nil, "email_verified": true}, http.StatusForbidden},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var seen *UserContext
			h := a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				uc, _ := UserFromContext(r.Context())
				seen = &uc
				w.WriteHeader(http.StatusNoContent)
			}))
			req := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
			req.Header.Set("Authorization", "Bearer "+fi.sign(t, tc.override))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			assert.Equal(t, tc.want, rec.Code)
			if tc.want == http.StatusForbidden {
				assert.Nil(t, seen, "handler (que cria o usuário) não deve ser chamado")
				assert.Contains(t, rec.Body.String(), `"code":"not_allowed"`)
				assert.Contains(t, rec.Header().Get("Content-Type"), "application/problem+json")
			}
		})
	}
}

func TestAllowlistEmptyAllowsAnyVerifiedOrNot(t *testing.T) {
	fi := newFakeIssuer(t)
	a := newAllowlistOIDC(t, fi)
	code, _ := serve(a, bearer(fi.sign(t, map[string]any{"email": "qualquer@x.com"})))
	assert.Equal(t, http.StatusNoContent, code)
}

func TestAllowlistDevMode(t *testing.T) {
	a := New(context.Background(), config.Auth{Mode: config.AuthModeDev, AllowedEmails: []string{"dev@local"}}, nil)
	code, _ := serve(a, http.Header{"X-Debug-User": {"u"}, "X-Debug-Email": {"dev@local"}})
	assert.Equal(t, http.StatusNoContent, code)
	code, _ = serve(a, http.Header{"X-Debug-User": {"u"}, "X-Debug-Email": {"x@local"}})
	assert.Equal(t, http.StatusForbidden, code)
	code, _ = serve(a, http.Header{"X-Debug-User": {"u"}})
	assert.Equal(t, http.StatusForbidden, code)
}
