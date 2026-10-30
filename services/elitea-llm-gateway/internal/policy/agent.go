package policy

// agent.go — which agent a request was made from (ADR-0041 decision 4).
//
// A throttle per agent needs the request's agent. The gateway does not receive
// one: the edge signs the runtime EXECUTION id into the identity tuple
// (X-Elitea-Execution-Id, v2), and elitea-main records the run's agent at
// admission in elitea_runtime.agent_execution_jobs.application_id (shared
// 0194). This file joins the two.
//
// # Cost
//
// The lookup runs only when a loaded rate limit names agents
// (Snapshot.HasAgentThrottles) and the request carries an execution id. A run
// makes many LLM calls and its agent never changes, so a found agent is cached
// for agentCacheTTL. "No agent" (an ad hoc chat, a tool run) is cached for a
// shorter time.
//
// # When the lookup fails
//
// The agent stays unknown, and a row scoped to agents does not match the
// request: the throttle is NOT applied. That is the same posture as a broken
// rate-limit counter (Limiter fails open), for the same reason — a throttle is
// an overload control, and a database error must not refuse every agent call
// on the platform. Every such failure is counted (Failures) and logged at most
// once per agentFailureLogEvery.

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AgentLookup reads the agent of one runtime execution. found is false when
// the execution is not an agent run (or is unknown). An error means the
// answer is not known.
type AgentLookup interface {
	LookupAgent(ctx context.Context, executionID string) (ref AgentRef, found bool, err error)
}

const (
	// agentCacheTTL is how long a found agent is kept. A run's agent is fixed
	// at admission, so the bound only limits memory held for finished runs.
	agentCacheTTL = 10 * time.Minute
	// agentMissTTL is how long "no agent" is kept. It is short so that a run
	// read in the instant before its admission row commits is not mistaken for
	// an ad hoc run for long.
	agentMissTTL = 30 * time.Second
	// agentCacheMax bounds the cache. When it is full, expired entries are
	// dropped first and then the whole cache is reset: a cold lookup is cheap
	// (one primary-key read), unbounded memory is not.
	agentCacheMax = 10_000
	// agentLookupTimeout bounds one lookup on the admission path.
	agentLookupTimeout = 2 * time.Second
	// agentFailureLogEvery bounds the failure log to one line per interval.
	agentFailureLogEvery = time.Minute
	// maxExecutionIDBytes matches the request log's bound on the same id. A
	// longer value is not an id elitea-main minted, so it is not looked up.
	maxExecutionIDBytes = 128
)

type agentEntry struct {
	ref     AgentRef
	found   bool
	expires time.Time
}

// AgentResolver caches AgentLookup answers for the admission path.
type AgentResolver struct {
	lookup AgentLookup
	log    *slog.Logger
	now    func() time.Time

	mu    sync.Mutex
	cache map[string]agentEntry

	failures atomic.Int64
	lastLog  atomic.Int64
}

// NewAgentResolver builds a resolver. A nil lookup builds one that resolves
// nothing, so every agent-scoped row stays unmatched.
func NewAgentResolver(lookup AgentLookup, log *slog.Logger, now func() time.Time) *AgentResolver {
	if log == nil {
		log = slog.Default()
	}
	if now == nil {
		now = time.Now
	}
	return &AgentResolver{lookup: lookup, log: log, now: now, cache: map[string]agentEntry{}}
}

// Failures returns how many lookups failed. Each one is a request that an
// agent throttle could not see.
func (r *AgentResolver) Failures() int64 {
	if r == nil {
		return 0
	}
	return r.failures.Load()
}

// Resolve returns the agent of executionID, or the zero AgentRef when there is
// none or it cannot be known.
func (r *AgentResolver) Resolve(ctx context.Context, executionID string) AgentRef {
	if r == nil || r.lookup == nil || executionID == "" || len(executionID) > maxExecutionIDBytes {
		return AgentRef{}
	}
	now := r.now()
	r.mu.Lock()
	entry, hit := r.cache[executionID]
	r.mu.Unlock()
	if hit && now.Before(entry.expires) {
		return entry.ref
	}

	lctx, cancel := context.WithTimeout(ctx, agentLookupTimeout)
	defer cancel()
	ref, found, err := r.lookup.LookupAgent(lctx, executionID)
	if err != nil {
		r.failures.Add(1)
		if last := r.lastLog.Load(); now.UnixNano()-last >= int64(agentFailureLogEvery) &&
			r.lastLog.CompareAndSwap(last, now.UnixNano()) {
			r.log.Warn("policy: the agent of a run could not be read; agent throttles do NOT apply to its calls "+
				"until the lookup succeeds", "execution_id", executionID, "failures", r.failures.Load(), "err", err)
		}
		return AgentRef{}
	}
	if !found || !ref.Known() {
		ref, found = AgentRef{}, false
	}
	ttl := agentMissTTL
	if found {
		ttl = agentCacheTTL
	}
	r.mu.Lock()
	if len(r.cache) >= agentCacheMax {
		for k, e := range r.cache {
			if !now.Before(e.expires) {
				delete(r.cache, k)
			}
		}
		if len(r.cache) >= agentCacheMax {
			r.cache = map[string]agentEntry{}
		}
	}
	r.cache[executionID] = agentEntry{ref: ref, found: found, expires: now.Add(ttl)}
	r.mu.Unlock()
	return ref
}

// PostgresAgentLookup reads the agent from elitea_runtime.agent_execution_jobs
// (application_id, shared 0194) and the owning project from
// elitea_runtime.execution_jobs (resource_project_id). It is the same join the
// agent suspension uses to find the runs it cancels.
type PostgresAgentLookup struct{ pool *pgxpool.Pool }

// NewPostgresAgentLookup builds the lookup. A nil pool returns nil, which the
// resolver treats as "resolve nothing".
func NewPostgresAgentLookup(pool *pgxpool.Pool) AgentLookup {
	if pool == nil {
		return nil
	}
	return &PostgresAgentLookup{pool: pool}
}

const agentLookupSQL = `
SELECT j.resource_project_id, a.application_id
FROM elitea_runtime.agent_execution_jobs AS a
JOIN elitea_runtime.execution_jobs AS j
  ON j.execution_id = a.execution_id AND j.generation = a.generation
WHERE a.execution_id = $1 AND a.application_id IS NOT NULL
ORDER BY a.generation DESC
LIMIT 1`

// LookupAgent implements AgentLookup.
func (p *PostgresAgentLookup) LookupAgent(ctx context.Context, executionID string) (AgentRef, bool, error) {
	var (
		projectID     int
		applicationID int64
	)
	err := p.pool.QueryRow(ctx, agentLookupSQL, executionID).Scan(&projectID, &applicationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentRef{}, false, nil
	}
	if err != nil {
		return AgentRef{}, false, err
	}
	return AgentRef{ProjectID: projectID, ApplicationID: applicationID}, true, nil
}
