// Package auth é a fronteira de autenticação da API. O app é single-user; o SSO vem do Keycloak
// (realm platform).
//
//   - oidc: valida o JWT (Bearer) contra o JWKS do issuer, conferindo iss, aud (Audience) e a
//     presença da role exigida em realm_access.roles. O provider é descoberto de forma preguiçosa
//     e com retry, para a API subir mesmo com o Keycloak temporariamente fora (readyz só fica
//     pronto quando o provider carrega).
//   - dev: confia no header X-Debug-User (apenas dev local, sem Keycloak). Nunca loga tokens.
package auth

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"

	"github.com/RodolfoBonis/rbnotes-api/internal/platform/config"
	"github.com/RodolfoBonis/rbnotes-api/internal/platform/httperr"
)

// providerInitInterval é o intervalo entre tentativas de descobrir o provider OIDC.
const providerInitInterval = 10 * time.Second

// Authenticator valida os tokens conforme o modo configurado. É seguro para uso concorrente
// (o verifier é publicado atomicamente uma única vez).
type Authenticator struct {
	mode         config.AuthMode
	requiredRole string
	// allowedEmails: set (lowercase) de e-mails aceitos; vazio = sem restrição.
	allowedEmails map[string]struct{}
	// verifier é carregado em background no modo oidc; nil até o provider ser descoberto.
	verifier atomic.Pointer[oidc.IDTokenVerifier]
	logger   *slog.Logger
}

// New constrói o autenticador. No modo oidc NÃO bloqueia na descoberta do provider: dispara um
// loop de retry em background (encerrado quando ctx cancela) e devolve imediatamente. Use Ready()
// para o readyz: a API aceita tráfego de probes mas rejeita requests autenticados (503) até o
// provider carregar.
func New(ctx context.Context, cfg config.Auth, logger *slog.Logger) *Authenticator {
	if logger == nil {
		logger = slog.Default()
	}
	a := &Authenticator{mode: cfg.Mode, requiredRole: cfg.RequiredRole, logger: logger,
		allowedEmails: make(map[string]struct{}, len(cfg.AllowedEmails))}
	for _, e := range cfg.AllowedEmails {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
			a.allowedEmails[e] = struct{}{}
		}
	}
	if cfg.Mode == config.AuthModeDev {
		return a
	}
	go a.initProvider(ctx, cfg)
	return a
}

// initProvider tenta descobrir o provider OIDC até conseguir (ou o ctx cancelar).
func (a *Authenticator) initProvider(ctx context.Context, cfg config.Auth) {
	for {
		provider, err := oidc.NewProvider(ctx, cfg.Issuer)
		if err == nil {
			a.verifier.Store(provider.Verifier(&oidc.Config{ClientID: cfg.Audience}))
			a.logger.Info("provider oidc carregado", slog.String("issuer", cfg.Issuer))
			return
		}
		a.logger.Warn("provider oidc indisponível; tentando de novo",
			slog.String("issuer", cfg.Issuer), slog.String("err", err.Error()))
		t := time.NewTimer(providerInitInterval)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// Ready indica se o autenticador está pronto para validar requests (sempre no modo dev; no modo
// oidc, após o provider carregar).
func (a *Authenticator) Ready() bool {
	if a.mode == config.AuthModeDev {
		return true
	}
	return a.verifier.Load() != nil
}

// userClaims captura os claims emitidos pelo Keycloak.
type userClaims struct {
	Email             string `json:"email"`
	EmailVerified     bool   `json:"email_verified"`
	Name              string `json:"name"`
	PreferredUsername string `json:"preferred_username"`
	RealmAccess       struct {
		Roles []string `json:"roles"`
	} `json:"realm_access"`
}

func (c userClaims) displayName() string {
	if c.Name != "" {
		return c.Name
	}
	return c.PreferredUsername
}

func (c userClaims) hasRole(want string) bool {
	if want == "" {
		return true
	}
	for _, role := range c.RealmAccess.Roles {
		if role == want {
			return true
		}
	}
	return false
}

// Middleware exige um usuário autenticado (e com a role exigida) e injeta o UserContext.
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uc, err := a.authenticate(r)
		if err != nil {
			logAuthFailure(r, err)
			httperr.Write(w, r, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(ContextWithUser(r.Context(), uc)))
	})
}

func (a *Authenticator) authenticate(r *http.Request) (UserContext, error) {
	if a.mode == config.AuthModeDev {
		sub := r.Header.Get("X-Debug-User")
		if sub == "" {
			return UserContext{}, httperr.New(httperr.KindUnauthorized, "Unauthorized",
				"defina X-Debug-User no modo dev")
		}
		email := r.Header.Get("X-Debug-Email")
		if err := a.checkAllowed(email, email != ""); err != nil {
			return UserContext{}, err
		}
		return UserContext{Issuer: "dev", Subject: sub, Email: email, EmailVerified: email != "", Name: r.Header.Get("X-Debug-Name")}, nil
	}

	verifier := a.verifier.Load()
	if verifier == nil {
		return UserContext{}, httperr.New(httperr.KindUnavailable, "Service unavailable",
			"provider de autenticação ainda não disponível")
	}

	raw, ok := bearerToken(r)
	if !ok {
		return UserContext{}, httperr.New(httperr.KindUnauthorized, "Unauthorized", "token Bearer ausente")
	}
	idToken, err := verifier.Verify(r.Context(), raw)
	if err != nil {
		return UserContext{}, httperr.New(httperr.KindUnauthorized, "Invalid token",
			"token inválido ou de emissor não confiável")
	}
	var claims userClaims
	if err := idToken.Claims(&claims); err != nil {
		return UserContext{}, httperr.Wrap(httperr.KindInternal, "Internal error", err)
	}
	if !claims.hasRole(a.requiredRole) {
		return UserContext{}, httperr.New(httperr.KindForbidden, "Forbidden",
			"o token não possui a role exigida")
	}
	if err := a.checkAllowed(claims.Email, claims.EmailVerified); err != nil {
		return UserContext{}, err
	}
	return UserContext{
		Issuer: idToken.Issuer, Subject: idToken.Subject, Email: claims.Email,
		EmailVerified: claims.EmailVerified, Name: claims.displayName(),
	}, nil
}

// checkAllowed aplica a allowlist de e-mails (quando configurada): exige e-mail verificado e
// presente na lista. Roda ANTES de qualquer criação de usuário (o middleware precede o resolve).
func (a *Authenticator) checkAllowed(email string, verified bool) error {
	if len(a.allowedEmails) == 0 {
		return nil
	}
	if _, ok := a.allowedEmails[strings.ToLower(strings.TrimSpace(email))]; !ok || !verified {
		return httperr.New(httperr.KindForbidden, "Forbidden", "esta conta não tem acesso a este serviço").
			WithCode("not_allowed")
	}
	return nil
}

// logAuthFailure registra 401/403 em nível info com o motivo (NUNCA o token).
func logAuthFailure(r *http.Request, err error) {
	he, ok := httperr.AsError(err)
	if !ok {
		return
	}
	switch he.Kind {
	case httperr.KindUnauthorized, httperr.KindForbidden:
		slog.InfoContext(r.Context(), "auth negada",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.String("reason", he.Title),
		)
	default:
	}
}

func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	return h[len(prefix):], true
}
