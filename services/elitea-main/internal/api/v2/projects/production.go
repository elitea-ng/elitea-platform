package projects

import (
	"errors"
	"net/http"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

var ErrInvalidCurrentProjectListRoute = errors.New("invalid current project-list route dependencies")

// CurrentProjectListRoute is an opaque production route. Construction binds
// trusted forwarded-identity verification, mutable-principal validation, and
// the current project-view permission before the SQLC-backed handler can run.
type CurrentProjectListRoute struct {
	handler http.Handler
}

func NewCurrentProjectListRoute(
	projects CurrentProjectLister,
	authConfig apimw.AuthConfig,
	permissions auth.MembershipPermissionResolver,
) (*CurrentProjectListRoute, error) {
	if projects == nil || permissions == nil {
		return nil, ErrInvalidCurrentProjectListRoute
	}
	// PrincipalValidator and ForwardedIdentityVerifier are optional — when
	// nil the auth middleware falls back to session-cookie verification
	// (OIDC deployments without Form auth / ELITEA_AUTH_CONFIG_FILE).

	handler := http.Handler(http.HandlerFunc(NewCurrentProjectListHandler(projects).GetCurrentProjectList))
	// Gated on the caller's OWN memberships, not on the public project that
	// stands in the path (#830). The handler answers with the caller's
	// projects only — ListCurrentUserProjects joins the membership table — so
	// the question the gate must ask is whether the caller may view a project
	// they belong to. Asking it of the public project instead refused every
	// account that is not enrolled there, which on a Go-provisioned install is
	// every account but the first: the sign-in that creates `project_user_<id>`
	// adds no membership of project 1.
	//
	// The `.../default/1` path segment is unchanged, because it is part of the
	// reference UI's URL. It now selects the public project for the
	// `check_public_role` filter alone, which is the only thing the handler
	// ever used it for.
	handler = apimw.RequireResolvedMembershipPermissions(
		permissions,
		CurrentProjectListMode,
		CurrentProjectListPermission,
	)(handler)
	handler = apimw.Auth(authConfig)(handler)
	return &CurrentProjectListRoute{handler: handler}, nil
}

func (route *CurrentProjectListRoute) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if route == nil || route.handler == nil {
		http.NotFound(writer, request)
		return
	}
	route.handler.ServeHTTP(writer, request)
}
