package scim

// How a SCIM request authenticates (shared migration 0134).
//
// The SCIM tree accepts ONE kind of credential: a SCIM client credential from
// `elitea_auth.scim_clients`. The bearer token is either the secret of a
// `bearer` client or an access token that the token endpoint issued to a
// `client_credentials` client (token.go). A personal access token and a
// browser session are refused with 401 and a sentence that says so.
//
// The tree is therefore mounted OUTSIDE the general `/api/v2` authentication
// middleware (internal/api/router.go). That middleware resolves a USER, and a
// SCIM client is not a user. This file is the only identity check the tree
// has, and it resolves to a CLIENT principal that carries no user id.

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/audit"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/scimclient"
)

// RefusalDetail is the SCIM error `detail` for every refused credential. One
// sentence for every cause, so a caller cannot learn whether a client exists,
// and it names the fix for the most likely cause: a personal access token that
// worked before this change.
const RefusalDetail = "SCIM requires a SCIM client credential; personal access tokens are not accepted"

// Credentials is the store seam for authentication.
type Credentials interface {
	Authenticate(ctx context.Context, token string) (scimclient.Principal, error)
	TouchLastUsed(ctx context.Context, id int64)
}

type principalKey struct{}

// PrincipalFromContext returns the SCIM client a request runs as.
func PrincipalFromContext(ctx context.Context) (scimclient.Principal, bool) {
	principal, ok := ctx.Value(principalKey{}).(scimclient.Principal)
	return principal, ok
}

// Authenticate is the middleware that admits a SCIM client credential and
// nothing else. A nil store refuses every request with 503: an unwired
// authenticator must not be read as "no authentication".
func Authenticate(credentials Credentials) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if credentials == nil {
				writeError(w, http.StatusServiceUnavailable, "",
					"user provisioning is not available on this deployment")
				return
			}
			token, ok := bearerToken(r)
			if !ok {
				refuse(w)
				return
			}
			principal, err := credentials.Authenticate(r.Context(), token)
			if errors.Is(err, scimclient.ErrRejected) {
				refuse(w)
				return
			}
			if err != nil {
				slog.Error("SCIM: credential lookup failed", "err", err)
				writeError(w, http.StatusServiceUnavailable, "",
					"the credential could not be checked; retry later")
				return
			}
			credentials.TouchLastUsed(r.Context(), principal.ID)
			ctx := context.WithValue(r.Context(), principalKey{}, principal)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// AnnotateAuditActor names the SCIM client as the actor of the audit row.
//
// It is mounted BELOW apimw.Audit, which is mounted BELOW Authenticate. That
// order is the point: a request Authenticate refuses never reaches the audit
// middleware, so an anonymous caller cannot write rows to the audit table by
// sending bad credentials. A refusal is logged by the access log instead.
func AnnotateAuditActor(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if principal, ok := PrincipalFromContext(r.Context()); ok {
			audit.Annotate(r.Context(), audit.Annotation{Actor: principal.ActorLabel()})
		}
		next.ServeHTTP(w, r)
	})
}

// bearerToken reads `Authorization: Bearer <token>`. The scheme is case
// insensitive (RFC 7235 §2.1).
func bearerToken(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	scheme, token, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}

func refuse(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="SCIM"`)
	writeError(w, http.StatusUnauthorized, "", RefusalDetail)
}
