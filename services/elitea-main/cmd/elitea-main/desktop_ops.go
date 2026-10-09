package main

import (
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	desktopopsapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/desktopops"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/legacyrbac"
)

// composeResolvedVersion builds resolveApplicationVersion (ADR-0029 decision
// 5a). The route is mounted wherever a database and a credential plane exist;
// resolved is nil where the agent plane is not composed, and the route then
// answers 501 rather than disappearing.
//
// The answer is a nil interface, never a typed nil, when there is no pool.
func composeResolvedVersion(
	pool *pgxpool.Pool,
	resolved desktopopsapi.ResolvedVersionUseCase,
	groupAuth apimw.AuthConfig,
) (http.Handler, error) {
	if pool == nil || groupAuth.PrincipalValidator == nil {
		return nil, nil
	}
	return desktopopsapi.NewResolvedVersionRoute(resolved, groupAuth, legacyrbac.NewPostgresResolver(pool))
}
