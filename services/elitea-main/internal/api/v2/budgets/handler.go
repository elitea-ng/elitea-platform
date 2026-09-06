// Package budgets serves the budgets / usage surface: per-project and
// per-member monthly LLM spend limits, and the current-period usage report the
// Settings → Usage and Admin → Budgets screens read.
//
// # Where the numbers come from
//
// Two tables, and only two:
//
//   - gateway.project_budget — the AUTHORED limit. The gateway's failmode store
//     point-reads this exact row on every LLM call
//     (services/elitea-llm-gateway/internal/failmode/store.go, snapshotSQL), so
//     a limit written here takes effect on the next call rather than at the next
//     period. That is the whole reason the port lands on this table instead of
//     gateway.governance_config, which elitea-main writes and nothing reads
//     (#218). Adding a second write-only config path was the one thing issue
//     #246 asked this change not to do.
//   - gateway.llm_budget_accumulators — the ACCRUED spend, one row per
//     (scope, scope_id, period_start). The gateway publishes billing deltas onto
//     the GATEWAY_BUDGET_DELTAS stream and elitea-scheduler's budgetwriteback
//     consumer folds them into this table. So this API reports what the
//     write-back path persists, which is issue #246's stated data source.
//   - gateway.llm_usage_events — the per-request ledger added by issue #320:
//     one row per billed call, carrying the member, the provider, the model and
//     the provider-reported token counts. It is a REPORT of the same money the
//     accumulator holds, written from the same delta under the same event id,
//     and no budget decision reads it. Only the usage endpoint's dimensional
//     views come from here; every limit and every spend figure still comes from
//     the accumulator, so the two can never disagree about what was billed.
//
// # The dimensional views, and when they are absent
//
// The pylon original assembled its numbers from LiteLLM, which kept a
// per-model, per-day, per-token ledger. Until issue #320 nothing in this
// platform kept one, so the token counts, the per-model table and the per-day
// series were absent from every response. gateway.llm_usage_events supplies
// them now.
//
// They are still ABSENT rather than empty for a project the ledger has no rows
// for, and the distinction is load-bearing in a way an empty array cannot
// carry. A deployment upgraded part-way through a billing period has
// accumulator spend from before the ledger existed and no events behind it;
// zero-filling the chart there would render as "this project made no calls",
// which is a different and wrong claim about the same period. The usage payload
// says which case it is in `usage_events_available`, exactly as
// `spend_available` already distinguishes "no spend yet" from "no data".
//
// # Denominations
//
// Limits and accumulated spend are USD NUMERIC. The gateway's counter is int64
// nano-USD and model prices are per 1,000,000 tokens; neither appears here.
// Every money value in these responses is produced by PostgreSQL as an exact
// NUMERIC and marshalled as a JSON number via json.Number — it is never
// round-tripped through float64, so a limit of 0.10 does not come back as
// 0.10000000000000001.
package budgets

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

// Permissions the routes are gated on in internal/api/router.go, transcribed
// from the `check_api` declarations of the pylon handlers this ports.
const (
	// AdminViewPermission gates every administration-mode read
	// (project_budget.py, project_budgets.py, user_budget.py, user_budgets.py).
	AdminViewPermission = "models.admin.project_budgets.view"
	// AdminEditPermission gates the two administration-mode writes
	// (project_budget.py PUT, user_budget.py PUT).
	AdminEditPermission = "models.admin.project_budgets.edit"
	// ProjectViewPermission gates the project-scoped (prompt_lib) reads, in
	// DEFAULT mode, exactly as the pylon `PromptLibAPI` handlers declare.
	ProjectViewPermission = "models.project_context.view"
)

// budgetScopeProject is the accumulator `scope` the gateway bills against —
// llmproxy/budget_gate.go's `budgetScopeProject`. Its `scope_id` is the numeric
// project id as text.
const budgetScopeProject = "project"

// budgetScopeUser is the accumulator scope a per-member figure is keyed by.
// Since issue #321 the gateway writes it: llmproxy/budget_gate.go bills a
// second delta under this scope for every call that carries a member id, and
// admits against it. Before that the reads below asked for a row nothing wrote,
// and every per-member figure reported spend_available=false. See
// userBudgetScopeID for the key shape, which the gateway mirrors in
// failmode.UserScopeID.
const budgetScopeUser = "user"

// defaultCurrency is the only currency this platform prices in. The gateway
// compares hard_limit_usd against a nano-USD counter, and there is no
// conversion anywhere in the money path, so a budget stored as "EUR" would be
// enforced as USD. The write path rejects anything else rather than storing a
// label the enforcement ignores; the reference stored it and let LiteLLM
// enforce USD regardless.
const defaultCurrency = "USD"

// Limit sources reported as `limit_source`, narrowed from the reference's three
// to two. pylon's "default" meant a platform-wide LiteLLM fallback limit; this
// platform has no such fallback — gateway.project_budget is the only limit
// there is — so a project either has an explicit one or is unlimited.
const (
	limitSourceExplicit  = "explicit"
	limitSourceUnlimited = "unlimited"
)

// Handler serves the budgets and usage routes.
type Handler struct {
	pool *pgxpool.Pool
	// now is the clock the reported period is derived from. Production leaves
	// it nil (time.Now); tests pin it so a run at 23:59 on the last of the
	// month cannot straddle a period boundary between the write and the read —
	// the shape that made the audit-seed E2E fail at midnight.
	now func() time.Time
}

// Option configures a Handler at construction.
type Option func(*Handler)

// WithClock pins the clock used to derive the reporting period.
func WithClock(clock func() time.Time) Option {
	return func(h *Handler) { h.now = clock }
}

// NewHandler builds a Handler over the given pool.
func NewHandler(pool *pgxpool.Pool, options ...Option) *Handler {
	handler := &Handler{pool: pool}
	for _, option := range options {
		option(handler)
	}
	return handler
}

// Routes returns a standalone router carrying every budgets route, mounted at
// the same relative paths router.go registers them under. Used by tests; the
// production wiring registers each route individually so each can carry its own
// RBAC gate (the gates differ per mode, and chi cannot carry a per-route gate
// across a Mount boundary).
func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/project_budget/prompt_lib/{projectID}/budget", h.GetProjectBudget)
	r.Get("/project_budget/administration/{projectID}/budget", h.GetProjectBudgetAdmin)
	r.Put("/project_budget/administration/{projectID}/budget", h.PutProjectBudget)
	r.Delete("/project_budget/administration/{projectID}/budget", h.DeleteProjectBudget)
	r.Get("/project_budgets/administration", h.ListProjectBudgets)
	r.Get("/user_budget/prompt_lib/{projectID}/user_budget/{userID}", h.GetUserBudget)
	r.Get("/user_budget/administration/{projectID}/user_budget/{userID}", h.GetUserBudgetAdmin)
	r.Put("/user_budget/administration/{projectID}/user_budget/{userID}", h.PutUserBudget)
	r.Delete("/user_budget/administration/{projectID}/user_budget/{userID}", h.DeleteUserBudget)
	r.Get("/user_budgets/prompt_lib/{projectID}", h.ListUserBudgets)
	r.Get("/user_budgets/administration/{projectID}", h.ListUserBudgetsAdmin)
	r.Get("/usage/prompt_lib/{projectID}/usage", h.GetUsage)
	return r
}

func (h *Handler) clock() time.Time {
	if h.now == nil {
		return time.Now()
	}
	return h.now()
}

/* ── the reporting period ──────────────────────────────────────────────── */

// reportingPeriod is the calendar month a budget accrues over, derived exactly
// as the gateway derives it (llmproxy/budget_gate.go billingPeriodStart): the
// first instant of the current month in UTC. The two MUST agree — the
// accumulator's unique key is (scope, scope_id, period_start), so a period
// computed in local time would look up a row that does not exist and report
// zero spend against a real limit.
type reportingPeriod struct {
	start time.Time
	end   time.Time // first instant of the NEXT month; the period is half-open
}

func periodFor(now time.Time) reportingPeriod {
	utc := now.UTC()
	start := time.Date(utc.Year(), utc.Month(), 1, 0, 0, 0, 0, time.UTC)
	return reportingPeriod{start: start, end: start.AddDate(0, 1, 0)}
}

// label is the "YYYYMM" tag the reference reported as `period`.
func (p reportingPeriod) label() string { return p.start.Format("200601") }

// firstDay / lastDay are the inclusive calendar bounds the reference reported
// as `period_start` / `period_end`, so a client can plot a full month.
func (p reportingPeriod) firstDay() string { return p.start.Format("2006-01-02") }
func (p reportingPeriod) lastDay() string  { return p.end.AddDate(0, 0, -1).Format("2006-01-02") }

// resetsAt is when the accumulator rolls over to a new period row.
func (p reportingPeriod) resetsAt() string { return p.end.Format(time.RFC3339) }

/* ── exact money marshalling ───────────────────────────────────────────── */

// numeric turns a PostgreSQL NUMERIC rendered as text into a JSON number,
// preserving the exact decimal. A nil pointer (SQL NULL) marshals as JSON null.
//
// json.Number rather than float64 on purpose: these values are money, and the
// only reason they are text at the SQL boundary is to keep them out of a binary
// float on the way here.
func numeric(text *string) *json.Number {
	if text == nil {
		return nil
	}
	value := json.Number(*text)
	return &value
}

/* ── request plumbing ──────────────────────────────────────────────────── */

// pathID reads a positive integer path parameter.
func pathID(r *http.Request, name string) (int64, bool) {
	value, err := strconv.ParseInt(chi.URLParam(r, name), 10, 64)
	return value, err == nil && value > 0
}

// callerID is the authenticated user's own id, which the project-scoped reads
// need in order to answer "is this you, and are you an admin here?".
func callerID(r *http.Request) (int64, bool) {
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		return 0, false
	}
	return user.OwningUserID()
}

// isProjectAdmin reports whether the user holds the project's `admin` role —
// the check `admin_check_user_is_admin` performs in the reference, asked
// directly of the tables legacyrbac.PostgresResolver reads.
//
// A failure to ASK is not an answer: the reference swallowed the RPC exception
// and treated it as "not an admin", which turns a database blip into a 403 on a
// page the caller is entitled to. Here the error propagates and the caller
// answers 500.
func (h *Handler) isProjectAdmin(ctx context.Context, projectID, userID int64) (bool, error) {
	var isAdmin bool
	err := h.pool.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1
    FROM public.auth_core__project_user_role AS assignment
    JOIN public.auth_core__project_role AS project_role
      ON project_role.id = assignment.role_id
     AND project_role.project_id = assignment.project_id
    WHERE assignment.project_id = $1
      AND assignment.user_id = $2
      AND project_role.name = 'admin'
)`, projectID, userID).Scan(&isAdmin)
	if err != nil {
		return false, fmt.Errorf("budgets: resolve project admin: %w", err)
	}
	return isAdmin, nil
}

// isPersonalProject reports whether the project is this user's own personal
// project. The predicate is the one the admin project listing already uses
// (internal/api/v2/admin/projects.go personalProjectPredicate) resolved to the
// specific owner, rather than the hardcoded personal_project_id "1" that #161
// found in the social author path.
func (h *Handler) isPersonalProject(ctx context.Context, projectID, userID int64) (bool, error) {
	// The name is composed here rather than in SQL: `'project_user_' || $2` puts
	// the parameter in a text context, and pgx then refuses to encode an int64
	// as text at all ("cannot find encode plan"), so the query fails outright
	// rather than returning a wrong answer.
	var personal bool
	err := h.pool.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM centry.project WHERE id = $1 AND name = $2)`,
		projectID, "project_user_"+strconv.FormatInt(userID, 10)).Scan(&personal)
	if err != nil {
		return false, fmt.Errorf("budgets: resolve personal project: %w", err)
	}
	return personal, nil
}

/* ── responses ─────────────────────────────────────────────────────────── */

func writeJSON(w http.ResponseWriter, code int, payload any) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		apierr.WriteStatus(w, http.StatusInternalServerError, "internal server error")
		return
	}
	encoded = append(encoded, '\n')
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(encoded)
}

func writeError(w http.ResponseWriter, code int, message string) {
	writeJSON(w, code, map[string]any{"error": message})
}

// structToMap re-reads a response struct as a mutable map so the redacting
// endpoints can REMOVE fields for a caller who may not see cost figures.
//
// It round-trips through the struct's own JSON tags rather than reflecting over
// field names, so the redacted payload and the unredacted one are guaranteed to
// be spelled the same way. UseNumber keeps every money value as the exact
// decimal PostgreSQL produced — decoding into float64 here would undo the whole
// reason these fields are json.Number.
//
// The error is RETURNED, not swallowed into an empty map. A json.Number holding
// something PostgreSQL can produce but JSON cannot express — NUMERIC admits
// 'NaN', and 'Infinity' on PG14+ — fails Marshal, and an empty map would then
// be served as a 200 whose body had silently lost every budget field. Every
// other write path in this package reports that failure as a 500; so does this
// one now.
func structToMap(value any) (map[string]any, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("budgets: encode payload: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	payload := map[string]any{}
	if err := decoder.Decode(&payload); err != nil {
		return nil, fmt.Errorf("budgets: decode payload: %w", err)
	}
	return payload, nil
}
