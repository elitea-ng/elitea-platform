package main

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	localturnsapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/localturns"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/localturn"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/audit"
	dbrepos "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/legacyrbac"
)

// composeLocalTurns builds startLocalTurn / commitLocalTurn (ADR-0029
// decision 5c). It needs a database and nothing from the runtime: no worker
// runs a local turn. The policy reader is the router's own
// nativepolicy.Service, so an admin save that turns local work off reaches the
// next start on this replica at once. The recall resolver is a MemoriesRepo,
// the same implementation cloud admission uses.
//
// The answer is a nil interface, never a typed nil, when there is no pool.
func composeLocalTurns(
	pool *pgxpool.Pool,
	policy localturn.PolicyReader,
	recorder audit.Recorder,
	groupAuth apimw.AuthConfig,
	logger *slog.Logger,
) (http.Handler, error) {
	// No database, or no credential plane at all (the zero group AuthConfig):
	// nothing to serve, and nothing to authenticate a token with.
	if pool == nil || groupAuth.PrincipalValidator == nil {
		return nil, nil
	}
	service, err := localturn.NewService(
		dbrepos.NewLocalTurnsRepo(pool),
		policy,
		dbrepos.NewMemoriesRepo(pool),
		recorder,
		newLocalTurnExecutionID,
		logger,
	)
	if err != nil {
		return nil, err
	}
	route, err := localturnsapi.NewRoute(service, groupAuth, legacyrbac.NewPostgresResolver(pool))
	if err != nil {
		return nil, err
	}
	return route, nil
}

// newLocalTurnExecutionID mints the runtime's execution id shape: 16 random
// bytes as 32 lowercase hex characters.
func newLocalTurnExecutionID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}
