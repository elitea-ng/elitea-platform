package main

import (
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	desktopopsapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/desktopops"
	toolkitrun "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/toolkitrun"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/localturn"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/audit"
	dbrepos "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
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

// composeRemoteToolkit builds executeRemoteToolkitTool (ADR-0029 decision 5b)
// over the SAME toolkit.call_tool.v1 use case test_tool holds, bound to a live
// local turn (the local turn routes' repository and native policy reader) and
// authorized by the 5a freeze (authorizer). toolRuns or authorizer is nil
// where no toolkit worker or no agent plane is composed, and the route then
// answers 501. The worker implementation only words the refusal of a tool the
// worker cannot run remotely (the Rust worker admits read-only tools only).
func composeRemoteToolkit(
	pool *pgxpool.Pool,
	toolRuns toolkitrun.UseCase,
	authorizer desktopopsapi.RemoteToolAuthorizer,
	workerImplementation string,
	policy localturn.PolicyReader,
	recorder audit.Recorder,
	groupAuth apimw.AuthConfig,
	logger *slog.Logger,
) (http.Handler, error) {
	if pool == nil || groupAuth.PrincipalValidator == nil {
		return nil, nil
	}
	turns, err := localturn.NewLiveTurns(dbrepos.NewLocalTurnsRepo(pool), policy, logger)
	if err != nil {
		return nil, err
	}
	return desktopopsapi.NewRemoteToolkitRoute(desktopopsapi.RemoteToolkitDependencies{
		Runs: toolRuns, Worker: workerImplementation, Authorizer: authorizer, Turns: turns, Audit: recorder,
	}, groupAuth, legacyrbac.NewPostgresResolver(pool))
}
