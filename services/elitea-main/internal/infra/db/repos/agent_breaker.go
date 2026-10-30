package repos

// The agent circuit breaker's durable half (ADR-0041 decision 2, #1498,
// shared 0213): the breaker's window of ended agent runs, the reads the
// main.agent_breaker routine evaluates, and the trip — a suspension of
// source breaker at agent scope, written with its security audit event in
// one transaction.
//
// Every read is bounded and indexed:
//
//   - IngestRuns ranges over execution_jobs_agent_settled (0213) for the
//     last few minutes and reads each run's step records by
//     run_steps_seq_key (run_id, seq).
//   - Observe reads one agent's window by agent_breaker_runs_agent and its
//     suspensions by execution_suspensions_project.
//   - SpendLastHour ranges over idx_llm_request_logs_time for one hour of
//     attributed calls and joins each execution by its primary key. It runs
//     only when a spend limit is set.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/breaker"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/safety"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// BreakerNotificationEventType is the notification an agent's owners and
// the project admins get when the breaker stops the agent. The web client
// renders meta.message.
const BreakerNotificationEventType = "agent_breaker_tripped"

// ActionBreakerTripped is the security audit action of a trip.
const ActionBreakerTripped = "agent.breaker.tripped"

// AgentBreaker implements breaker.Store.
type AgentBreaker struct {
	pool        *pgxpool.Pool
	appender    securityaudit.Appender
	suspensions *ExecutionSuspensionRepository
}

var _ breaker.Store = (*AgentBreaker)(nil)

// NewAgentBreaker builds the store.
func NewAgentBreaker(pool *pgxpool.Pool, appender securityaudit.Appender) (*AgentBreaker, error) {
	if pool == nil || appender == nil {
		return nil, errors.New("agent breaker: a pool and an appender are required")
	}
	suspensions, err := NewExecutionSuspensionRepository(pool)
	if err != nil {
		return nil, err
	}
	return &AgentBreaker{pool: pool, appender: appender, suspensions: suspensions}, nil
}

// ingestRunsSQL copies the application runs that ended in the last $1
// seconds into the window, with the longest streak of consecutive failures
// of one tool (by name and toolkit) in the run's step records. A tool's
// streak counts its own calls in order; a call of another tool between two
// failures does not break it, a success of the same tool does. A row that
// changed (a late step record, a settlement that moved) is updated; one that
// did not is left alone, so RETURNING names only the agents with news.
const ingestRunsSQL = `
INSERT INTO elitea_runtime.agent_breaker_runs AS r
    (execution_id, generation, project_id, application_id, admitted_at, settled_at, state,
     tool_failure_streak, tool_failure_name)
SELECT j.execution_id, j.generation, j.resource_project_id, a.application_id, j.admitted_at, j.settled_at, j.state,
       COALESCE(streak.n, 0), COALESCE(left(streak.tool, 128), '')
FROM elitea_runtime.execution_jobs AS j
JOIN elitea_runtime.agent_execution_jobs AS a
  ON a.execution_id = j.execution_id AND a.generation = j.generation
LEFT JOIN LATERAL (
    SELECT steps.name AS tool, count(*)::integer AS n
    FROM (
        SELECT s.name, s.toolkit_id, s.seq,
               COALESCE(s.summary->>'status' = 'error', false) AS failed,
               row_number() OVER (PARTITION BY s.name, s.toolkit_id ORDER BY s.seq)
             - row_number() OVER (PARTITION BY s.name, s.toolkit_id, COALESCE(s.summary->>'status' = 'error', false)
                                  ORDER BY s.seq) AS grp
        FROM elitea_runtime.run_steps AS s
        WHERE s.run_id = j.execution_id AND s.kind = 'tool_call'
    ) AS steps
    WHERE steps.failed
    GROUP BY steps.name, steps.toolkit_id, steps.grp
    ORDER BY count(*) DESC, min(steps.seq)
    LIMIT 1
) AS streak ON TRUE
WHERE j.capability_id = 'agent.execute.application.v1'
  AND j.settled_at IS NOT NULL
  AND j.settled_at > now() - make_interval(secs => $1::double precision)
  AND j.state IN ('SUCCEEDED', 'FAILED', 'CANCELLED')
  AND a.application_id IS NOT NULL
  AND j.resource_project_id > 0
ON CONFLICT (execution_id, generation) DO UPDATE
SET state = EXCLUDED.state, settled_at = EXCLUDED.settled_at,
    tool_failure_streak = EXCLUDED.tool_failure_streak, tool_failure_name = EXCLUDED.tool_failure_name,
    recorded_at = clock_timestamp()
WHERE (r.state, r.settled_at, r.tool_failure_streak, r.tool_failure_name)
      IS DISTINCT FROM (EXCLUDED.state, EXCLUDED.settled_at, EXCLUDED.tool_failure_streak, EXCLUDED.tool_failure_name)
RETURNING r.project_id, r.application_id`

// IngestRuns implements breaker.Store.
func (b *AgentBreaker) IngestRuns(ctx context.Context, lookback time.Duration) ([]breaker.AgentKey, error) {
	rows, err := b.pool.Query(ctx, ingestRunsSQL, lookback.Seconds())
	if err != nil {
		return nil, fmt.Errorf("agent breaker: copy the ended runs: %w", err)
	}
	defer rows.Close()
	seen := map[breaker.AgentKey]bool{}
	var out []breaker.AgentKey
	for rows.Next() {
		var key breaker.AgentKey
		if err := rows.Scan(&key.ProjectID, &key.ApplicationID); err != nil {
			return nil, fmt.Errorf("agent breaker: scan an ended run: %w", err)
		}
		if !seen[key] {
			seen[key] = true
			out = append(out, key)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("agent breaker: copy the ended runs: %w", err)
	}
	return out, nil
}

// spendLastHourSQL is each agent's spend in the last hour: the model calls
// priced at the catalogue rate per row (as the run analytics price them)
// and the metered tool calls at the cost the gateway billed. A call made
// before the agent's last resume does not count.
const spendLastHourSQL = `
WITH calls AS (
    SELECT l.execution_id, l.occurred_at,
           CASE WHEN l.operation = 'tool' THEN COALESCE(l.cost_usd, 0)
                ELSE COALESCE(l.prompt_tokens::numeric * m.input_cost_per_1m_tokens / 1000000, 0)
                   + COALESCE(l.completion_tokens::numeric * m.output_cost_per_1m_tokens / 1000000, 0)
           END AS cost
    FROM gateway.llm_request_logs AS l
    LEFT JOIN gateway.gateway_models AS m
      ON m.provider = l.provider AND m.model_name = l.model AND l.operation <> 'tool'
    WHERE l.occurred_at > now() - interval '1 hour' AND l.execution_id IS NOT NULL
),
runs AS (
    SELECT DISTINCT ON (c.execution_id) c.execution_id, j.resource_project_id::bigint AS project_id,
           a.application_id, last.resumed_at
    FROM (SELECT DISTINCT execution_id FROM calls) AS c
    JOIN elitea_runtime.agent_execution_jobs AS a ON a.execution_id = c.execution_id
    JOIN elitea_runtime.execution_jobs AS j ON j.execution_id = a.execution_id AND j.generation = a.generation
    LEFT JOIN LATERAL (
        SELECT max(s.resumed_at) AS resumed_at FROM elitea_runtime.execution_suspensions AS s
        WHERE s.scope = 'agent' AND s.project_id = j.resource_project_id
          AND s.scope_key = a.application_id::text
    ) AS last ON TRUE
    WHERE a.application_id IS NOT NULL AND j.capability_id = 'agent.execute.application.v1'
    ORDER BY c.execution_id, a.generation
)
SELECT r.project_id, r.application_id, sum(c.cost)::double precision
FROM calls AS c
JOIN runs AS r ON r.execution_id = c.execution_id
WHERE r.resumed_at IS NULL OR c.occurred_at > r.resumed_at
GROUP BY r.project_id, r.application_id`

// SpendLastHour implements breaker.Store.
func (b *AgentBreaker) SpendLastHour(ctx context.Context) (map[breaker.AgentKey]float64, error) {
	rows, err := b.pool.Query(ctx, spendLastHourSQL)
	if err != nil {
		return nil, fmt.Errorf("agent breaker: read the spend: %w", err)
	}
	defer rows.Close()
	out := map[breaker.AgentKey]float64{}
	for rows.Next() {
		var (
			key breaker.AgentKey
			usd float64
		)
		if err := rows.Scan(&key.ProjectID, &key.ApplicationID, &usd); err != nil {
			return nil, fmt.Errorf("agent breaker: scan the spend: %w", err)
		}
		out[key] = usd
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("agent breaker: read the spend: %w", err)
	}
	return out, nil
}

// AnySpendOverride implements breaker.Store.
func (b *AgentBreaker) AnySpendOverride(ctx context.Context) (bool, error) {
	var found bool
	err := b.pool.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM elitea_lifecycle.agent_registry
               WHERE CASE WHEN jsonb_typeof(circuit_breaker->'spend_per_hour_usd') = 'number'
                          THEN (circuit_breaker->'spend_per_hour_usd')::numeric > 0
                          ELSE false END)`).Scan(&found)
	if err != nil {
		return false, fmt.Errorf("agent breaker: read the spend overrides: %w", err)
	}
	return found, nil
}

// Observe implements breaker.Store.
func (b *AgentBreaker) Observe(ctx context.Context, key breaker.AgentKey, window int) (breaker.Observation, error) {
	observation := breaker.Observation{Key: key, Tier: agentlifecycle.TierLow, Outcomes: []string{}}
	agentRefText := strconv.FormatInt(key.ApplicationID, 10)
	var (
		active    *bool
		resumedAt *time.Time
	)
	if err := b.pool.QueryRow(ctx, `
SELECT bool_or(resumed_at IS NULL), max(resumed_at)
FROM elitea_runtime.execution_suspensions
WHERE scope = 'agent' AND project_id = $1 AND scope_key = $2`, key.ProjectID, agentRefText).Scan(&active, &resumedAt); err != nil {
		return observation, fmt.Errorf("agent breaker: read the agent's suspensions: %w", err)
	}
	observation.Suspended = active != nil && *active
	if observation.Suspended {
		return observation, nil
	}

	var (
		tier     string
		override []byte
	)
	err := b.pool.QueryRow(ctx, `
SELECT risk_tier, circuit_breaker FROM elitea_lifecycle.agent_registry
WHERE tenant_project_id = $1 AND application_id = $2`, key.ProjectID, key.ApplicationID).Scan(&tier, &override)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// No registry record yet: tier low, no override.
	case err != nil:
		return observation, fmt.Errorf("agent breaker: read the agent's registry record: %w", err)
	default:
		if parsed := agentlifecycle.Tier(tier); parsed.Valid() {
			observation.Tier = parsed
		}
		// A stored override that no longer parses is ignored: the tier
		// default still applies, and the registry edit refuses it.
		if parsed, err := breaker.ParseOverride(override); err == nil {
			observation.Override = parsed
		}
	}

	if window > 0 {
		rows, err := b.pool.Query(ctx, `
SELECT state FROM elitea_runtime.agent_breaker_runs
WHERE project_id = $1 AND application_id = $2 AND state <> 'CANCELLED'
  AND admitted_at > COALESCE($3::timestamptz, '-infinity'::timestamptz)
ORDER BY settled_at DESC
LIMIT $4`, key.ProjectID, key.ApplicationID, resumedAt, window)
		if err != nil {
			return observation, fmt.Errorf("agent breaker: read the agent's window: %w", err)
		}
		states, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return observation, fmt.Errorf("agent breaker: read the agent's window: %w", err)
		}
		observation.Outcomes = states
	}

	err = b.pool.QueryRow(ctx, `
SELECT count(*) OVER (), tool_failure_streak, tool_failure_name, execution_id
FROM elitea_runtime.agent_breaker_runs
WHERE project_id = $1 AND application_id = $2
  AND settled_at > now() - interval '1 hour'
  AND admitted_at > COALESCE($3::timestamptz, '-infinity'::timestamptz)
ORDER BY tool_failure_streak DESC, settled_at DESC
LIMIT 1`, key.ProjectID, key.ApplicationID, resumedAt).Scan(
		&observation.RunsLastHour, &observation.ToolStreak, &observation.ToolName, &observation.ToolExecutionID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return observation, fmt.Errorf("agent breaker: read the agent's last hour: %w", err)
	}
	return observation, nil
}

// Trip implements breaker.Store: the suspension (which cancels the agent's
// runs in flight, behind the admission fence) and the
// ai.elitea.security.v1.agent.breaker event commit together or not at all.
func (b *AgentBreaker) Trip(ctx context.Context, trip breaker.Trip) (safety.Suspension, error) {
	tx, err := b.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite})
	if err != nil {
		return safety.Suspension{}, fmt.Errorf("agent breaker: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	created, err := b.suspensions.CreateSuspensionTx(ctx, tx, safety.NewSuspension{
		Target: safety.Target{Scope: safety.ScopeAgent, ProjectID: trip.Key.ProjectID, ApplicationID: trip.Key.ApplicationID},
		Reason: trip.Reason(), ActorID: 0, Source: safety.SourceBreaker,
		ResumeRequires: trip.ResumeRequires, Detail: trip.Detail(),
	})
	if err != nil {
		return safety.Suspension{}, err
	}
	event := securityaudit.SecurityEvent{
		Type: securityaudit.TypeAgentBreaker, TenantProjectID: trip.Key.ProjectID, Action: ActionBreakerTripped,
		Reasons: trip.AuditReasons(created.ID), PrincipalRef: "system:agent_breaker",
		ResourceType: "agent", ResourceID: strconv.FormatInt(trip.Key.ApplicationID, 10),
		ExecutionID: trip.ExecutionID,
	}
	if name, ok := trip.Detail()["tool_name"].(string); ok {
		event.ToolName = name
	}
	if err := b.appender.Append(ctx, tx, event); err != nil {
		return safety.Suspension{}, fmt.Errorf("agent breaker: append the audit event: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			if existing, findErr := b.suspensions.activeFor(ctx, b.pool, created.Target); findErr == nil {
				return safety.Suspension{}, &safety.AlreadySuspendedError{Existing: existing}
			}
		}
		return safety.Suspension{}, fmt.Errorf("agent breaker: commit: %w", err)
	}
	return created, nil
}

// NotifyTrip implements breaker.Store: one notification to each owner of
// the agent (its registry owners, else its author) and each admin of the
// project. The uuid names the suspension and the person, so a retried pass
// never repeats one.
func (b *AgentBreaker) NotifyTrip(ctx context.Context, trip breaker.Trip, suspension safety.Suspension) (int, error) {
	recipients := map[int64]bool{}
	var order []int64
	add := func(id int64) {
		if id > 0 && !recipients[id] {
			recipients[id] = true
			order = append(order, id)
		}
	}
	var business, technical *int64
	err := b.pool.QueryRow(ctx, `
SELECT business_owner_id, technical_owner_id FROM elitea_lifecycle.agent_registry
WHERE tenant_project_id = $1 AND application_id = $2`, trip.Key.ProjectID, trip.Key.ApplicationID).Scan(&business, &technical)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("agent breaker: read the agent's owners: %w", err)
	}
	if business != nil {
		add(*business)
	}
	if technical != nil {
		add(*technical)
	}
	name := ""
	if schema, err := tenantSchema(strconv.FormatInt(trip.Key.ProjectID, 10)); err == nil {
		var owner *int64
		var appName *string
		if err := b.pool.QueryRow(ctx, fmt.Sprintf(`SELECT owner_id::bigint, name FROM %s.applications WHERE id = $1`, schema),
			trip.Key.ApplicationID).Scan(&owner, &appName); err == nil {
			if len(order) == 0 && owner != nil {
				add(*owner)
			}
			if appName != nil {
				name = *appName
			}
		}
	}
	if admins, err := projectAdminIDs(ctx, b.pool, trip.Key.ProjectID); err == nil {
		for _, id := range admins {
			add(id)
		}
	}
	if name == "" {
		name = "#" + strconv.FormatInt(trip.Key.ApplicationID, 10)
	}
	message := fmt.Sprintf("The circuit breaker stopped agent %q. %s Who can resume it: %s.",
		name, trip.Reason(), resumeWords(trip.ResumeRequires))
	sent := 0
	var firstErr error
	for _, recipient := range order {
		meta := map[string]any{
			"message": message, "project_id": trip.Key.ProjectID, "application_id": trip.Key.ApplicationID,
			"suspension_id": suspension.ID, "rule": string(trip.Rule),
		}
		key := fmt.Sprintf("breaker-%d-%d", suspension.ID, recipient)
		if err := insertReviewNotification(ctx, b.pool, key, trip.Key.ProjectID, recipient, BreakerNotificationEventType, meta); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		sent++
	}
	return sent, firstErr
}

func resumeWords(requires safety.ResumeRequirement) string {
	switch requires {
	case safety.ResumeGovernanceRole:
		return "a platform administrator"
	case safety.ResumeTwoPerson:
		return "two different platform administrators"
	}
	return "a project or platform administrator"
}

// projectAdminIDs reads the project's admins, by the role rule chat
// authority applies (conversation_activity.go). An absent role projection
// has no admins.
func projectAdminIDs(ctx context.Context, pool *pgxpool.Pool, projectID int64) ([]int64, error) {
	var present bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('public.auth_core__project_user_role') IS NOT NULL
		AND to_regclass('public.auth_core__project_role') IS NOT NULL`).Scan(&present); err != nil || !present {
		return nil, err
	}
	rows, err := pool.Query(ctx, `SELECT DISTINCT assignment.user_id::bigint
		FROM public.auth_core__project_user_role assignment
		JOIN public.auth_core__project_role role ON role.id = assignment.role_id AND role.project_id = assignment.project_id
		WHERE assignment.project_id = $1 AND lower(role.name) IN ('admin', 'super_admin')`, projectID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int64])
}

// Prune implements breaker.Store.
func (b *AgentBreaker) Prune(ctx context.Context, retention time.Duration) (int64, error) {
	tag, err := b.pool.Exec(ctx, `
DELETE FROM elitea_runtime.agent_breaker_runs
WHERE settled_at < now() - make_interval(secs => $1::double precision)`, retention.Seconds())
	if err != nil {
		return 0, fmt.Errorf("agent breaker: prune the window: %w", err)
	}
	return tag.RowsAffected(), nil
}

// checkBreakerOverride parses a posted circuit breaker override and refuses
// one that loosens the tier default (422 circuit_breaker_override_loosens)
// against the circuit_breaker section as stored now. It answers the
// override's stored form.
func checkBreakerOverride(ctx context.Context, pool *pgxpool.Pool, tier agentlifecycle.Tier, raw []byte) ([]byte, error) {
	override, err := breaker.ParseOverride(raw)
	if err != nil {
		return nil, agentlifecycle.Refuse(agentlifecycle.InvalidCode,
			"circuit_breaker must be an object of error_rate_percent, error_rate_window_runs, spend_per_hour_usd, "+
				"tool_failure_streak and runs_per_hour: "+err.Error())
	}
	// Read on the pool, not the edit's transaction: a database without the
	// config table must not abort the edit.
	values, err := platformconfig.Load(ctx, pool, platformconfig.SectionCircuitBreaker)
	if err != nil && !platformconfig.UndefinedTable(err) {
		return nil, fmt.Errorf("agent lifecycle: read the circuit_breaker section: %w", err)
	}
	settings := breaker.SettingsFromValues(values)
	var loose *breaker.LooseningError
	if err := breaker.CheckOverride(settings.Tier(tier).Rules, override); errors.As(err, &loose) {
		fields := make([]any, 0, len(loose.Fields))
		for _, field := range loose.Fields {
			fields = append(fields, field)
		}
		return nil, agentlifecycle.RefuseWith(agentlifecycle.BreakerOverrideLoosensCode,
			"A circuit breaker override can only tighten the default of the agent's tier ("+string(tier)+").",
			map[string]any{"fields": fields})
	}
	return override.Encode(), nil
}

// jsonEqual compares two stored JSON objects by value.
func jsonEqual(a, b []byte) bool {
	var left, right any
	if json.Unmarshal(a, &left) != nil || json.Unmarshal(b, &right) != nil {
		return string(a) == string(b)
	}
	encodedLeft, _ := json.Marshal(left)
	encodedRight, _ := json.Marshal(right)
	return string(encodedLeft) == string(encodedRight)
}

// breakerOverrideReasons are the reason codes of an override change: each
// set limit as "<field>:<value>", or "cleared".
func breakerOverrideReasons(raw []byte) []string {
	override, err := breaker.ParseOverride(raw)
	if err != nil || override.Empty() {
		return []string{"cleared"}
	}
	var reasons []string
	addInt := func(name string, value *int) {
		if value != nil {
			reasons = append(reasons, name+":"+strconv.Itoa(*value))
		}
	}
	addInt("error_rate_percent", override.ErrorRatePercent)
	addInt("error_rate_window_runs", override.ErrorRateWindowRuns)
	if override.SpendPerHourUSD != nil {
		reasons = append(reasons, "spend_per_hour_usd:"+strconv.FormatFloat(*override.SpendPerHourUSD, 'f', -1, 64))
	}
	addInt("tool_failure_streak", override.ToolFailureStreak)
	addInt("runs_per_hour", override.RunsPerHour)
	return reasons
}
