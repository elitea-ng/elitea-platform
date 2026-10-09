package middleware

import (
	"context"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/projectaccess"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

// projectAccessQuerier is deliberately small so the authorization decision can
// be exercised without a live database.
type projectAccessQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// ProjectAccessQuerier is the one query this middleware runs, exposed so a
// router-level test can supply an answer without a live database. The
// membership decision is the whole point of the middleware, so a test that
// cannot control it can only ever observe the nil-pool 503 — which proves
// nothing about cross-tenant refusal (#302).
type ProjectAccessQuerier = projectAccessQuerier

// RequireProjectAccess rejects requests whose project path parameter does not
// belong to the authenticated user. Authentication establishes identity; this
// middleware establishes tenant scope before a handler may select p_<id> data.
func RequireProjectAccess(pool *pgxpool.Pool) func(http.Handler) http.Handler {
	if pool == nil {
		return unavailableProjectAccess
	}
	return requireProjectAccess(pool)
}

// RequireProjectAccessWith is RequireProjectAccess over an injected querier.
// It takes the interface rather than *pgxpool.Pool, so — unlike the exported
// constructor above — it cannot perform the nil-pool guard: a nil pool boxed
// into an interface is non-nil. Callers pass a real querier or nothing.
func RequireProjectAccessWith(querier ProjectAccessQuerier) func(http.Handler) http.Handler {
	if querier == nil {
		return unavailableProjectAccess
	}
	return requireProjectAccess(querier)
}

// unavailableProjectAccess fails closed with 503 when no pool is configured.
// A nil *pgxpool.Pool boxed into the projectAccessQuerier interface would
// otherwise compare non-nil, bypassing the nil guard inside
// requireProjectAccess and panicking on the subsequent QueryRow call.
func unavailableProjectAccess(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := authenticatedUser(r); !ok {
			apierr.WriteStatus(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		apierr.WriteStatus(w, http.StatusServiceUnavailable, "project authorization unavailable")
	})
}

// authenticatedUser returns the request's authenticated user, or false if
// the request carries no valid identity.
func authenticatedUser(r *http.Request) (auth.User, bool) {
	user, ok := auth.UserFromContext(r.Context())
	if !ok || user.ID == "" {
		return auth.User{}, false
	}
	return user, true
}

func requireProjectAccess(pool projectAccessQuerier) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user, ok := authenticatedUser(r)
			if !ok {
				apierr.WriteStatus(w, http.StatusUnauthorized, "unauthorized")
				return
			}

			projectID, err := strconv.Atoi(chi.URLParam(r, "projectID"))
			if err != nil || projectID <= 0 {
				apierr.WriteStatus(w, http.StatusBadRequest, "invalid project id")
				return
			}
			userID, err := strconv.Atoi(user.ID)
			if err != nil || userID <= 0 {
				apierr.WriteStatus(w, http.StatusForbidden, "forbidden")
				return
			}
			if pool == nil {
				apierr.WriteStatus(w, http.StatusServiceUnavailable, "project authorization unavailable")
				return
			}

			// The central-administrator branch keys on the role NAME AND MODE.
			// auth_core__role is UNIQUE (name, mode), so a legacy database also
			// carries a `super_admin` role in the `default` and `developer`
			// modes. Only the `administration` mode grants central access.
			//
			// KNOWN SPLIT, recorded on purpose. This predicate and the two
			// halves of eliteacore's project-user listing
			// (internal/api/v2/eliteacore/handler.go:290 and :312) admit the
			// administration-mode super_admin only.
			// internal/infra/legacyrbac/postgres.go:60 still resolves central
			// permissions for a developer-mode caller. A developer-mode
			// super_admin therefore keeps its central grants and loses
			// cross-project access here. The three SQL sites agree with each
			// other, not with the resolver.
			//
			// The second column answers a different question: does the project
			// exist? A member always implies existence, so the column only
			// matters on the administrator branch, which admits every project
			// id. Without it a request for a deleted or unknown project reaches
			// the handler. The handler builds the p_<id> schema name and fails
			// with `invalid_schema_name`. The caller reads a 500 for a
			// condition the server understands.
			//
			// centry.project is a pylon-owned table. No migration in
			// migrations/shared/ creates it, and
			// 0071_token_project_binding.sql:42 says so. Two facts make the
			// read safe. First, migrations/shared/0030_execution_kernel.sql:5
			// declares a foreign key to centry.project, so the shared corpus
			// cannot apply without the table. Second,
			// internal/infra/legacyrbac/postgres.go:172 already reads the same
			// table on the default-mode permission path.
			//
			// Accept the cost of that dependency. If the table is ever absent,
			// this middleware answers 503 on EVERY project-scoped route.
			// Before this change only the RBAC resolver failed.
			var allowed, projectExists bool
			err = pool.QueryRow(r.Context(),
				"SELECT "+projectaccess.Membership(1, 2)+", "+projectaccess.ProjectExists(1),
				projectID, userID).Scan(&allowed, &projectExists)
			if err != nil {
				apierr.WriteStatus(w, http.StatusServiceUnavailable, "project authorization unavailable")
				return
			}
			// Refuse a non-member BEFORE the existence answer. The opposite
			// order tells a stranger which project ids exist.
			if !allowed {
				apierr.WriteStatus(w, http.StatusForbidden, "forbidden")
				return
			}
			if !projectExists {
				apierr.WriteStatus(w, http.StatusNotFound, "project not found")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
