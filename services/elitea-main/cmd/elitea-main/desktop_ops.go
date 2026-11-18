package main

import (
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"

	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	desktopopsapi "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/desktopops"
	toolkitrun "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/toolkitrun"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/delegationedge"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/delegationnotice"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/localturn"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
	dbrepos "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/legacyrbac"
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
// platformTools runs a platform MCP server's tool (toolkit id 0) in process;
// the router binds it to the MCP server it builds. platformReplays is the
// cross-replica Idempotency-Key store those calls claim (nil without the
// live-update NATS: platform calls then answer 501).
func composeRemoteToolkit(
	pool *pgxpool.Pool,
	toolRuns toolkitrun.UseCase,
	authorizer desktopopsapi.RemoteToolAuthorizer,
	workerImplementation string,
	policy localturn.PolicyReader,
	recorder audit.Recorder,
	platformTools *desktopopsapi.PlatformToolBridge,
	platformReplays desktopopsapi.PlatformReplayStore,
	groupAuth apimw.AuthConfig,
	logger *slog.Logger,
) (http.Handler, error) {
	if pool == nil || groupAuth.PrincipalValidator == nil {
		return nil, nil
	}
	localTurns := dbrepos.NewLocalTurnsRepo(pool)
	turns, err := localturn.NewLiveTurns(localTurns, policy, logger)
	if err != nil {
		return nil, err
	}
	return desktopopsapi.NewRemoteToolkitRoute(desktopopsapi.RemoteToolkitDependencies{
		Runs: toolRuns, Worker: workerImplementation, Authorizer: authorizer, Turns: turns,
		Confirmations: localTurns, Audit: recorder, Platform: platformTools,
		Replays: platformReplays,
	}, groupAuth, legacyrbac.NewPostgresResolver(pool))
}

// composeChildTurns builds startLocalChildTurn and readLocalChildTurn (issue
// 1239): a nested agent of a live local turn run as an ordinary cloud turn
// through the chat start use case (start), authorized by the 5a version
// service (authorizer: the turn's own nested agents only). start or
// authorizer is nil where no agent plane is composed, and the routes then
// answer 501.
func composeChildTurns(
	pool *pgxpool.Pool,
	start desktopopsapi.ChildAgentStarter,
	authorizer desktopopsapi.ChildAgentAuthorizer,
	policy localturn.PolicyReader,
	recorder audit.Recorder,
	groupAuth apimw.AuthConfig,
	delegation delegationSwitch,
	logger *slog.Logger,
) (http.Handler, error) {
	if pool == nil || groupAuth.PrincipalValidator == nil {
		return nil, nil
	}
	turns, err := localturn.NewLiveTurns(dbrepos.NewLocalTurnsRepo(pool), policy, logger)
	if err != nil {
		return nil, err
	}
	edge, editors, err := composeDelegationEdge(pool, delegation)
	if err != nil {
		return nil, err
	}
	return desktopopsapi.NewChildTurnRoute(desktopopsapi.ChildTurnDependencies{
		Start: start, Authorizer: authorizer, Turns: turns,
		Store: dbrepos.NewLocalChildTurnsRepo(pool), Audit: recorder,
		Delegation: edge, Editors: editors,
	}, groupAuth, legacyrbac.NewPostgresResolver(pool))
}

// delegationSwitch is what the nested agent's delegation rule reads: the grant
// switch (ELITEA_DELEGATION_GRANTS_ENFORCED, off by default) and the platform's
// `delegation` section.
type delegationSwitch struct {
	grantsEnforced bool
	settings       delegationedge.SettingsReader
}

// composeDelegationEdge builds the delegation rule of the edge from a local
// turn's agent to a nested agent it starts as a cloud child (issue 2214). It
// answers no evaluator while the grant switch is off, so the child start is
// unchanged.
func composeDelegationEdge(pool *pgxpool.Pool, delegation delegationSwitch) (*delegationedge.Evaluator, delegationnotice.EditorLister, error) {
	if !delegation.grantsEnforced || delegation.settings == nil {
		return nil, nil, nil
	}
	sources, err := dbrepos.NewWorkerEgressSources(pool)
	if err != nil {
		return nil, nil, err
	}
	callees, err := dbrepos.NewDelegationCallees(pool, sources)
	if err != nil {
		return nil, nil, err
	}
	editors, err := dbrepos.NewDelegationEditors(pool)
	if err != nil {
		return nil, nil, err
	}
	evaluator, err := delegationedge.New(delegation.settings, callees, dbrepos.NewDelegationGrantReader(pool))
	if err != nil {
		return nil, nil, err
	}
	return evaluator, editors, nil
}
