package repos

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// ComposeAgentLifecycle builds the store the way the process uses it: the
// security audit outbox of elitea-main, the tier table read from the
// agent_lifecycle Configuration section on every call (a save applies at
// once), the release gate (nil: NoGate), the kill switch gate's
// invalidation (nil: none) and the candidate digest of the release store.
func ComposeAgentLifecycle(pool *pgxpool.Pool, gate agentlifecycle.ReleaseGate, suspensionChanged func(id int64)) (*AgentLifecycle, error) {
	appender, err := securityaudit.NewPostgresAppender("elitea-main")
	if err != nil {
		return nil, err
	}
	policy := func(ctx context.Context) (agentlifecycle.Policy, error) {
		return platformconfig.LoadAgentLifecyclePolicy(ctx, pool)
	}
	releases, err := NewAgentReleases(pool)
	if err != nil {
		return nil, err
	}
	return NewAgentLifecycle(pool, appender, policy,
		WithLifecycleGate(gate), WithSuspensionResumed(suspensionChanged), WithCandidateDigester(releases))
}
