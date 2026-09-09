package analytics

import (
	"encoding/json"
	"time"
)

// UsageSummary is the project overview: what the LLM path did in a window.
//
// Every field here has a producer. The figures that do not — tool_runs,
// chat_msgs, agent_runs, and a per-model cost — are ABSENT from this struct
// rather than present and zero, for the reason errors.go gives: a zero is a
// count, and a count nobody measured is a claim.
type UsageSummary struct {
	ProjectID   string       `json:"project_id,omitempty"`
	Period      string       `json:"period"`
	TotalTokens int64        `json:"total_tokens"`
	TotalRuns   int64        `json:"total_runs"`
	ByModel     []ModelUsage `json:"by_model,omitempty"`

	// ModelsTruncated is true when ByModel was cut to the busiest N pairs. The
	// client normalises its share column by summing that array, so a silent cut
	// turns every share into a percentage of the subset rather than of the
	// project — beside a llm_calls figure carrying the real total.
	ModelsTruncated bool `json:"models_truncated"`

	// ActiveUsers is how many distinct callers made an LLM call in the window.
	//
	// CALLERS, not members. It counts every identity the gateway resolved under
	// this project, which includes a removed member, a global administrator and
	// a service token — all of them real usage, none of them in the membership
	// table. That is why it is NOT the numerator of the adoption rate; see
	// ActiveMembers.
	ActiveUsers int64 `json:"active_users"`

	// ActiveMembers is the subset of ActiveUsers that the project's membership
	// actually contains, and is the numerator of the adoption rate. Nil
	// whenever TotalProjectUsers is: both come from the same statement, because
	// a numerator and a denominator measured over two snapshots can disagree.
	ActiveMembers *int64 `json:"active_members,omitempty"`

	// TotalProjectUsers is the project's membership count, and is nil when the
	// membership tables are absent — `public.auth_core__user_role` and
	// `public.auth_core__project_role` are owned by a different corpus and are
	// not created by this service's migrations, so a Go-bootstrapped database
	// legitimately has neither. Nil rather than 0 because "no membership
	// source" and "a project with no members" are different claims, and an
	// adoption rate computed from the second would be a division by zero
	// dressed up as a percentage.
	TotalProjectUsers *int64 `json:"total_project_users,omitempty"`

	// DailyActivity is one point per UTC day that had traffic. Days with no
	// traffic are absent rather than zero-filled: the client draws the axis
	// from the window it asked for and knows which days it is missing.
	DailyActivity []DailyPoint `json:"daily_activity,omitempty"`

	// TopUsers is the leaderboard, most calls first, capped at TopUsersLimit.
	TopUsers []UserActivity `json:"top_users,omitempty"`

	// Health is what went WRONG, and how slowly. Nil only when the read failed;
	// a project with no traffic has a Health with zero totals and empty
	// breakdowns, which is a different and true statement.
	Health *Health `json:"health,omitempty"`
}

// Health is the reliability and latency view of the same window.
//
// It exists because the gateway's per-request log records the requests that
// FAILED, and no other table in this platform does: the billing ledger is
// written from a billing delta, and a billing delta rides only a billed
// request, so a call refused by a budget, rejected by a policy, addressed to an
// unresolvable model or failed upstream never reaches it. A health view built
// over the ledger would list successes and no failures — the opposite of what
// an operator opens one for.
type Health struct {
	// Requests and Errors are the window's totals. An error is status >= 400,
	// the same predicate 0099's partial index is built on.
	Requests int64 `json:"requests"`
	Errors   int64 `json:"errors"`
	// ErrorRate is Errors over Requests, as a percentage to one decimal. Zero
	// for a window with no traffic, which is honest: nothing failed.
	ErrorRate float64 `json:"error_rate"`

	// ByErrorCode is the failure breakdown, most frequent first.
	//
	// The gateway's own CLASSIFICATION, never an upstream string — 0099 has no
	// column a provider's error text can reach, and that is structural rather
	// than a policy somebody has to remember: upstream errors routinely quote
	// the offending fragment of the request back, and a request is user-authored
	// free text.
	ByErrorCode []ErrorCodeCount `json:"by_error_code"`
	// ErrorCodesTruncated is true when ByErrorCode was cut. It sits beside an
	// uncapped Errors total, so a silent cut leaves an operator reconciling the
	// two with failures that belong to no listed classification.
	ErrorCodesTruncated bool `json:"error_codes_truncated"`

	// ByModel is per (provider, model, streaming). Empty for a window whose
	// requests never resolved a model.
	ByModel []ModelHealth `json:"by_model"`

	// Daily is one point per UTC day that had traffic, for the trend chart.
	Daily []DailyHealth `json:"daily"`
}

// ErrorCodeCount is one failure classification's share of the window.
type ErrorCodeCount struct {
	ErrorCode string `json:"error_code"`
	Requests  int64  `json:"requests"`
}

// ModelHealth is one (provider, model, streaming) triple's reliability.
//
// STREAMING IS PART OF THE KEY, not a column. 0099's own header says a streamed
// and a buffered request of the same model have very different latency
// profiles and that averaging them together makes both unreadable — a streamed
// response's duration is the whole stream, which is seconds where a buffered
// call is milliseconds. Folding them into one row would produce a number that
// describes neither, and would move whenever the streamed/buffered mix did.
type ModelHealth struct {
	Provider  string  `json:"provider"`
	Model     string  `json:"model"`
	Streaming bool    `json:"streaming"`
	Requests  int64   `json:"requests"`
	Errors    int64   `json:"errors"`
	ErrorRate float64 `json:"error_rate"`
	// AvgDurationMS and P95DurationMS are wall-clock milliseconds from the start
	// of the handler to the response being complete. Both are reported because
	// they answer different questions and an average alone hides the tail an
	// operator investigating "chat feels slow" is looking for.
	AvgDurationMS float64 `json:"avg_duration_ms"`
	P95DurationMS float64 `json:"p95_duration_ms"`
}

// DailyHealth is one UTC day: how much was served and how much of it failed.
type DailyHealth struct {
	Date     string `json:"date"`
	Requests int64  `json:"requests"`
	Errors   int64  `json:"errors"`
}

// ModelUsage is one (provider, model) pair's share of the window.
//
// There is no TotalCost. Money is keyed by (scope, scope_id, period) in
// gateway.llm_budget_accumulators and carries no model dimension, so a
// per-model cost cannot be derived from anything this platform writes —
// /analytics_costs says the same thing about the same table. A zero here would
// read as "this model was free".
type ModelUsage struct {
	Model            string `json:"model"`
	Provider         string `json:"provider"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
	RunCount         int64  `json:"run_count"`
}

// DailyPoint is one UTC day of the window.
type DailyPoint struct {
	Date        string `json:"date"`
	LLMCalls    int64  `json:"llm_calls"`
	TotalTokens int64  `json:"total_tokens"`
	ActiveUsers int64  `json:"active_users"`
}

type AgentAnalytics struct {
	ApplicationID string  `json:"application_id"`
	Name          string  `json:"name"`
	RunCount      int64   `json:"run_count"`
	AvgDuration   float64 `json:"avg_duration_ms"`
	TotalTokens   int64   `json:"total_tokens"`
	ErrorRate     float64 `json:"error_rate"`

	// Priced (issue #875) is true when the catalogue prices at least one call
	// this agent made. The three money fields below are pointers so they
	// disappear from the JSON when nothing priced them — the same
	// "priced"/omitted-money contract estimate.go's by_model and by_user rows
	// already carry, so a client reading either dimension learns the absence
	// the same way.
	//
	// This is an ESTIMATE, derived from gateway.gateway_models the way every
	// other cost figure this platform derives (never accounted) is. It can
	// never disagree with GetUsageSummary's total_tokens for the same agent,
	// because it is computed over the SAME rows agentUsage already resolves —
	// it adds a price to a token count agentUsage was already summing.
	Priced     bool         `json:"priced"`
	InputCost  *json.Number `json:"input_cost,omitempty"`
	OutputCost *json.Number `json:"output_cost,omitempty"`
	TotalCost  *json.Number `json:"total_cost,omitempty"`
}

// AgentBreakdown is the Agents tab, and the reason it is a struct rather than a
// bare []AgentAnalytics is Available.
//
// # Why an availability flag and not an empty list
//
// The agent dimension is a COLUMN ADDED LATE. gateway.llm_request_logs gained
// execution_id in shared migration 0100, and nothing in the rows written before
// it identifies an agent — the log records the model a request addressed, never
// what composed it, and no other table records the correlation after the fact.
// A backfill is therefore impossible rather than merely unwritten, and any
// value put into those rows would be invented.
//
// So a window that predates the migration has NO agent data, and that is not
// the same claim as "no agent ran". Rendering it as an empty list beside a
// live llm_calls figure would produce the 200-with-a-fallback-body this
// codebase keeps being burned by: a dashboard reporting "0 agent runs" for a
// month in which agents ran constantly, with nothing on screen able to tell the
// difference.
//
// The precedent is exact and deliberate: usageDimensions.Available
// (internal/api/v2/budgets/usage_dimensions.go) exists because a deployment
// upgraded mid-period has accumulator spend from before the ledger existed, and
// it OMITS the dimensional block rather than zero-filling it. This does the
// same. When Available is false, Agents is nil and the handler emits no items
// key at all.
type AgentBreakdown struct {
	// Available reports whether the window contains at least one request
	// carrying an execution id — that is, whether this deployment was writing
	// the agent dimension while these requests were served.
	//
	// False means "no data for this window", which covers a period before
	// migration 0100 and a deployment whose runtime is not tagging its calls.
	// It does NOT mean "no agent ran", and the two must not render alike.
	Available bool `json:"agent_dimension_available"`

	// Agents is the per-agent table, busiest first. Nil when Available is
	// false. Empty (and present) when the window HAS attributable requests but
	// none of them resolves to a named agent — an ad-hoc run, or an execution
	// whose chat projection has since been deleted. That is a real and
	// different state from both of the above.
	Agents []AgentAnalytics `json:"-"`

	// AttributedCalls and UnattributedCalls split the window's requests. They
	// are published because the per-agent rows do NOT sum to the project's
	// llm_calls tile and never will: most /llm traffic is not made from a
	// runtime execution. Without both figures on screen an operator is left
	// reconciling a breakdown against a total it was never a partition of.
	AttributedCalls   int64 `json:"attributed_llm_calls"`
	UnattributedCalls int64 `json:"unattributed_llm_calls"`

	// Truncated is true when the table was cut to the busiest N agents, stated
	// for the reason ModelsTruncated is: the client normalises shares by summing
	// what it received.
	Truncated bool `json:"truncated"`
}

type ToolAnalytics struct {
	ToolkitID string `json:"toolkit_id"`
	// ToolkitName is the toolkit the call ran against, as the producer knew it.
	// It travels beside ToolkitID because the two producers know different
	// halves of that identity: an explicit run holds the saved toolkit's row id
	// and an agent turn holds only the name its tool metadata carried. A row
	// with a name and no id is a real measurement, not a broken one.
	ToolkitName string  `json:"toolkit_name"`
	ToolName    string  `json:"tool_name"`
	RunCount    int64   `json:"run_count"`
	ErrorCount  int64   `json:"error_count"`
	AvgDuration float64 `json:"avg_duration_ms"`
	ErrorRate   float64 `json:"error_rate"`
}

// ToolBreakdown is the Tools tab, and it is a struct rather than a bare
// []ToolAnalytics for the reason AgentBreakdown is: Available.
//
// # Why an availability flag and not an empty list
//
// The tool dimension is a TABLE ADDED LATE. Before shared migration 0119
// nothing in this platform recorded a tool call in a form a project-wide read
// could group by — the explicit run's execution_jobs row carries no toolkit id
// and no tool name, and the agent turn's trace step is per-tenant, covers chat
// turns only, and carries no toolkit id either. Neither is a producer that can
// be read after the fact, so there is nothing to backfill and none is invented.
//
// A window that ends before that migration was applied therefore has NO tool
// data, which is a different sentence from "no tool ran". Rendering it as an
// empty list would report a month of constant tool use as zero, with nothing on
// screen able to tell the difference — the 200-with-a-fallback-body this
// codebase keeps being burned by. When Available is false, Tools is nil and the
// handler emits no items key at all.
type ToolBreakdown struct {
	// Available reports whether this deployment was recording tool calls for
	// the whole of the requested window.
	//
	// False means "no data for this window": a window that ends before shared
	// migration 0119 ran, or a database that has not run it. It does NOT mean
	// "no tool ran".
	Available bool `json:"tool_dimension_available"`

	// Tools is the per-tool table, busiest first. Nil when Available is false.
	// Empty (and present) when the deployment WAS recording and no tool ran in
	// the window — a real and different state, and the one an empty list is
	// allowed to say.
	Tools []ToolAnalytics `json:"-"`

	// Truncated is true when the table was cut to the busiest N tools, stated
	// for the reason AgentBreakdown.Truncated is: the client normalises shares
	// by summing what it received.
	Truncated bool `json:"truncated"`
}

// UserActivity is one member's LLM usage in the window.
//
// Email is empty when the identity tables are absent — the same guarded read as
// TotalProjectUsers. The row is still reported: "user 41 made 900 calls" is
// useful without a display name, and dropping the row because a join failed
// would silently shrink a leaderboard.
type UserActivity struct {
	UserID       string    `json:"user_id"`
	Email        string    `json:"email"`
	Name         string    `json:"name,omitempty"`
	RunCount     int64     `json:"run_count"`
	TotalTokens  int64     `json:"total_tokens"`
	LastActiveAt time.Time `json:"last_active_at"`
}

// QueryParams is one analytics read's scope.
//
// From/To are the RESOLVED window — parsed, defaulted and clamped by the API
// layer's dateWindow, so every repository method reads the same instants and no
// query has to re-interpret a raw string. StartDate/EndDate are kept as the
// caller sent them for diagnostics only; nothing queries on them.
type QueryParams struct {
	ProjectID string    `json:"-"`
	From      time.Time `json:"-"`
	To        time.Time `json:"-"`
	StartDate string    `json:"start_date,omitempty"`
	EndDate   string    `json:"end_date,omitempty"`
	Period    string    `json:"period,omitempty"`
	Page      int       `json:"page,omitempty"`
	PageSize  int       `json:"page_size,omitempty"`
}
