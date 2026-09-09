package repos

// AnalyticsRepo — the /elitea_core/analytics reads.
//
// # What used to be here, and why the refusal was right at the time
//
// Four methods, each a SELECT against `usage_records` or `tool_usage_records`.
// Neither table has ever existed: no migration creates them, nothing INSERTs
// into them, and every call raised PostgreSQL 42P01 on every deployment this
// service has ever had. It went unnoticed because the handler discarded the
// error (`summary, _ :=`) and answered 200 with hardcoded zeros, so a project
// with real spend and a project that does not exist produced byte-identical
// dashboards (issue #303).
//
// The queries were deleted and every method returned ErrNoSource, because at
// that moment nothing in the platform produced the figures. That was accurate
// when it was written and it is no longer accurate for half of this file, which
// is the point worth keeping: a disclosed gap is a claim about the corpus at a
// moment in time, and nothing re-checks it when the corpus changes.
//
// # What changed: gateway.llm_request_logs (shared migration 0099)
//
// The LLM Proxy work gave the gateway a per-request log — one row per request
// it served, billed or not, succeeded or not — carrying project_id, user_id,
// model, provider, prompt_tokens, completion_tokens, status, duration_ms and
// occurred_at. That is a direct producer for the figures this endpoint could
// not source before, and it sidesteps every modelling fork the old note listed
// as the reason not to guess:
//
//   - ONE project column. `elitea_runtime.execution_jobs` has
//     `resource_project_id` AND `projection_project_id` and they can differ, so
//     "analytics for project N" had no single answer. This table has
//     `project_id` and nothing else.
//   - ONE clock. The chat and trace tables are scoped by TENANT SCHEMA and
//     store `timestamp` where the gateway tables store `timestamptz`, so one
//     query spanning both needed dynamic SQL and an explicit cast — the kind of
//     mismatch that yields a plausible wrong window rather than an error. This
//     table is `timestamptz`, in one schema.
//   - PER-CALL ROWS. `gateway.llm_budget_accumulators` holds one row per
//     (scope, scope_id, period), so its count(*) counts billing periods, not
//     calls; /analytics_costs is careful to publish that as `rows`. This table
//     has a row per call, so `llm_calls` is a real count.
//
// # What is STILL refused, and why it is not the same kind of gap
//
//   - TOOL analytics. `p_<id>.chat_message_trace_step` records a
//     `tool_name` but no `toolkit_id`, and covers chat turns only — so a
//     "toolkit usage" table built from it would silently exclude every tool
//     call an agent made outside a chat.
//
//     NOTE(#618): `toolkit.call_tool.v1` (#340/#616) did NOT close this, and it
//     is worth saying why, because it looks as if it should have.
//     `elitea_runtime.execution_jobs` now carries one row per explicit tool run
//     with a project and an `admitted_at`, which is two of the five columns
//     ToolAnalytics needs. It carries NEITHER `toolkit_id` NOR `tool_name`:
//     that capability deliberately owns no binding table, because it dispatches
//     inline and its command scalars never need to survive the request (see
//     internal/db/queries/runtime_toolkit_call_tool.sql). And it covers only
//     tool runs a person or an MCP client asked for — the toolkit test button
//     and `tools/call` — not the tool calls an AGENT makes inside a turn, which
//     is the bulk of tool usage and the exact exclusion this bullet is about.
//
//     Closing #618 therefore needs a durable per-tool-call record — project,
//     toolkit id, tool name, started/finished, outcome — written by BOTH the
//     agent turn and the explicit run. That is a table, not an event, and no
//     amount of producer-side signalling from this capability substitutes for
//     it.
//
// It is answered with ErrNoSource, which the API layer turns into a FINAL
// status rather than a retryable one. It is a product gap, not a fault.
//
// # What is NO LONGER refused: agent analytics (migration 0100)
//
// The paragraph above used to have a second bullet, and it read: "A gateway
// request knows the model it addressed, not the agent that composed it, and
// nothing correlates the two." That was true of the corpus when it was written
// and it is the exact statement this file is a record of going stale.
//
// The correlation now exists, and it is a CORRELATION rather than a
// denormalisation. gateway.llm_request_logs.execution_id (shared 0100) carries
// the runtime execution a request was made from — signed into the identity
// tuple at the edge under v2, so it cannot be attached by a caller — and the
// agent is resolved from it at READ time. Nothing writes an agent id onto the
// log, deliberately: elitea_runtime.execution_jobs has resource_project_id AND
// projection_project_id and they can differ, so an agent id copied onto a log
// row would have had to pick one of those two project meanings and bake it in.
// That is the exact ambiguity the paragraph at the top of this file says the
// request log exists to sidestep.
//
// WHAT CANNOT BE ANSWERED IS THE PAST. Nothing on a row written before 0100
// identifies an agent, so there is no backfill to write and none is faked. The
// read reports AVAILABILITY (analytics.AgentBreakdown.Available) and omits the
// breakdown for a window it cannot speak for, the way
// usageDimensions.Available already does for a deployment upgraded mid-period.
// It never answers "0 agent runs" for a month full of them.
//
// # The ACCOUNTED total is deliberately not read here
//
// `total_cost` has a producer — gateway.llm_budget_accumulators — and
// /analytics_costs already reports it, with the scope rules that keep it from
// double-counting (a user-scope row is a subset of its project's spend, not an
// addition to it). Reading that table a second way here would be a second
// view of the ACCOUNTED money that could disagree with the first, so it never
// is: the client asks /analytics_costs for the accounted figure and this
// endpoint for volume.
//
// agentUsage's Priced/InputCost/OutputCost/TotalCost fields (issue #875) are a
// DIFFERENT figure and do not break this rule: they price each row at
// gateway.gateway_models' catalogue rate, the exact ESTIMATE
// /analytics_costs' own `estimate` block already derives for by_model and
// by_user, from the same request-log rows this file was already summing for
// tokens. It is a second query of that estimate, the way the Costs tab's
// by_model and the Overview tab's model table already are two queries of
// overlapping request-log rows — never a second query of the accumulator.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/analytics"
)

// analyticsReadTimeout bounds one dashboard read. Every statement below is
// served by 0099's (project_id, occurred_at DESC) index over a clamped window,
// so this is a backstop rather than a budget.
const analyticsReadTimeout = 20 * time.Second

// topUsersLimit caps the overview's leaderboard, and userRowsLimit caps the
// Users tab's full list.
//
// Both are capped because the row count here is the project's CALLER count, not
// its call count — a group-by collapses a million calls to one row per user —
// but a project bound to a large SCIM group is a real thing on this deployment
// and an uncapped list is a way for one request to serialise all of it.
//
// THE CAP IS STATED IN THE RESPONSE. GetUserActivity reports whether it cut the
// list, the handler publishes that as `truncated`, and the Users tab says so on
// screen. A silent cap would be worse here than almost anywhere else in this
// service, because the CLIENT PAGINATES AND SEARCHES OVER WHAT IT RECEIVES:
// apps/elitea-web/src/features/analytics/api/useAnalytics.ts asks for the
// schema maximum once and treats the result as the complete set. Cut silently,
// a 900-caller project would show the top 500 with a "500 users" count and a
// working pagination footer — presented as everyone, with nothing anywhere able
// to tell the difference.
//
// The leaderboard has no such flag and needs none: it is labelled a top-N list
// on screen, so its cap is the feature rather than a truncation of one.
const (
	topUsersLimit  = 10
	userRowsLimit  = 500
	modelRowsLimit = 100
	// agentRowsLimit caps the Agents tab. Same order as modelRowsLimit and for
	// the same reason: the rows are one per AGENT, not one per run, so a
	// group-by collapses a project's whole history into its agent count. The cut
	// is REPORTED (AgentBreakdown.Truncated) because the client normalises its
	// share column by summing what it received.
	agentRowsLimit = 100
	// agentCostScale is how many decimal places agentUsage's derived money
	// carries — a STRING because it is interpolated into the query text, not
	// bound as a parameter. Matches estimateCostScale
	// (internal/api/v2/analytics/estimate.go): three digits finer than the
	// platform's own int64 nano-USD unit, so a division by 1,000,000 in
	// NUMERIC does not keep growing the scale into a string of trailing zeros.
	agentCostScale = "12"
	// toolRowsLimit caps the Tools tab. Same order as agentRowsLimit and for
	// the same reason: the rows are one per (toolkit, tool), not one per call.
	// The cut is REPORTED (ToolBreakdown.Truncated) because the client
	// normalises its share column by summing what it received.
	toolRowsLimit = 100
	// healthModelRowsLimit caps the health table. Higher than modelRowsLimit
	// because its rows are keyed by (provider, model, STREAMING), so a
	// deployment serving both response kinds produces two rows per model.
	healthModelRowsLimit = 200
	// errorCodeRowsLimit caps the failure breakdown. The gateway assigns error
	// codes from its own finite taxonomy, so this is a backstop against a
	// future taxonomy rather than a real cardinality bound.
	errorCodeRowsLimit = 50
)

// errorPredicate is what counts as a failure, everywhere in this file.
//
// status >= 400, which is the predicate 0099's partial index
// idx_llm_request_logs_errors is built on — so the health reads are served by
// it rather than scanning. Written once because a health table whose totals and
// whose per-model rows disagreed about what an error is would be worse than no
// health table.
const errorPredicate = `status >= 400`

// AnalyticsRepo answers the analytics reads from the gateway's request log.
type AnalyticsRepo struct {
	pool *pgxpool.Pool
}

func NewAnalyticsRepo(pool *pgxpool.Pool) *AnalyticsRepo {
	return &AnalyticsRepo{pool: pool}
}

// requestLogWindow is the row set every query below reads, written once so no
// two of them can describe different rows.
//
// Half-open at the top: a window ending at midnight must not also claim the
// first instant of the next day, or two adjacent windows both count it.
const requestLogWindow = `
WHERE project_id = $1
  AND occurred_at >= $2
  AND occurred_at < $3`

// missingRelation reports whether err is PostgreSQL's undefined_table (42P01)
// or undefined_schema (3F000).
//
// It exists so an absent table becomes a NAMED absence rather than a generic
// 500. A deployment that has not run shared migration 0099 has no
// gateway.llm_request_logs, and "this deployment does not have the request log"
// is a true, actionable sentence; "failed to query analytics" is not.
func missingRelation(err error) bool {
	var pgErr interface{ SQLState() string }
	if !errors.As(err, &pgErr) {
		return false
	}
	switch pgErr.SQLState() {
	case "42P01", "3F000":
		return true
	default:
		return false
	}
}

// projectID parses the path segment once, so a malformed id fails as a bad
// REQUEST rather than as a database error — the API layer maps ErrBadProject to
// 400. It was a plain fmt.Errorf first, which reached writeRepoFailure, matched
// no branch there and answered `500 {"code":"query_failed"}`: the server taking
// the blame for a value the caller sent, and inviting a retry that cannot
// succeed. /analytics_costs answers 400 for the same input.
func projectID(params analytics.QueryParams) (int64, error) {
	id, err := strconv.ParseInt(params.ProjectID, 10, 64)
	if err != nil || id < 1 {
		return 0, analytics.BadProjectError(params.ProjectID)
	}
	return id, nil
}

// GetUsageSummary is the Overview tab: totals, the per-model split, the daily
// series and the leaderboard, all from the request log.
func (r *AnalyticsRepo) GetUsageSummary(ctx context.Context, params analytics.QueryParams) (analytics.UsageSummary, error) {
	id, err := projectID(params)
	if err != nil {
		return analytics.UsageSummary{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, analyticsReadTimeout)
	defer cancel()

	// ONE snapshot for all four statements, for the reason /analytics_costs
	// opens one: the gateway commits into this table continuously, so without
	// it the totals could be summed over rows the daily series never saw, and a
	// dashboard whose header disagrees with its own chart is worse than a stale
	// one. Read-only REPEATABLE READ cannot raise a serialization failure, so
	// this buys a snapshot and no retry path.
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return analytics.UsageSummary{}, fmt.Errorf("analytics: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	summary := analytics.UsageSummary{ProjectID: params.ProjectID, Period: params.Period}

	const totalsQuery = `
SELECT count(*)::bigint,
       coalesce(sum(prompt_tokens + completion_tokens), 0)::bigint,
       count(DISTINCT user_id)::bigint
FROM gateway.llm_request_logs` + requestLogWindow

	if err := tx.QueryRow(ctx, totalsQuery, id, params.From, params.To).
		Scan(&summary.TotalRuns, &summary.TotalTokens, &summary.ActiveUsers); err != nil {
		if missingRelation(err) {
			return analytics.UsageSummary{}, analytics.NoSourceError("usage summary",
				"gateway.llm_request_logs is absent — shared migration 0099 has not run on this database")
		}
		return analytics.UsageSummary{}, fmt.Errorf("analytics: usage totals: %w", err)
	}

	if summary.ByModel, summary.ModelsTruncated, err = modelUsage(ctx, tx, id, params); err != nil {
		return analytics.UsageSummary{}, err
	}
	if summary.DailyActivity, err = dailyActivity(ctx, tx, id, params); err != nil {
		return analytics.UsageSummary{}, err
	}
	if summary.TopUsers, err = userActivity(ctx, tx, id, params, topUsersLimit); err != nil {
		return analytics.UsageSummary{}, err
	}
	health, err := projectHealth(ctx, tx, id, params)
	if err != nil {
		return analytics.UsageSummary{}, err
	}
	summary.Health = health

	members, activeMembers, ok, err := projectAdoption(ctx, tx, id, params)
	if err != nil {
		return analytics.UsageSummary{}, err
	}
	if ok {
		summary.TotalProjectUsers = &members
		summary.ActiveMembers = &activeMembers
	}

	return summary, nil
}

// GetUserActivity is the Users tab: every caller who made a request in the
// window, and whether that list had to be cut.
func (r *AnalyticsRepo) GetUserActivity(ctx context.Context, params analytics.QueryParams) ([]analytics.UserActivity, bool, error) {
	id, err := projectID(params)
	if err != nil {
		return nil, false, err
	}

	ctx, cancel := context.WithTimeout(ctx, analyticsReadTimeout)
	defer cancel()

	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, false, fmt.Errorf("analytics: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// One row OVER the cap is requested, so "there is more" is a fact about the
	// result set rather than a guess from `len(rows) == limit` — which cannot
	// tell a project with exactly 500 callers from one with 5,000.
	users, err := userActivity(ctx, tx, id, params, userRowsLimit+1)
	if err != nil {
		return nil, false, err
	}
	if len(users) > userRowsLimit {
		return users[:userRowsLimit], true, nil
	}
	return users, false, nil
}

/* ── agents ─────────────────────────────────────────────────────────────── */

// agentCapabilities are the execution capabilities that mean "an agent ran".
//
// A fixed list and not a prefix match. elitea_runtime.execution_jobs also holds
// configuration validations and index ingests, which are executions the
// platform made for itself; folding those into an agent breakdown would report
// runs no agent performed. agent_execution_jobs' own CHECK constraint (shared
// 0055) is this same pair, which is what keeps the two lists from drifting into
// disagreement without anything failing.
// toolCallRecordsMigration is the shared migration that created
// elitea_runtime.tool_call_records. It is the moment this deployment began
// recording tool calls, and therefore the boundary of what the Tools tab can
// speak for. The number is pinned here rather than derived so a later
// renumbering at merge cannot silently move the boundary — the manifest head
// test names the same file.
const toolCallRecordsMigration = 119

const agentCapabilities = `('agent.execute.application.v1', 'agent.execute.adhoc.v1')`

// agentExecutionColumn probes for the column migration 0100 adds.
//
// It is a COLUMN probe and not a relation probe, because the relation is
// present on every deployment that ran 0099 and the question here is whether
// the newer file has run. checkRelations cannot answer it: to_regclass reports
// on tables.
//
// It has to be asked BEFORE the statement rather than caught after it, for the
// reason userIdentities spells out at length: GetAgentAnalytics runs on a
// snapshot transaction, and in PostgreSQL a failed statement poisons the whole
// transaction, so a tolerated 42703 would take every later read with it.
const agentExecutionColumn = `
SELECT EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_schema = 'gateway'
      AND table_name = 'llm_request_logs'
      AND column_name = 'execution_id'
)`

// GetAgentAnalytics is the Agents tab: which agents ran in this window, how
// often, how slowly, and how much of it failed.
//
// # How a request becomes an agent
//
//	gateway.llm_request_logs.execution_id   the runtime execution, signed at the
//	                                        edge, written by the gateway
//	  -> elitea_runtime.execution_jobs      the GUARD: is this execution real,
//	                                        is it an AGENT execution, and does it
//	                                        belong to this project
//	  -> p_<id>.chat_message_group.task_id  the turn that execution produced
//	  -> chat_participants.entity_meta      the agent that authored the turn
//	  -> p_<id>.applications.name           its display name
//
// execution_jobs is a guard and NOT the project source. The window is scoped by
// llm_request_logs.project_id, which is one column with one meaning;
// execution_jobs has resource_project_id AND projection_project_id and they can
// differ, so asking it "which project is this" has no single answer. Asking it
// "does either of your project columns agree with the one the log already
// named" does, and that is all it is asked.
//
// That guard is also what contains a forged execution id. The id is signed into
// the v2 identity tuple so it cannot normally be attached by a caller at all;
// if one ever were, it would resolve only inside the project the LOG row names,
// against an agent that caller can already see. It is never an authorization
// input.
//
// # What it does NOT do
//
// It does not zero-fill and it does not backfill. See AgentBreakdown.Available
// and the file header.
func (r *AnalyticsRepo) GetAgentAnalytics(ctx context.Context, params analytics.QueryParams) (analytics.AgentBreakdown, error) {
	id, err := projectID(params)
	if err != nil {
		return analytics.AgentBreakdown{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, analyticsReadTimeout)
	defer cancel()

	// One snapshot, for the reason GetUsageSummary opens one: the attributed
	// and unattributed counts and the per-agent rows are three views of one row
	// set, and the gateway commits into this table continuously.
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return analytics.AgentBreakdown{}, fmt.Errorf("analytics: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var hasColumn bool
	if err := tx.QueryRow(ctx, agentExecutionColumn).Scan(&hasColumn); err != nil {
		if missingRelation(err) {
			return analytics.AgentBreakdown{}, analytics.NoSourceError("agent analytics",
				"gateway.llm_request_logs is absent — shared migration 0099 has not run on this database")
		}
		return analytics.AgentBreakdown{}, fmt.Errorf("analytics: agent column probe: %w", err)
	}
	if !hasColumn {
		// A NAMED absence, not an empty table. The endpoint refuses rather than
		// answering with a breakdown it cannot build, because a 200 with no
		// agents is indistinguishable from a project whose agents never ran.
		return analytics.AgentBreakdown{}, analytics.NoSourceError("agent analytics",
			"gateway.llm_request_logs has no execution_id column — shared migration 0100 has not run on this database")
	}

	attributed, unattributed, err := agentAttribution(ctx, tx, id, params)
	if err != nil {
		return analytics.AgentBreakdown{}, err
	}

	breakdown := analytics.AgentBreakdown{
		AttributedCalls:   attributed,
		UnattributedCalls: unattributed,
	}
	if attributed == 0 {
		// NOT AVAILABLE, and not "zero agents".
		//
		// This is the pre-migration window, and it is also the window of a
		// deployment whose runtime is not tagging its calls. Both are "we have
		// nothing to say about agents here", which is a different sentence from
		// "no agent ran here" — and the second one is a measurement, so it is
		// not made. Agents stays nil and the handler omits the list entirely.
		return breakdown, nil
	}
	breakdown.Available = true

	agents, truncated, err := agentUsage(ctx, tx, id, params)
	if err != nil {
		return analytics.AgentBreakdown{}, err
	}
	// Present-and-possibly-empty once the window HAS attributable traffic: at
	// that point "no row resolved to a named agent" is a measured fact about
	// ad-hoc runs and deleted conversations, not an absence of data.
	if agents == nil {
		agents = []analytics.AgentAnalytics{}
	}
	breakdown.Agents = agents
	breakdown.Truncated = truncated
	return breakdown, nil
}

// agentAttribution splits the window into requests that carry a usable
// execution id and requests that do not.
//
// BOTH HALVES ARE REPORTED. The per-agent rows are not a partition of the
// project's llm_calls tile and never will be — most /llm traffic is not made
// from a runtime execution — so publishing only the attributed side would leave
// an operator reconciling a breakdown against a total it was never part of.
func agentAttribution(ctx context.Context, q analyticsQuerier, id int64, params analytics.QueryParams) (attributed, unattributed int64, err error) {
	query := `
SELECT count(*) FILTER (WHERE l.execution_id IS NOT NULL AND EXISTS (
           SELECT 1 FROM elitea_runtime.execution_jobs AS j
           WHERE j.execution_id = l.execution_id
             AND j.capability_id IN ` + agentCapabilities + `
             AND (j.resource_project_id = l.project_id
                  OR j.projection_project_id = l.project_id)))::bigint,
       count(*)::bigint
FROM gateway.llm_request_logs AS l
WHERE l.project_id = $1
  AND l.occurred_at >= $2
  AND l.occurred_at < $3`

	var total int64
	if err := q.QueryRow(ctx, query, id, params.From, params.To).Scan(&attributed, &total); err != nil {
		if missingRelation(err) {
			return 0, 0, analytics.NoSourceError("agent analytics",
				"gateway.llm_request_logs or elitea_runtime.execution_jobs is absent on this database")
		}
		return 0, 0, fmt.Errorf("analytics: agent attribution: %w", err)
	}
	return attributed, total - attributed, nil
}

// agentUsage groups the attributable requests by the agent that made them.
//
// # Why the tenant schema is interpolated
//
// The chat projection lives in p_<project_id>, and a schema name cannot be a
// bind parameter. The id is not caller text: projectID() has already parsed it
// as a positive integer and pgx.Identifier.Sanitize quotes the result. This is
// the same construction internal/infra/db/repos/index_activity.go uses.
//
// # A MISS IS NOT A FAILURE
//
// p_<id>.applications is pylon-owned and a Go-bootstrapped database
// legitimately has none, exactly as userIdentities documents for
// public.auth_core__user. The relations are probed BEFORE the statement runs,
// never caught after: this read is on a snapshot transaction, and a failed
// statement poisons the whole transaction rather than just itself.
//
// When the chat projection is absent the breakdown is EMPTY rather than
// refused: the window's attributed count is still a true measurement, and the
// caller has already published it.
func agentUsage(ctx context.Context, q analyticsQuerier, id int64, params analytics.QueryParams) ([]analytics.AgentAnalytics, bool, error) {
	schema := pgx.Identifier{"p_" + strconv.FormatInt(id, 10)}.Sanitize()

	// The chat projection is REQUIRED — without it there is no execution-to-agent
	// mapping at all — and the name is not.
	//
	// They are probed separately because they belong to different corpora.
	// chat_message_group and chat_participants are created by this service's
	// tenant history (0123), so a migrated database always has them.
	// `applications` is PYLON-OWNED, and a Go-bootstrapped database
	// legitimately has none — the same split userIdentities documents for
	// public.auth_core__user.
	//
	// So an absent applications table costs the DISPLAY NAME and nothing else.
	// Dropping the rows instead would silently shrink the breakdown, and
	// refusing the whole read because a decoration is unavailable is the
	// "absence reads as failure" defect this repository has met before.
	present, err := checkRelations(ctx, q, schema+".chat_message_group", schema+".chat_participants")
	if err != nil {
		return nil, false, err
	}
	if !present {
		return nil, false, nil
	}
	named, err := checkRelations(ctx, q, schema+".applications")
	if err != nil {
		return nil, false, err
	}
	// A fixed fragment chosen from two constants, never assembled from request
	// data — the shape usageScopeFilter uses for the same reason.
	// nameGroup is separate from nameSelect because a literal cannot appear in
	// GROUP BY — PostgreSQL reads a bare constant there as an ordinal and
	// refuses a string outright (42601). The unnamed form groups on the id
	// alone, which is the same grouping: one constant cannot split a group.
	nameSelect, nameJoin, nameGroup := `''`, ``, ``
	if named {
		nameSelect = `coalesce(app.name, '')`
		nameJoin = `
LEFT JOIN ` + schema + `.applications AS app
       ON app.id = agent.application_id::integer`
		nameGroup = `, coalesce(app.name, '')`
	}

	// attributed: one row per EXECUTION, so the per-agent fold below cannot be
	// distorted by an execution that has several generations in execution_jobs.
	// EXISTS rather than a JOIN for exactly that reason — execution_jobs is
	// keyed (execution_id, generation), and a join on the id alone multiplies
	// every request by the number of retries the turn had.
	//
	// The LEFT JOIN against gateway.gateway_models (issue #875) prices EACH
	// ROW at ITS OWN model's rate before the per-execution sum, the way
	// estimate.go's by_model and by_user reads do — an agent's calls can
	// address several different models, so pricing has to happen before the
	// aggregate, not after. A row the catalogue does not price joins to NULL
	// rates; sum() skips a NULL term, so an unpriced call adds nothing to the
	// money and everything to the tokens agentUsage was already summing.
	//
	// agent: DISTINCT ON (task_id) because a projection could hold more than
	// one group against an execution; the earliest is the response the
	// execution was admitted for.
	query := `
WITH attributed AS (
    SELECT l.execution_id,
           count(*)::bigint AS requests,
           coalesce(sum(l.prompt_tokens + l.completion_tokens), 0)::bigint AS tokens,
           coalesce(sum(l.duration_ms), 0)::bigint AS duration_ms,
           count(*) FILTER (WHERE ` + errorPredicate + `)::bigint AS errors,
           bool_or(m.input_cost_per_1m_tokens IS NOT NULL
                    OR m.output_cost_per_1m_tokens IS NOT NULL) AS priced,
           coalesce(sum(l.prompt_tokens::numeric * m.input_cost_per_1m_tokens) / 1000000, 0) AS input_cost,
           coalesce(sum(l.completion_tokens::numeric * m.output_cost_per_1m_tokens) / 1000000, 0) AS output_cost
    FROM gateway.llm_request_logs AS l
    LEFT JOIN gateway.gateway_models AS m
      ON m.provider = l.provider AND m.model_name = l.model
    WHERE l.project_id = $1
      AND l.occurred_at >= $2
      AND l.occurred_at < $3
      AND l.execution_id IS NOT NULL
      AND EXISTS (
          SELECT 1 FROM elitea_runtime.execution_jobs AS j
          WHERE j.execution_id = l.execution_id
            AND j.capability_id IN ` + agentCapabilities + `
            AND (j.resource_project_id = l.project_id
                 OR j.projection_project_id = l.project_id)
      )
    GROUP BY l.execution_id
), agent AS (
    SELECT DISTINCT ON (g.task_id)
           g.task_id AS execution_id,
           (author.entity_meta ->> 'id') AS application_id
    FROM ` + schema + `.chat_message_group AS g
    JOIN ` + schema + `.chat_participants AS author
      ON author.id = g.author_participant_id
     AND author.entity_name = 'application'
    WHERE g.task_id IN (SELECT execution_id FROM attributed)
      AND author.entity_meta ->> 'id' ~ '^[1-9][0-9]*$'
    ORDER BY g.task_id, g.id
)
SELECT agent.application_id,
       ` + nameSelect + `,
       sum(attributed.requests)::bigint,
       sum(attributed.tokens)::bigint,
       sum(attributed.duration_ms)::bigint,
       sum(attributed.errors)::bigint,
       bool_or(attributed.priced),
       round(sum(attributed.input_cost), ` + agentCostScale + `)::text,
       round(sum(attributed.output_cost), ` + agentCostScale + `)::text,
       round(sum(attributed.input_cost) + sum(attributed.output_cost), ` + agentCostScale + `)::text
FROM agent
JOIN attributed ON attributed.execution_id = agent.execution_id` + nameJoin + `
GROUP BY agent.application_id` + nameGroup + `
ORDER BY sum(attributed.requests) DESC, agent.application_id ASC
LIMIT $4`

	rows, err := q.Query(ctx, query, id, params.From, params.To, agentRowsLimit+1)
	if err != nil {
		if missingRelation(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("analytics: agent usage: %w", err)
	}
	defer rows.Close()

	agents := make([]analytics.AgentAnalytics, 0)
	for rows.Next() {
		var (
			agent                      analytics.AgentAnalytics
			durationMS                 int64
			errorCount                 int64
			inCost, outCost, totalCost string
		)
		if err := rows.Scan(&agent.ApplicationID, &agent.Name, &agent.RunCount,
			&agent.TotalTokens, &durationMS, &errorCount,
			&agent.Priced, &inCost, &outCost, &totalCost); err != nil {
			return nil, false, fmt.Errorf("analytics: agent usage scan: %w", err)
		}
		if agent.Priced {
			in, out, total := json.Number(inCost), json.Number(outCost), json.Number(totalCost)
			agent.InputCost, agent.OutputCost, agent.TotalCost = &in, &out, &total
		}
		// Guarded rather than assumed non-zero. A group-by cannot produce a row
		// with no requests today, but a division that only works because of an
		// invariant elsewhere is the kind that starts returning NaN into a JSON
		// body the day the invariant moves.
		if agent.RunCount > 0 {
			agent.AvgDuration = math.Round(float64(durationMS)/float64(agent.RunCount)*10) / 10
			agent.ErrorRate = math.Round(float64(errorCount)/float64(agent.RunCount)*1000) / 10
		}
		agents = append(agents, agent)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(agents) > agentRowsLimit {
		return agents[:agentRowsLimit], true, nil
	}
	return agents, false, nil
}

/* ── tools ─────────────────────────────────────────────────────── */

// GetToolAnalytics is the Tools tab. It used to answer ErrNoSource, and the
// header of this file records the exact statement that stopped being true:
// there was no durable per-tool-call record, only two half-producers that each
// under-report by an unknown factor.
//
// Shared migration 0119 is that record, and BOTH halves write it — the explicit
// tool run (toolkit.call_tool.v1: the test button and MCP tools/call) and the
// agent turn's tool-call trace step, which is the bulk of tool usage and the
// exact exclusion the old refusal named. So this read is one statement over one
// table with one project column and one clock, and it needs neither a tenant
// schema loop nor a join across two time types.
//
// AVAILABILITY IS A PROPERTY OF THE WINDOW, not of the table. Nothing written
// before 0119 identifies a tool call, so a window that ENDS before the
// migration was applied is a window this deployment cannot speak for, and it is
// reported unavailable rather than as an empty list. Once the producer was
// running, zero rows is a measurement: no tool ran.
func (r *AnalyticsRepo) GetToolAnalytics(ctx context.Context, params analytics.QueryParams) (analytics.ToolBreakdown, error) {
	id, err := projectID(params)
	if err != nil {
		return analytics.ToolBreakdown{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, analyticsReadTimeout)
	defer cancel()

	// One snapshot, for the reason GetAgentAnalytics opens one: the
	// availability probe and the rows are two views of one table that both
	// producers commit into continuously, and a breakdown read after a
	// separately-read flag can disagree with it.
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return analytics.ToolBreakdown{}, fmt.Errorf("analytics: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	present, err := checkRelations(ctx, tx, "elitea_runtime.tool_call_records")
	if err != nil {
		return analytics.ToolBreakdown{}, err
	}
	if !present {
		// A NAMED absence. The endpoint refuses rather than answering with an
		// empty breakdown, because a 200 with no tools is indistinguishable
		// from a project whose tools never ran.
		return analytics.ToolBreakdown{}, analytics.NoSourceError("tool analytics",
			"elitea_runtime.tool_call_records is absent — shared migration 0119 has not run on this database")
	}

	recording, err := toolRecordingSince(ctx, tx)
	if err != nil {
		return analytics.ToolBreakdown{}, err
	}
	breakdown := analytics.ToolBreakdown{}
	if recording.IsZero() || !params.To.After(recording) {
		// NOT AVAILABLE, and not "no tool ran".
		//
		// The ledger has no row for 0119 (recording is zero) or the whole
		// window closed before the producer started. Both are "we have nothing
		// to say about tools here". Tools stays nil and the handler omits the
		// list entirely.
		return breakdown, nil
	}
	breakdown.Available = true

	tools, truncated, err := toolUsage(ctx, tx, id, params)
	if err != nil {
		return analytics.ToolBreakdown{}, err
	}
	// Present-and-possibly-empty once the producer was running for the window:
	// at that point "no tool ran" is a measured fact.
	if tools == nil {
		tools = []analytics.ToolAnalytics{}
	}
	breakdown.Tools = tools
	breakdown.Truncated = truncated
	return breakdown, nil
}

// toolRecordingSince reports the moment this database began recording tool
// calls: the applied_at of shared migration 0119.
//
// The LEDGER is the source, not the earliest row in the table. A project with
// no tool calls yet has no earliest row, and reading "no rows" as "the producer
// is not running" would make an idle project indistinguishable from an
// un-migrated one — the availability flag would then be a statement about
// traffic rather than about the deployment.
func toolRecordingSince(ctx context.Context, q analyticsQuerier) (time.Time, error) {
	var appliedAt *time.Time
	if err := q.QueryRow(ctx, `
SELECT min(applied_at)
FROM elitea_runtime.schema_migrations
WHERE target_kind = 'shared' AND version = $1`, toolCallRecordsMigration).Scan(&appliedAt); err != nil {
		if missingRelation(err) {
			return time.Time{}, nil
		}
		return time.Time{}, fmt.Errorf("analytics: tool producer probe: %w", err)
	}
	if appliedAt == nil {
		return time.Time{}, nil
	}
	return *appliedAt, nil
}

// toolUsage folds the window into one row per tool.
//
// GROUPED BY (toolkit_id, toolkit_name, tool_name) rather than by tool_name
// alone. Two toolkits can expose a tool of the same name, and collapsing them
// would publish one row whose duration and error rate belong to two different
// integrations. The toolkit id is NULL on an agent-turn row, so the grouping
// keeps the name beside it: `coalesce` on the id would fold every unidentified
// toolkit into one bucket, which is the invented fact this table exists to
// avoid.
//
// duration is derived from the two timestamps and averaged over the calls that
// FINISHED. A call still running has no duration, and counting it as zero would
// pull the average toward zero exactly when a tool has started hanging.
func toolUsage(ctx context.Context, q analyticsQuerier, id int64, params analytics.QueryParams) ([]analytics.ToolAnalytics, bool, error) {
	rows, err := q.Query(ctx, `
SELECT coalesce(r.toolkit_id::text, ''),
       coalesce(r.toolkit_name, ''),
       r.tool_name,
       count(*)::bigint,
       count(*) FILTER (WHERE r.is_error)::bigint,
       coalesce(avg(EXTRACT(EPOCH FROM (r.finished_at - r.started_at)) * 1000)
                FILTER (WHERE r.finished_at IS NOT NULL), 0)::double precision
FROM elitea_runtime.tool_call_records AS r
WHERE r.project_id = $1
  AND r.started_at >= $2
  AND r.started_at < $3
GROUP BY r.toolkit_id, r.toolkit_name, r.tool_name
ORDER BY count(*) DESC, r.tool_name ASC
LIMIT $4`, id, params.From, params.To, toolRowsLimit+1)
	if err != nil {
		return nil, false, fmt.Errorf("analytics: tool usage: %w", err)
	}
	defer rows.Close()

	tools := make([]analytics.ToolAnalytics, 0)
	for rows.Next() {
		var tool analytics.ToolAnalytics
		if err := rows.Scan(&tool.ToolkitID, &tool.ToolkitName, &tool.ToolName,
			&tool.RunCount, &tool.ErrorCount, &tool.AvgDuration); err != nil {
			return nil, false, fmt.Errorf("analytics: tool usage scan: %w", err)
		}
		tool.AvgDuration = math.Round(tool.AvgDuration*10) / 10
		// Guarded rather than assumed non-zero, for the reason agentUsage
		// guards its own division.
		if tool.RunCount > 0 {
			tool.ErrorRate = math.Round(float64(tool.ErrorCount)/float64(tool.RunCount)*1000) / 10
		}
		tools = append(tools, tool)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(tools) > toolRowsLimit {
		return tools[:toolRowsLimit], true, nil
	}
	return tools, false, nil
}

// analyticsQuerier is the read seam every helper takes, so they run inside
// whichever snapshot the caller opened. Named for this file because the package
// already has a narrower `querier` (applications.go) that carries only QueryRow.
type analyticsQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// modelUsage splits the window by (provider, model).
//
// Rows with an empty model are EXCLUDED. An empty model means the request never
// got far enough to resolve one — a 404, an auth refusal, a malformed body —
// and those are real traffic but they are not a model's usage; folding them
// into a nameless row would put a blank bar on the chart that no operator can
// act on. They are still in the totals, which is where "we served N requests"
// belongs.
//
// The cap is REPORTED. The client sums this array to normalise its share
// column, so a silent cut makes every share a percentage of the busiest N
// rather than of the project — shares adding to 100% over a subset, beside a
// `llm_calls` tile carrying the true total, with nothing on screen explaining
// the disagreement.
func modelUsage(ctx context.Context, q analyticsQuerier, id int64, params analytics.QueryParams) ([]analytics.ModelUsage, bool, error) {
	const query = `
SELECT model,
       provider,
       coalesce(sum(prompt_tokens), 0)::bigint,
       coalesce(sum(completion_tokens), 0)::bigint,
       count(*)::bigint
FROM gateway.llm_request_logs` + requestLogWindow + `
  AND model <> ''
GROUP BY model, provider
ORDER BY count(*) DESC, model ASC
LIMIT $4`

	rows, err := q.Query(ctx, query, id, params.From, params.To, modelRowsLimit+1)
	if err != nil {
		return nil, false, fmt.Errorf("analytics: model usage: %w", err)
	}
	defer rows.Close()

	models := make([]analytics.ModelUsage, 0)
	for rows.Next() {
		var m analytics.ModelUsage
		if err := rows.Scan(&m.Model, &m.Provider, &m.PromptTokens, &m.CompletionTokens, &m.RunCount); err != nil {
			return nil, false, fmt.Errorf("analytics: model usage scan: %w", err)
		}
		models = append(models, m)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(models) > modelRowsLimit {
		return models[:modelRowsLimit], true, nil
	}
	return models, false, nil
}

// dailyActivity is one point per UTC day that had traffic.
//
// The bucket is `AT TIME ZONE 'UTC'` rather than the session's TimeZone, which
// is whatever the connection happened to inherit. Without the cast, the same
// window bucketed on a server set to a different zone yields a different first
// and last day, and the chart shifts by one column with nothing to show for it.
func dailyActivity(ctx context.Context, q analyticsQuerier, id int64, params analytics.QueryParams) ([]analytics.DailyPoint, error) {
	const query = `
SELECT to_char(date_trunc('day', occurred_at AT TIME ZONE 'UTC'), 'YYYY-MM-DD'),
       count(*)::bigint,
       coalesce(sum(prompt_tokens + completion_tokens), 0)::bigint,
       count(DISTINCT user_id)::bigint
FROM gateway.llm_request_logs` + requestLogWindow + `
GROUP BY 1
ORDER BY 1`

	rows, err := q.Query(ctx, query, id, params.From, params.To)
	if err != nil {
		return nil, fmt.Errorf("analytics: daily activity: %w", err)
	}
	defer rows.Close()

	points := make([]analytics.DailyPoint, 0)
	for rows.Next() {
		var p analytics.DailyPoint
		if err := rows.Scan(&p.Date, &p.LLMCalls, &p.TotalTokens, &p.ActiveUsers); err != nil {
			return nil, fmt.Errorf("analytics: daily activity scan: %w", err)
		}
		points = append(points, p)
	}
	return points, rows.Err()
}

// userActivity groups the window by member, then enriches with identity.
//
// Rows whose user_id is NULL are excluded: 0099 stores NULL for a request that
// resolved no member, and "no member" is not a user to put on a leaderboard.
func userActivity(ctx context.Context, q analyticsQuerier, id int64, params analytics.QueryParams, limit int) ([]analytics.UserActivity, error) {
	const query = `
SELECT user_id,
       count(*)::bigint,
       coalesce(sum(prompt_tokens + completion_tokens), 0)::bigint,
       max(occurred_at)
FROM gateway.llm_request_logs` + requestLogWindow + `
  AND user_id IS NOT NULL
GROUP BY user_id
ORDER BY count(*) DESC, user_id ASC
LIMIT $4`

	rows, err := q.Query(ctx, query, id, params.From, params.To, limit)
	if err != nil {
		if missingRelation(err) {
			return nil, analytics.NoSourceError("user activity",
				"gateway.llm_request_logs is absent — shared migration 0099 has not run on this database")
		}
		return nil, fmt.Errorf("analytics: user activity: %w", err)
	}
	defer rows.Close()

	users := make([]analytics.UserActivity, 0)
	ids := make([]int64, 0)
	for rows.Next() {
		var (
			user   analytics.UserActivity
			userID int64
		)
		if err := rows.Scan(&userID, &user.RunCount, &user.TotalTokens, &user.LastActiveAt); err != nil {
			return nil, fmt.Errorf("analytics: user activity scan: %w", err)
		}
		user.UserID = strconv.FormatInt(userID, 10)
		users = append(users, user)
		ids = append(ids, userID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(users) == 0 {
		return users, nil
	}

	identities, err := userIdentities(ctx, q, ids)
	if err != nil {
		return nil, err
	}
	for i := range users {
		if identity, ok := identities[users[i].UserID]; ok {
			users[i].Email = identity.email
			users[i].Name = identity.name
		}
	}
	return users, nil
}

type userIdentity struct {
	email string
	name  string
}

// userIdentities resolves display identity for the ids on the leaderboard.
//
// A MISS IS NOT A FAILURE. `public.auth_core__user` is owned by a different
// corpus — 0060 guards on `to_regclass` before touching it for the same reason
// — so a Go-bootstrapped database legitimately has no such table. If the join
// cannot run, every row keeps its numbers and loses only its email. Failing the
// whole read because a decoration is unavailable is the "absence reads as
// failure" defect, and it is why this is a second statement rather than a LEFT
// JOIN on the query above.
//
// THE MISS MUST BE DETECTED BEFORE THE STATEMENT RUNS, NOT AFTER IT FAILS.
//
// A `WHERE to_regclass(...) IS NOT NULL` predicate inside the query reads like a
// guard and is not one: PostgreSQL resolves the names in the FROM clause while
// PARSING, so a query naming a missing table raises 42P01 before any predicate
// is evaluated. checkRelations CAN ask the question in SQL only because
// to_regclass takes the name as a STRING and nothing in that statement's FROM
// clause has to resolve.
//
// Catching the 42P01 afterwards is not enough either, and this is the part that
// cost a real figure. GetUsageSummary runs every statement in ONE transaction,
// and in PostgreSQL a failed statement aborts the whole transaction: every
// later statement on it answers 25P02 (in_failed_sql_transaction) until it is
// rolled back. So tolerating the error here did not contain it — it poisoned
// the tx, projectMemberCount's own probe then failed, checkRelations reported
// "absent", and a project with a populated membership table reported NO
// denominator and NO adoption rate.
//
// That state is real and not exotic: `public.auth_core__user` and the two role
// tables are created by different corpora, and a database can legitimately have
// the role tables and not the user table. TestMembershipSurvivesAnAbsentUserTable
// pins it; the pre-existing missing-tables test could not, because it dropped
// all three and the denominator would have been absent either way.
//
// The error check below stays as the belt to this braces: the probe and the
// query are two statements and a table could vanish between them.
func userIdentities(ctx context.Context, q analyticsQuerier, ids []int64) (map[string]userIdentity, error) {
	const query = `
SELECT u.id, coalesce(u.email, ''), coalesce(u.name, '')
FROM public.auth_core__user AS u
WHERE u.id = ANY($1)`

	present, err := checkRelations(ctx, q, "public.auth_core__user")
	if err != nil {
		return nil, err
	}
	if !present {
		return map[string]userIdentity{}, nil
	}

	rows, err := q.Query(ctx, query, ids)
	if err != nil {
		if missingRelation(err) {
			return map[string]userIdentity{}, nil
		}
		return nil, fmt.Errorf("analytics: user identities: %w", err)
	}
	defer rows.Close()

	identities := make(map[string]userIdentity, len(ids))
	for rows.Next() {
		var (
			id       int64
			identity userIdentity
		)
		if err := rows.Scan(&id, &identity.email, &identity.name); err != nil {
			return nil, fmt.Errorf("analytics: user identities scan: %w", err)
		}
		identities[strconv.FormatInt(id, 10)] = identity
	}
	return identities, rows.Err()
}

// projectAdoption measures BOTH sides of the adoption rate in one statement,
// and reports whether it could be measured at all.
//
// # WHY BOTH SIDES, AND NOT JUST THE DENOMINATOR
//
// The obvious shape — count the members, divide the request log's distinct
// callers by it — produces rates above 100% as a matter of course, because the
// two figures are drawn from unrelated sets. `count(DISTINCT user_id)` over the
// request log counts everyone who called the gateway under this project:
// a member who has since been removed, a global administrator, a service token.
// None of them is in the denominator. Measured on a real schema, one member and
// three non-member callers reported `adoption_rate: 300`, `ai_active_users: 3`,
// `total_project_users: 1` — and the tile rendered "3 of 1, ↑300% adoption".
//
// That is the same defect as dividing by an invented zero, in the other
// direction: a percentage of two different populations. So the numerator here
// is the INTERSECTION — members who called — and both sides come from one
// statement over one snapshot, which is also the only way they cannot disagree.
//
// The unintersected caller count is still reported, as ai_active_users. It is a
// true and useful figure ("this many people used AI here"); it is just not the
// numerator of an adoption rate.
//
// DISTINCT on the member count, because membership is expressed as role grants
// and one member can hold several roles in the same project — counting grants
// would inflate the denominator and understate adoption.
//
// # WHICH TABLE HOLDS MEMBERSHIP
//
// public.auth_core__project_user_role, and only that one. The earlier form of
// this query joined public.auth_core__user_role to public.auth_core__project_role
// on `pr.id = ur.role_id`, and those two ids name DIFFERENT things:
// auth_core__user_role.role_id references auth_core__role, the CENTRAL role
// table, which has no project column at all. The join therefore matched a user
// whose central role id happened to equal a project role id of this project,
// which is an accident of two SERIAL sequences.
//
// Measured on a fresh install: the owner of a personal project holds one
// central grant, its id collides with a role of the FIRST project, and the
// project's own member row is invisible to the query. The tile read
// "AI ACTIVE 1 of 0 members" — one caller, no members, on a project whose
// single member is the caller. The Users page has always read the right table
// (eliteacore/handler.go's usersPageQuery); this figure did not.
//
// Project provisioning writes the owner's row into auth_core__project_user_role
// (application/projectprovisioning/steps.go, createOwnerMembership), so a
// personal project now measures 1 member, which is the truth.
//
// Guarded like userIdentities, and for the same reason: this table belongs to
// a different corpus. When it is absent the caller omits total_project_users
// and adoption_rate entirely rather than reporting a rate over a denominator it
// invented.
func projectAdoption(ctx context.Context, q analyticsQuerier, id int64, params analytics.QueryParams) (members, active int64, ok bool, err error) {
	// The two variants differ only in the SYSTEM-USER filter.
	//
	// Every project provisions a system account of its own and grants it a
	// membership row (application/projectprovisioning/steps.go,
	// createSystemUser). It is not a person, and the Users page hides it with
	// exactly this predicate (eliteacore/handler.go, usersPageQuery). Counting
	// it would put one member on this tile that the page beside it does not
	// list — on a personal project, "1 of 2" where the truth is "1 of 1".
	//
	// The filter needs public.auth_core__user, and that table can be absent
	// while the membership table is present. Losing the whole denominator over
	// a display column would be the worse answer, so absence drops the FILTER
	// and keeps the figure.
	const filteredMembers = `
WITH project_members AS (
    SELECT DISTINCT pur.user_id
    FROM public.auth_core__project_user_role AS pur
    JOIN public.auth_core__user AS account ON account.id = pur.user_id
    WHERE pur.project_id = $1
      AND account.email NOT LIKE '%@centry.user'
)`
	const unfilteredMembers = `
WITH project_members AS (
    SELECT DISTINCT pur.user_id
    FROM public.auth_core__project_user_role AS pur
    WHERE pur.project_id = $1
)`
	const tail = `
SELECT (SELECT count(*)::bigint FROM project_members),
       (SELECT count(DISTINCT l.user_id)::bigint
        FROM gateway.llm_request_logs AS l
        WHERE l.project_id = $1
          AND l.occurred_at >= $2
          AND l.occurred_at < $3
          AND l.user_id IN (SELECT user_id FROM project_members))`

	// Asked BEFORE the statement below, for the reason userIdentities gives at
	// length: the query names the table in its FROM clause, so a missing one
	// raises 42P01 at parse time, no in-query predicate can prevent it, and the
	// resulting error would abort the shared transaction rather than being
	// contained. The missingRelation check after it is the belt to this braces
	// — the two statements are not atomic.
	present, err := checkRelations(ctx, q, "public.auth_core__project_user_role")
	if err != nil {
		return 0, 0, false, err
	}
	if !present {
		return 0, 0, false, nil
	}
	accounts, err := checkRelations(ctx, q, "public.auth_core__user")
	if err != nil {
		return 0, 0, false, err
	}
	query := unfilteredMembers + tail
	if accounts {
		query = filteredMembers + tail
	}

	if err := q.QueryRow(ctx, query, id, params.From, params.To).Scan(&members, &active); err != nil {
		if missingRelation(err) {
			return 0, 0, false, nil
		}
		return 0, 0, false, fmt.Errorf("analytics: project adoption: %w", err)
	}
	return members, active, true, nil
}

// checkRelations reports whether every named relation exists, in one round
// trip. `to_regclass` returns NULL rather than raising for a missing name,
// which is what makes this askable at all.
//
// IT RETURNS THE ERROR. Swallowing it and answering `false` made a transient
// database failure — an expired deadline, a reset connection — indistinguishable
// from a table that genuinely is not there, and the callers turn "not there"
// into a feature this deployment does not have. The Users tab would then answer
// 200 with every email blank, rendering "User 41" for each row with nothing
// anywhere reporting a fault and nothing prompting a retry.
//
// That is this endpoint's own defect inverted. It answers 501 rather than 500
// for an absent producer so that a permanent gap is not read as a fault; the
// same distinction has to hold in the other direction, or a fault is read as a
// permanent gap.
func checkRelations(ctx context.Context, q analyticsQuerier, names ...string) (bool, error) {
	var present bool
	if err := q.QueryRow(ctx,
		`SELECT bool_and(to_regclass(name) IS NOT NULL) FROM unnest($1::text[]) AS name`,
		names).Scan(&present); err != nil {
		return false, fmt.Errorf("analytics: relation probe %v: %w", names, err)
	}
	return present, nil
}

/* ── health ────────────────────────────────────────────────────────────── */

// projectHealth is the Health tab: what failed, how often, and how slowly.
//
// # Why this is answerable here and nowhere else
//
// `gateway.llm_request_logs` is the only table in this platform that records a
// request that FAILED. `gateway.llm_usage_events` is written from a billing
// delta and a billing delta rides only a BILLED request, so a call refused by a
// budget, rejected by a policy, addressed to an unresolvable model or failed
// upstream leaves no trace in it — a health view built over the ledger would
// list successes and no failures.
//
// It runs on the caller's snapshot, so the totals, the breakdown and the trend
// are three views of one row set rather than three reads of a table the gateway
// is committing into continuously.
func projectHealth(ctx context.Context, q analyticsQuerier, id int64, params analytics.QueryParams) (*analytics.Health, error) {
	health := &analytics.Health{
		ByErrorCode: []analytics.ErrorCodeCount{},
		ByModel:     []analytics.ModelHealth{},
		Daily:       []analytics.DailyHealth{},
	}

	const totalsQuery = `
SELECT count(*)::bigint,
       count(*) FILTER (WHERE ` + errorPredicate + `)::bigint
FROM gateway.llm_request_logs` + requestLogWindow

	if err := q.QueryRow(ctx, totalsQuery, id, params.From, params.To).
		Scan(&health.Requests, &health.Errors); err != nil {
		if missingRelation(err) {
			return nil, analytics.NoSourceError("health",
				"gateway.llm_request_logs is absent — shared migration 0099 has not run on this database")
		}
		return nil, fmt.Errorf("analytics: health totals: %w", err)
	}
	health.ErrorRate = ratePercent(health.Errors, health.Requests)

	var err error
	if health.ByErrorCode, health.ErrorCodesTruncated, err = healthByErrorCode(ctx, q, id, params); err != nil {
		return nil, err
	}
	if health.ByModel, err = healthByModel(ctx, q, id, params); err != nil {
		return nil, err
	}
	if health.Daily, err = healthDaily(ctx, q, id, params); err != nil {
		return nil, err
	}
	return health, nil
}

// ratePercent is the one place a count becomes a percentage, to one decimal.
//
// A zero denominator yields 0 rather than NaN: a window with no requests had
// nothing fail in it, and NaN would serialise to a JSON the client cannot read.
func ratePercent(part, whole int64) float64 {
	if whole <= 0 {
		return 0
	}
	return math.Round(float64(part)/float64(whole)*1000) / 10
}

// healthByErrorCode is the failure breakdown.
//
// FAILURES ONLY, and never an upstream string. `error_code` is a classification
// the gateway assigns from its own taxonomy; 0099 has no column a provider's
// error text could reach, because upstream errors routinely quote the offending
// fragment of the request back and a request is user-authored free text.
//
// A failed request with an EMPTY error_code is reported under a placeholder
// rather than dropped: it means the gateway returned a 4xx/5xx without
// classifying it, which is itself worth seeing, and dropping those rows would
// make the breakdown's total disagree with the headline error count.
//
// The cap is REPORTED, for the reason the users list reports its own: the
// breakdown sits beside an uncapped `errors` headline, and an operator
// reconciling the two against a silently cut list finds failures belonging to
// no listed classification with nothing saying why. One row over the cap is
// requested so "there is more" is a fact rather than a guess from
// `len(rows) == limit`.
func healthByErrorCode(ctx context.Context, q analyticsQuerier, id int64, params analytics.QueryParams) ([]analytics.ErrorCodeCount, bool, error) {
	const query = `
SELECT CASE WHEN error_code = '' THEN 'unclassified' ELSE error_code END,
       count(*)::bigint
FROM gateway.llm_request_logs` + requestLogWindow + `
  AND ` + errorPredicate + `
GROUP BY 1
ORDER BY count(*) DESC, 1 ASC
LIMIT $4`

	rows, err := q.Query(ctx, query, id, params.From, params.To, errorCodeRowsLimit+1)
	if err != nil {
		return nil, false, fmt.Errorf("analytics: health by error code: %w", err)
	}
	defer rows.Close()

	codes := make([]analytics.ErrorCodeCount, 0)
	for rows.Next() {
		var code analytics.ErrorCodeCount
		if err := rows.Scan(&code.ErrorCode, &code.Requests); err != nil {
			return nil, false, fmt.Errorf("analytics: health by error code scan: %w", err)
		}
		codes = append(codes, code)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(codes) > errorCodeRowsLimit {
		return codes[:errorCodeRowsLimit], true, nil
	}
	return codes, false, nil
}

// healthByModel is reliability and latency per (provider, model, streaming).
//
// STREAMING IS IN THE GROUP BY. 0099's header states that a streamed and a
// buffered request of the same model have very different latency profiles and
// that averaging them makes both unreadable — a streamed duration is the whole
// stream, seconds where a buffered call is milliseconds. One row per model
// would report a number describing neither, and it would move whenever the mix
// did rather than when the service did.
//
// P95 as well as the mean, because the mean is what hides the tail an operator
// investigating "chat feels slow" came to look at. `percentile_cont` rather
// than `percentile_disc`: it interpolates, so a group of two requests reports a
// p95 between them instead of jumping to the slower one.
//
// Rows with no resolved model are excluded for the reason modelUsage gives —
// they are real traffic but not a model's, and a nameless row is not something
// an operator can act on. The headline totals still count them.
func healthByModel(ctx context.Context, q analyticsQuerier, id int64, params analytics.QueryParams) ([]analytics.ModelHealth, error) {
	const query = `
SELECT provider,
       model,
       streaming,
       count(*)::bigint,
       count(*) FILTER (WHERE ` + errorPredicate + `)::bigint,
       coalesce(avg(duration_ms), 0)::float8,
       coalesce(percentile_cont(0.95) WITHIN GROUP (ORDER BY duration_ms), 0)::float8
FROM gateway.llm_request_logs` + requestLogWindow + `
  AND model <> ''
GROUP BY provider, model, streaming
ORDER BY count(*) DESC, model ASC, streaming ASC
LIMIT $4`

	rows, err := q.Query(ctx, query, id, params.From, params.To, healthModelRowsLimit)
	if err != nil {
		return nil, fmt.Errorf("analytics: health by model: %w", err)
	}
	defer rows.Close()

	models := make([]analytics.ModelHealth, 0)
	for rows.Next() {
		var m analytics.ModelHealth
		if err := rows.Scan(&m.Provider, &m.Model, &m.Streaming, &m.Requests, &m.Errors,
			&m.AvgDurationMS, &m.P95DurationMS); err != nil {
			return nil, fmt.Errorf("analytics: health by model scan: %w", err)
		}
		m.ErrorRate = ratePercent(m.Errors, m.Requests)
		// Rounded here rather than in SQL so the two latency figures and the
		// rate all go through one rule.
		m.AvgDurationMS = math.Round(m.AvgDurationMS*10) / 10
		m.P95DurationMS = math.Round(m.P95DurationMS*10) / 10
		models = append(models, m)
	}
	return models, rows.Err()
}

// healthDaily is the requests-and-errors trend, one point per UTC day that had
// traffic.
//
// `AT TIME ZONE 'UTC'` for the reason dailyActivity gives: without it the
// bucket follows the connection's inherited TimeZone, and the same window
// bucketed on a differently-configured server shifts the chart by a column with
// nothing to show for it.
func healthDaily(ctx context.Context, q analyticsQuerier, id int64, params analytics.QueryParams) ([]analytics.DailyHealth, error) {
	const query = `
SELECT to_char(date_trunc('day', occurred_at AT TIME ZONE 'UTC'), 'YYYY-MM-DD'),
       count(*)::bigint,
       count(*) FILTER (WHERE ` + errorPredicate + `)::bigint
FROM gateway.llm_request_logs` + requestLogWindow + `
GROUP BY 1
ORDER BY 1`

	rows, err := q.Query(ctx, query, id, params.From, params.To)
	if err != nil {
		return nil, fmt.Errorf("analytics: health daily: %w", err)
	}
	defer rows.Close()

	daily := make([]analytics.DailyHealth, 0)
	for rows.Next() {
		var point analytics.DailyHealth
		if err := rows.Scan(&point.Date, &point.Requests, &point.Errors); err != nil {
			return nil, fmt.Errorf("analytics: health daily scan: %w", err)
		}
		daily = append(daily, point)
	}
	return daily, rows.Err()
}
