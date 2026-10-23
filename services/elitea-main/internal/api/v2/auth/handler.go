package auth

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// TokenSigner signs the bearer of a personal access token.
//
// The signer must hold the SAME key that the deployment's token validator
// reads a token back with. A deployment that authenticates through an
// authentication configuration file keeps that key in
// credentials.pat_signing_key_file, never in APPLICATION_SECRET_KEY.
type TokenSigner interface {
	SignPAT(tokenUUID *string, expiresAt *time.Time) (string, error)
}

type Handler struct {
	permissions     auth.PermissionResolver
	tokens          tokenRepository
	tokenSigningKey []byte
	tokenSigner     TokenSigner
}

type Option func(*Handler)

func WithPermissionResolver(resolver auth.PermissionResolver) Option {
	return func(handler *Handler) {
		handler.permissions = resolver
	}
}

// WithTokenSigningKey signs personal access tokens with one raw key.
//
// Use it only when the deployment validates a token with that same key. The
// OIDC-only shape does: APPLICATION_SECRET_KEY signs the token and
// authsvc.NewLocalValidator reads it back. A deployment with a form
// authentication graph must use WithTokenSigner instead.
func WithTokenSigningKey(secret string) Option {
	return func(handler *Handler) {
		handler.tokenSigningKey = []byte(secret)
	}
}

// WithTokenSigner signs personal access tokens through the deployment's own
// authentication graph, which holds the key its validator uses. It takes
// precedence over WithTokenSigningKey.
func WithTokenSigner(signer TokenSigner) Option {
	return func(handler *Handler) {
		handler.tokenSigner = signer
	}
}

func NewHandler(pool *pgxpool.Pool, opts ...Option) *Handler {
	handler := &Handler{}
	if pool != nil {
		handler.tokens = newPostgresTokenRepository(pool)
	}
	for _, opt := range opts {
		opt(handler)
	}
	return handler
}

func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Use(noStore)
	r.Get("/permissions/prompt_lib/{projectID}", h.PermissionList)
	r.Get("/token/", h.TokenList)
	r.Get("/token/{tokenUUID}", h.TokenGet)
	r.Post("/token/", h.TokenCreate)
	r.Delete("/token/{tokenUUID}", h.TokenDelete)
	return r
}

func noStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Pragma", "no-cache")
		next.ServeHTTP(w, r)
	})
}

type Permission struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Enabled     bool   `json:"enabled"`
}

func (h *Handler) PermissionList(w http.ResponseWriter, r *http.Request) {
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		apierr.Write(w, apierr.Unauthorized("not authenticated"))
		return
	}

	projectID := chi.URLParam(r, "projectID")
	result := []Permission{}
	if h.permissions != nil {
		resolution, err := h.permissions.ResolvePermissions(
			r.Context(),
			user,
			auth.PermissionModeDefault,
			projectID,
		)
		if err == nil {
			result = make([]Permission, 0, len(resolution.Permissions))
			for _, p := range resolution.Permissions {
				result = append(result, Permission{Name: p, Enabled: true})
			}
		}
	}

	writeJSON(w, http.StatusOK, result)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
