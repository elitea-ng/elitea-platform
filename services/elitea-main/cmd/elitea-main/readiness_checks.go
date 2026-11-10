package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"

	"github.com/elitea-ng/elitea-platform/libs/go/faults"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/health"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/depfailure"
)

// readinessMonitorInterval is how often Main checks its dependencies on its
// own, so the loss and recovery lines appear even when nothing probes it.
const readinessMonitorInterval = 5 * time.Second

// degradedRunnerSource reports runtime runners restarted after a dependency
// failure (runtimecomposition.Runtime).
type degradedRunnerSource interface {
	DegradedRunners() []string
}

// mainReadinessChecks are Main's readiness dependencies (FU-275):
//
//   - db (critical): one ping of the main pool, through its own
//     connections. The runtime pools reach the same PostgreSQL; their small
//     pools are not pinged, so an admission burst that holds every runtime
//     connection cannot read as a database outage and take every replica out
//     of service.
//   - nats: the client-side state of each NATS connection, read with
//     Conn.Status (no round trip). Reconnecting is degraded, not not-ready:
//     admitted commands wait in the outbox and claimed runs renew their
//     leases over gRPC, so taking every Main replica out of service for a
//     shared NATS blip would turn a degraded UI into an outage. A closed
//     connection never comes back on its own, so it is unavailable (503).
//   - runners: runtime runners restarted after a dependency failure; their
//     restarts are logged by the supervisor, so this check logs nothing.
func mainReadinessChecks(pools []*pgxpool.Pool, natsStatuses []func() nats.Status, runners degradedRunnerSource) []health.Check {
	checks := []health.Check{{
		Name:      "db",
		Critical:  true,
		Probe:     func(ctx context.Context) (string, string) { return probePools(ctx, pools) },
		Lost:      faults.DependencyDatabaseUnavailable.Sentence(),
		Code:      faults.DependencyDatabaseUnavailable,
		Recovered: "Main reached its database again",
	}}
	if len(natsStatuses) != 0 {
		checks = append(checks, health.Check{
			Name:      "nats",
			Probe:     func(context.Context) (string, string) { return probeNATS(natsStatuses) },
			Lost:      faults.DependencyNatsUnavailable.Sentence(),
			Code:      faults.DependencyNatsUnavailable,
			Recovered: "Main reached NATS again",
		})
	}
	if runners != nil {
		checks = append(checks, health.Check{
			Name: "runners",
			Probe: func(context.Context) (string, string) {
				names := runners.DegradedRunners()
				if len(names) == 0 {
					return health.StateOK, ""
				}
				return health.StateDegraded, "restarted after a dependency failure: " + strings.Join(names, ", ")
			},
		})
	}
	return checks
}

func probePools(ctx context.Context, pools []*pgxpool.Pool) (string, string) {
	errs := make([]error, len(pools))
	var wg sync.WaitGroup
	for i, pool := range pools {
		if pool == nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = pool.Ping(ctx)
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return health.StateUnavailable, depfailure.Describe(err)
	}
	return health.StateOK, ""
}

func probeNATS(statuses []func() nats.Status) (string, string) {
	state, cause := health.StateOK, ""
	for _, status := range statuses {
		switch current := status(); current {
		case nats.CONNECTED:
		case nats.CLOSED:
			return health.StateUnavailable, "a NATS connection is closed and will not reconnect"
		default:
			state, cause = health.StateReconnecting, "a NATS connection is "+strings.ToLower(current.String())
		}
	}
	return state, cause
}
