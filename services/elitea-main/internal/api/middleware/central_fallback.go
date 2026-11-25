package middleware

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
	"github.com/go-chi/chi/v5"
)

// CentralFallback is the second way through a project route gate: it runs
// only when the caller does not hold the route's permission in the project,
// and answers the request to continue with, or admitted false. An error is a
// failed read; the gate answers it as a resolver error.
type CentralFallback func(r *http.Request) (next *http.Request, admitted bool, err error)

// RequireResolvedPermissionsOrCentral is RequireResolvedPermissions with a
// central fallback (ADR-0059 R0.6b): a caller who holds `required` in the
// `{projectID}` of the path passes as before, with the resolved set on the
// context, and the fallback is never asked. A caller who does not is
// admitted only when the fallback admits it; otherwise the answer is the
// gate's own 403.
//
// The fallback decides on its own terms (the central override's reviewed
// allowlist); the member path is not widened by it.
//
// The gate is marked as an administration gate, so a holder of an
// administration permission passes the maintenance gate on it during a window
// (ADR-0059 R0.4b). Such a request is decided by the fallback alone: the
// project-member path is the product, which the window closes.
func RequireResolvedPermissionsOrCentral(
	resolver auth.PermissionResolver,
	mode string,
	fallback CentralFallback,
	required ...string,
) func(http.Handler) http.Handler {
	return RequireResolvedPermissionsForProjectOrCentral(resolver, mode, projectIDFromPath, fallback, required...)
}

// projectIDFromPath is the `{projectID}` path parameter, as it is.
func projectIDFromPath(r *http.Request) (string, bool) {
	projectID := chi.URLParam(r, "projectID")
	return projectID, projectID != ""
}

// RequireResolvedPermissionsForProjectOrCentral is
// RequireResolvedPermissionsOrCentral for a route that reads its project with
// its own extractor (the reviewed Configurations routes accept a decimal
// `{projectID}` only, #3024). A project the extractor refuses is refused
// before anything is resolved, and the fallback is never asked.
func RequireResolvedPermissionsForProjectOrCentral(
	resolver auth.PermissionResolver,
	mode string,
	project ProjectIDExtractor,
	fallback CentralFallback,
	required ...string,
) func(http.Handler) http.Handler {
	requiredSet := permissionSet(required)
	return func(next http.Handler) http.Handler {
		return MarkAdministrationRoute(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user, ok := auth.UserFromContext(r.Context())
			if !ok {
				apierr.WriteStatus(w, http.StatusUnauthorized, "authentication required")
				return
			}
			if resolver == nil || project == nil {
				apierr.WriteStatus(w, http.StatusForbidden, "insufficient permissions")
				return
			}
			projectID, valid := project(r)
			if !valid {
				apierr.WriteStatus(w, http.StatusForbidden, "insufficient permissions")
				return
			}
			resolution, err := auth.PermissionResolution{}, auth.ErrPermissionDenied
			if !InMaintenanceAsNarrowHolder(r.Context()) {
				resolution, err = resolver.ResolvePermissions(r.Context(), user, mode, projectID)
			}
			if err != nil && !errors.Is(err, auth.ErrPermissionDenied) {
				writeResolverError(w, r, err)
				return
			}
			if err == nil && hasIntersection(requiredSet, permissionSet(resolution.Permissions)) {
				user.UserID = strconv.FormatInt(resolution.UserID, 10)
				ctx := auth.ContextWithUser(r.Context(), user)
				ctx = context.WithValue(ctx, resolvedProjectPermissionsKey{}, resolvedProjectPermissions{
					mode: mode, projectID: projectID, permissions: append([]string(nil), resolution.Permissions...),
				})
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
			if fallback != nil {
				admittedRequest, admitted, fallbackErr := fallback(r)
				if fallbackErr != nil {
					writeResolverError(w, r, fallbackErr)
					return
				}
				if admitted {
					next.ServeHTTP(w, admittedRequest)
					return
				}
			}
			apierr.WriteStatus(w, http.StatusForbidden, "insufficient permissions")
		}))
	}
}

// HoldsCentralPermission answers whether the caller holds any of `permissions`
// in the administration mode (ResolveCentralPermission without the id).
func HoldsCentralPermission(ctx context.Context, resolver auth.PermissionResolver, permissions ...string) (bool, error) {
	_, held, err := ResolveCentralPermission(ctx, resolver, permissions...)
	return held, err
}

// ResolveCentralPermission answers whether the caller holds any of
// `permissions` in the administration mode, and the resolved numeric user id
// when it does. A native device session holds none at any administration check
// that stands in for a route gate (ADR-0059 decision 7), and a denied
// resolution is false, not an error.
func ResolveCentralPermission(
	ctx context.Context, resolver auth.PermissionResolver, permissions ...string,
) (userID int64, held bool, err error) {
	user, ok := auth.UserFromContext(ctx)
	if !ok || resolver == nil || len(permissions) == 0 {
		return 0, false, nil
	}
	if nativeAtAdministrationGate(user, auth.PermissionModeAdministration) {
		return 0, false, nil
	}
	resolution, err := resolver.ResolvePermissions(ctx, user, auth.PermissionModeAdministration, "")
	if errors.Is(err, auth.ErrPermissionDenied) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	if !hasIntersection(permissionSet(permissions), permissionSet(resolution.Permissions)) {
		return 0, false, nil
	}
	return resolution.UserID, true, nil
}
