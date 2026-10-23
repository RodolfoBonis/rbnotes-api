package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	dbgen "github.com/RodolfoBonis/rbnotes-api/db/gen"
	"github.com/RodolfoBonis/rbnotes-api/internal/platform/auth"
	"github.com/RodolfoBonis/rbnotes-api/internal/platform/httperr"
)

// O RbNotes é pessoal e multi-tenant: cada identidade do IdP (issuer + subject) é um usuário
// isolado. O usuário nasce no primeiro request autenticado; tudo depois é escopo do user_id dele.

type appUserKey struct{}

func withAppUser(ctx context.Context, u dbgen.User) context.Context {
	return context.WithValue(ctx, appUserKey{}, u)
}

// currentUser devolve o usuário do app resolvido pelo middleware (sempre presente nas rotas /v1).
func currentUser(r *http.Request) dbgen.User {
	u, _ := r.Context().Value(appUserKey{}).(dbgen.User)
	return u
}

// touchEvery limita a escrita de last_seen/nome/e-mail a uma vez por intervalo por pessoa.
const touchEvery = time.Hour

// resolveUser (depois da autenticação) encontra ou cria o usuário do app e o põe no contexto.
func (h *Handlers) resolveUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uc, ok := auth.UserFromContext(r.Context())
		if !ok {
			httperr.Write(w, r, httperr.New(httperr.KindUnauthorized, "Unauthorized", ""))
			return
		}
		u, err := h.userFor(r.Context(), uc)
		if err != nil {
			writeInternal(w, r, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(withAppUser(r.Context(), u)))
	})
}

func (h *Handlers) userFor(ctx context.Context, uc auth.UserContext) (dbgen.User, error) {
	u, err := h.q.GetUserByIdentity(ctx, dbgen.GetUserByIdentityParams{Issuer: uc.Issuer, Subject: uc.Subject})
	if errors.Is(err, pgx.ErrNoRows) {
		return h.q.CreateUser(ctx, dbgen.CreateUserParams{Issuer: uc.Issuer, Subject: uc.Subject, Email: uc.Email, Name: uc.Name})
	}
	if err != nil {
		return u, err
	}
	if h.stale(u, uc) {
		if terr := h.q.TouchUser(ctx, dbgen.TouchUserParams{ID: u.ID, Email: uc.Email, Name: uc.Name}); terr != nil {
			h.logger.Warn("users: atualizar último acesso falhou", slog.String("err", terr.Error()))
		}
		u.Email, u.Name = uc.Email, uc.Name
	}
	return u, nil
}

// stale: nome/e-mail mudaram no IdP ou o último acesso registrado é antigo.
func (h *Handlers) stale(u dbgen.User, uc auth.UserContext) bool {
	return u.Email != uc.Email || u.Name != uc.Name || !u.LastSeenAt.Valid || time.Since(u.LastSeenAt.Time) > touchEvery
}

// meDTO ↔ components.schemas.User.
type meDTO struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	Name  string `json:"name"`
}

// getMe — GET /me.
func (h *Handlers) getMe(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	writeJSON(w, http.StatusOK, meDTO{ID: uuidStr(u.ID), Email: u.Email, Name: u.Name})
}
