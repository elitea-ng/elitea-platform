package middleware

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

// PersonalProjectCheck reports whether projectID is the personal project of
// userID. The user id is the one the permission resolver returned, not a value
// the request supplied.
type PersonalProjectCheck func(ctx context.Context, projectID string, userID int64) (bool, error)

// RequireResolvedPermissionOrPersonalProject gates a project-scoped route on
// `required`, with one exception for the caller's own personal project.
//
// The exception exists for issue #6789. A team project restricts its settings
// to the project admin. A personal project has no admin and no editor: its
// owner is the only member. Projects that pylon created give that owner the
// `editor` role, not `admin`. A strict admin-only gate would lock those owners
// out of their own project. So, in the caller's OWN personal project, the
// route accepts `personalPermission` in place of `required`.
//
// The exception never widens the route beyond `personalPermission`. The caller
// must still hold that permission in the project, and the check reads the user
// id from the resolver, after the resolver has verified the principal.
//
// The constructor fails closed. A nil resolver refuses everybody. A nil check
// disables the exception and leaves the strict gate. A check error is a 500,
// because a database failure must not read as a refusal.
func RequireResolvedPermissionOrPersonalProject(
	resolver auth.PermissionResolver,
	mode string,
	isPersonal PersonalProjectCheck,
	personalPermission string,
	required ...string,
) func(http.Handler) http.Handler {
	requiredSet := permissionSet(required)
	personalSet := permissionSet([]string{personalPermission})

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user, ok := auth.UserFromContext(r.Context())
			if !ok {
				apierr.WriteStatus(w, http.StatusUnauthorized, "authentication required")
				return
			}
			projectID := chi.URLParam(r, "projectID")
			if resolver == nil || projectID == "" {
				apierr.WriteStatus(w, http.StatusForbidden, "insufficient permissions")
				return
			}

			resolution, err := resolver.ResolvePermissions(r.Context(), user, mode, projectID)
			if err != nil {
				writeResolverError(w, r, err)
				return
			}
			granted := permissionSet(resolution.Permissions)
			admitted := hasIntersection(requiredSet, granted)
			if !admitted && isPersonal != nil && personalPermission != "" &&
				resolution.UserID > 0 && hasIntersection(personalSet, granted) {
				personal, checkErr := isPersonal(r.Context(), projectID, resolution.UserID)
				if checkErr != nil {
					slog.ErrorContext(r.Context(), "personal project check failed",
						"error", checkErr, "method", r.Method, "path", r.URL.Path)
					apierr.WriteStatus(w, http.StatusInternalServerError, "permission resolution failed")
					return
				}
				admitted = personal
			}
			if !admitted {
				apierr.WriteStatus(w, http.StatusForbidden, "insufficient permissions")
				return
			}

			user.UserID = strconv.FormatInt(resolution.UserID, 10)
			next.ServeHTTP(w, r.WithContext(auth.ContextWithUser(r.Context(), user)))
		})
	}
}

// ResolvedPermissionFlag runs the request with a context mark when the caller
// holds `permission` in the {projectID} project. It never refuses a request.
//
// Use it after a real gate, when one handler serves two audiences with two
// response scopes. The artifact ACL listing is the example: a member with
// `.view` sees only their own row, and a member with `.edit` sees every row.
// A resolver error leaves the mark unset, so the narrow scope is the default.
func ResolvedPermissionFlag(
	resolver auth.PermissionResolver,
	mode string,
	mark func(context.Context) context.Context,
	permission string,
) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user, ok := auth.UserFromContext(r.Context())
			projectID := chi.URLParam(r, "projectID")
			if ok && resolver != nil && mark != nil && projectID != "" {
				resolution, err := resolver.ResolvePermissions(r.Context(), user, mode, projectID)
				if err == nil && hasIntersection(permissionSet([]string{permission}), permissionSet(resolution.Permissions)) {
					r = r.WithContext(mark(r.Context()))
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}
