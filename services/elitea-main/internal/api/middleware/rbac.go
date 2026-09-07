package middleware

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
	"github.com/go-chi/chi/v5"
)

// RequirePermissions gates a route on the permission set the caller ALREADY
// carries in `auth.User.Permissions`. It asks no resolver and reads no database.
//
// DO NOT USE IT ON A PRODUCTION ROUTE. Production never fills that field. The
// only source that assigns it is the legacy Redis-RPC validator at
// internal/infra/authsvc/rpc.go:121, and production wires
// `authsvc.NewPrincipalValidator` instead, which leaves the field nil. So a
// route gated this way answers 403 to EVERY caller, the operator included, and
// no migration can grant a way in.
//
// That defect shipped on the gateway governance routes and #386 fixed it. The
// route now uses RequireCentralPermissions. This constructor has no production
// call site today. Use RequireCentralPermissions for a platform-wide surface, or
// RequireResolvedPermissions for a project-scoped one. Both ask the resolver.
func RequirePermissions(required ...string) func(http.Handler) http.Handler {
	requiredSet := make(map[string]struct{}, len(required))
	for _, p := range required {
		requiredSet[p] = struct{}{}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user, ok := auth.UserFromContext(r.Context())
			if !ok {
				apierr.WriteStatus(w, http.StatusUnauthorized, "authentication required")
				return
			}

			if !hasIntersection(requiredSet, permissionSet(user.Permissions)) {
				apierr.WriteStatus(w, http.StatusForbidden, "insufficient permissions")
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

func RequireResolvedPermissions(
	resolver auth.PermissionResolver,
	mode string,
	required ...string,
) func(http.Handler) http.Handler {
	return RequireResolvedPermissionsForProject(
		resolver,
		mode,
		func(r *http.Request) (string, bool) {
			projectID := chi.URLParam(r, "projectID")
			return projectID, projectID != ""
		},
		required...,
	)
}

// RequireCentralPermissions gates a route on a CENTRAL (project-less)
// permission — the `administration` and `developer` modes, where
// `legacyrbac.PostgresResolver` reads auth_core__user_role/auth_core__role
// directly and ignores the project id entirely.
//
// RequireResolvedPermissions cannot express this: its extractor demands a
// non-empty `{projectID}` URL param, and the admin-panel routes
// (`/admin/auth_users/{mode}`, `/admin/user_suspend/{mode}/{userID}`, …) have
// no project in their path. Before unit A14 that meant admin-panel writes had
// no route-level gate available at all.
func RequireCentralPermissions(
	resolver auth.PermissionResolver,
	mode string,
	required ...string,
) func(http.Handler) http.Handler {
	return RequireResolvedPermissionsForProject(
		resolver,
		mode,
		// Always valid, always empty: central modes ignore the project id.
		func(*http.Request) (string, bool) { return "", true },
		required...,
	)
}

// RequireResolvedMembershipPermissions gates a route on the caller holding one
// of `required` in AT LEAST ONE project they are a member of.
//
// Use it only where the handler's own answer is already scoped to those
// memberships. The project list is such a route: it returns the caller's
// projects and nothing else, so resolving its gate against one project in the
// URL asked a question the answer does not depend on — and refused every
// account that is not a member of that one project (#830).
//
// It is not a weaker gate than RequireResolvedPermissions. A caller with no
// membership resolves to an empty set and gets 403, and a caller whose roles
// grant nothing gets 403 as well.
func RequireResolvedMembershipPermissions(
	resolver auth.MembershipPermissionResolver,
	mode string,
	required ...string,
) func(http.Handler) http.Handler {
	requiredSet := permissionSet(required)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user, ok := auth.UserFromContext(r.Context())
			if !ok {
				apierr.WriteStatus(w, http.StatusUnauthorized, "authentication required")
				return
			}
			// Fail closed, exactly as RequireResolvedPermissionsForProject
			// does: a route composed without a resolver refuses everyone
			// rather than running ungated.
			if resolver == nil {
				apierr.WriteStatus(w, http.StatusForbidden, "insufficient permissions")
				return
			}

			resolution, err := resolver.ResolveMembershipPermissions(r.Context(), user, mode)
			if err != nil {
				writeResolverError(w, r, err)
				return
			}
			if !hasIntersection(requiredSet, permissionSet(resolution.Permissions)) {
				apierr.WriteStatus(w, http.StatusForbidden, "insufficient permissions")
				return
			}

			user.UserID = strconv.FormatInt(resolution.UserID, 10)
			ctx := auth.ContextWithUser(r.Context(), user)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

type ProjectIDExtractor func(*http.Request) (string, bool)

func ProjectIDFromQuery(parameter string) ProjectIDExtractor {
	return func(r *http.Request) (string, bool) {
		projectID := r.URL.Query().Get(parameter)
		value, err := strconv.ParseInt(projectID, 10, 64)
		return projectID, err == nil && value > 0 && strconv.FormatInt(value, 10) == projectID
	}
}

func RequireResolvedPermissionsForProject(
	resolver auth.PermissionResolver,
	mode string,
	projectID ProjectIDExtractor,
	required ...string,
) func(http.Handler) http.Handler {
	requiredSet := permissionSet(required)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user, ok := auth.UserFromContext(r.Context())
			if !ok {
				apierr.WriteStatus(w, http.StatusUnauthorized, "authentication required")
				return
			}
			if resolver == nil || projectID == nil {
				apierr.WriteStatus(w, http.StatusForbidden, "insufficient permissions")
				return
			}

			resolvedProjectID, validProjectID := projectID(r)
			if !validProjectID {
				apierr.WriteStatus(w, http.StatusForbidden, "insufficient permissions")
				return
			}

			resolution, err := resolver.ResolvePermissions(
				r.Context(),
				user,
				mode,
				resolvedProjectID,
			)
			if err != nil {
				writeResolverError(w, r, err)
				return
			}
			if !hasIntersection(requiredSet, permissionSet(resolution.Permissions)) {
				apierr.WriteStatus(w, http.StatusForbidden, "insufficient permissions")
				return
			}

			user.UserID = strconv.FormatInt(resolution.UserID, 10)
			ctx := auth.ContextWithUser(r.Context(), user)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// writeResolverError answers a permission-resolver error.
//
// The resolver reports a REFUSAL as auth.ErrPermissionDenied. Every other
// error is an infrastructure failure: a saturated connection pool, a query
// timeout, or a scan failure. The two must not share one status code. When
// they do, a database outage reaches the user as "insufficient permissions" on
// every screen. It reaches the operator as a wall of 403 answers with no 5xx
// and no error rate alert.
//
// A canceled request context means the client went away. It gets no status and
// no log line, because a 5xx there is a false alarm.
func writeResolverError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, auth.ErrPermissionDenied):
		apierr.WriteStatus(w, http.StatusForbidden, "insufficient permissions")
	case errors.Is(err, context.Canceled), errors.Is(r.Context().Err(), context.Canceled):
		return
	default:
		slog.ErrorContext(r.Context(), "permission resolution failed",
			"error", err,
			"method", r.Method,
			"path", r.URL.Path,
		)
		apierr.WriteStatus(w, http.StatusInternalServerError, "permission resolution failed")
	}
}

func permissionSet(perms []string) map[string]struct{} {
	result := make(map[string]struct{}, len(perms))
	for _, p := range perms {
		result[p] = struct{}{}
	}
	return result
}

func hasIntersection(required, userPerms map[string]struct{}) bool {
	for k := range required {
		if _, ok := userPerms[k]; ok {
			return true
		}
	}
	return false
}
