package budgets

// Project budgets.
//
//	GET  /elitea_core/project_budget/{mode}/{project_id}/budget   ← project_budget.py
//	PUT  /elitea_core/project_budget/administration/{project_id}/budget
//	GET  /elitea_core/project_budgets/administration              ← project_budgets.py
//
// The PUT is the only write in this package that reaches enforcement: it sets
// gateway.project_budget.is_unlimited and hard_limit_usd, which the gateway's
// failmode snapshot reads on the next call.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// budgetState is the assembled read model both the project and the member
// endpoints answer with. Its money fields are *json.Number so an unlimited
// scope reports `null` rather than a zero that reads like "no budget left".
type budgetState struct {
	MonthlyLimit   *json.Number `json:"monthly_limit"`
	EffectiveLimit *json.Number `json:"effective_limit"`
	LimitSource    string       `json:"limit_source"`
	Currency       string       `json:"currency"`
	Enabled        bool         `json:"enabled"`
	WarningPct     int          `json:"warning_pct"`
	Spend          *json.Number `json:"spend"`
	Remaining      *json.Number `json:"remaining"`
	PercentUsed    *json.Number `json:"percent_used"`
	SpendAvailable bool         `json:"spend_available"`
	Period         string       `json:"period"`
	PeriodStart    string       `json:"period_start"`
	PeriodEnd      string       `json:"period_end"`
	ResetsAt       string       `json:"resets_at"`
}

// projectBudgetState is a project's budget plus the two policy columns that
// exist only at project scope: the period it accrues over and the NATS-failure
// policy the gateway applies to it.
//
// Both are now read from the STORED row rather than reported as literals.
// `budget_period` used to be the constant "monthly" whatever the column held,
// and `nats_fail_mode` was not reported at all, so neither column had a reader
// and neither had a writer — an operator could not see or change either.
//
// NatsFailMode is a pointer because NULL is a real and distinct state: it means
// "inherit the platform baseline" (LLM_BUDGET_NATS_FAIL_MODE, resolved by
// services/elitea-llm-gateway/internal/failmode.ResolveFailMode), which is not
// the same as any of the three named modes.
type projectBudgetState struct {
	ProjectID    int64   `json:"project_id"`
	BudgetPeriod string  `json:"budget_period"`
	NatsFailMode *string `json:"nats_fail_mode"`
	budgetState
}

// budgetStateSelect assembles one scope's budget and current-period spend in a
// single round trip.
//
// Every derived money figure — remaining, percent_used — is computed by
// PostgreSQL in NUMERIC and returned as text. Computing them here in float64
// would reintroduce the rounding the whole money path is built to avoid, and
// the percentage is what a warning banner triggers on.
//
// The LEFT JOINs from a one-row anchor are what make "this project has no
// budget row" and "this project has a budget row" the same shape: the query
// always returns exactly one row, so a project nobody has configured reports an
// unlimited state instead of 404-ing a page that legitimately has nothing to
// show yet.
//
// Arguments: $1 scope_id, $2 scope, $3 period_start, $4 default warning pct,
// then whatever the caller's limit-table join binds from $5 on.
//
// `enforced` is read from the limit join's `is_unlimited`, NOT from `enabled`.
// For gateway.project_budget those are two different columns and only the first
// one governs: it is what the gateway's failmode snapshot reads. `enabled` is
// the AUTHORED flag, and a row this API did not write can have the two
// disagree — a pre-existing row with `is_unlimited = true` and a non-null
// hard_limit_usd is backfilled to `enabled = true` by 003, and deriving from
// `enabled` would then report an enforced ceiling that admits every call. Both
// limit joins project an `is_unlimited`, so this select reads what is enforced
// and reports `enabled` alongside it without conflating them.
const budgetStateSelect = `
SELECT
    limits.hard_limit_usd::text                                  AS monthly_limit,
    COALESCE(limits.enabled, false)                              AS enabled,
    (NOT COALESCE(limits.is_unlimited, true))                    AS enforced,
    COALESCE(limits.soft_alert_pct, ` + globalWarningPctSQL + `, $4::smallint) AS warning_pct,
    COALESCE(accrued.accumulated_cost, 0)::text                  AS spend,
    (accrued.accumulated_cost IS NOT NULL)                       AS spend_available,
    CASE WHEN NOT COALESCE(limits.is_unlimited, true) AND limits.hard_limit_usd IS NOT NULL
         THEN GREATEST(0, limits.hard_limit_usd - COALESCE(accrued.accumulated_cost, 0))::text
    END                                                          AS remaining,
    CASE WHEN NOT COALESCE(limits.is_unlimited, true) AND limits.hard_limit_usd > 0
         THEN round(COALESCE(accrued.accumulated_cost, 0) / limits.hard_limit_usd * 100, 2)::text
    END                                                          AS percent_used`

// readBudgetState runs budgetStateSelect against one limit table. limitJoin is
// a fixed SQL fragment chosen by the caller from the two constants below, never
// assembled from request data; joinArgs bind its $5-onward placeholders.
func (h *Handler) readBudgetState(
	ctx context.Context, limitJoin, scope, scopeID string, period reportingPeriod, joinArgs ...any,
) (budgetState, error) {
	var (
		monthlyLimit   *string
		enabled        bool
		enforced       bool
		warningPct     int
		spend          string
		spendAvailable bool
		remaining      *string
		percentUsed    *string
	)
	query := budgetStateSelect + `
FROM (SELECT 1) AS anchor(one)
` + limitJoin + `
LEFT JOIN gateway.llm_budget_accumulators AS accrued
       ON accrued.scope = $2 AND accrued.scope_id = $1
      AND accrued.period_start = $3::timestamptz`
	args := append([]any{scopeID, scope, period.start, DefaultWarningPct}, joinArgs...)
	err := h.pool.QueryRow(ctx, query, args...).Scan(
		&monthlyLimit, &enabled, &enforced, &warningPct, &spend, &spendAvailable, &remaining, &percentUsed,
	)
	if err != nil {
		return budgetState{}, fmt.Errorf("budgets: read %s budget state: %w", scope, err)
	}

	state := budgetState{
		MonthlyLimit:   numeric(monthlyLimit),
		Currency:       defaultCurrency,
		Enabled:        enabled,
		WarningPct:     warningPct,
		Spend:          numeric(&spend),
		Remaining:      numeric(remaining),
		PercentUsed:    numeric(percentUsed),
		SpendAvailable: spendAvailable,
		Period:         period.label(),
		PeriodStart:    period.firstDay(),
		PeriodEnd:      period.lastDay(),
		ResetsAt:       period.resetsAt(),
	}
	// The enforced limit, which is not the stored one: a scope the gateway
	// treats as unlimited keeps the number an operator typed but is not held
	// to it. `enforced` comes from is_unlimited, so this cannot claim a ceiling
	// the gateway is not applying.
	state.LimitSource = limitSourceUnlimited
	if enforced && monthlyLimit != nil {
		state.EffectiveLimit = state.MonthlyLimit
		state.LimitSource = limitSourceExplicit
	}
	return state, nil
}

// DefaultWarningPct is the last-resort utilisation percentage, used when
// neither the scope nor the platform config names one.
//
// It is the SAME number as internal/api/gateway.DefaultSoftAlertThresholdPct
// and as the literal in the gateway's own snapshot query
// (failmode/store.go defaultSoftAlertPctSQL). It is a FALLBACK now rather than
// a default: since #322 the platform threshold an operator sets through
// PUT /admin/gateway/budget-alerts is consulted first, and migration 0084 made
// gateway.project_budget.soft_alert_pct nullable so "this project authored no
// threshold" is representable and that platform value has something to apply
// to. Before that the column was NOT NULL DEFAULT 80 and the global default
// could never reach anything.
const DefaultWarningPct = 80

// globalWarningPctSQL is the platform default threshold: the same
// gateway.governance_config row the gateway's snapshot query reads and the
// budget-alerts surface writes (internal/api/gateway/budget_alerts.go).
//
// It is spliced into the COALESCE chain of every read that reports a warning
// percentage, so the API and the gateway resolve the same threshold for the
// same project. A reader that stopped at DefaultWarningPct would show an
// operator 80 while the gateway alerted at the value they set.
//
// The cast is guarded for the same reason the gateway's copy is: this JSONB
// column is reachable by direct SQL, and a bare cast over a non-numeric value
// raises 22P02 and turns one bad row into a 500 on every budget read. A value
// that fails the guard falls through to DefaultWarningPct.
const globalWarningPctSQL = `(SELECT CASE
	    WHEN data->>'threshold_pct' ~ '^[0-9]{1,3}$'
	     AND (data->>'threshold_pct')::int BETWEEN 1 AND 100
	    THEN (data->>'threshold_pct')::smallint END
	FROM gateway.governance_config
	WHERE section = 'governance' AND type = 'budget_alert' AND name = 'global'
	  AND enabled)`

// projectLimitJoin and userLimitJoin (user_budgets.go) are the two fixed
// limit-table joins readBudgetState selects between. Both MUST expose
// hard_limit_usd, enabled, soft_alert_pct and is_unlimited — the last is what
// budgetStateSelect reads to decide whether a ceiling is enforced, and for the
// project table it is a real column rather than a derivation.
const projectLimitJoin = `LEFT JOIN (
    SELECT project_id, hard_limit_usd, enabled, soft_alert_pct, is_unlimited
    FROM gateway.project_budget
) AS limits ON limits.project_id = $5::integer`

// userBudgetScopeID is the accumulator scope_id a per-member figure would be
// keyed by. Nothing publishes it today (see budgetScopeUser); the shape is
// project-qualified because a user id alone would collide across projects, and
// the accumulator's unique key is (scope, scope_id, period_start).
func userBudgetScopeID(projectID, userID int64) string {
	return strconv.FormatInt(projectID, 10) + ":" + strconv.FormatInt(userID, 10)
}

/* ── GET the project's own budget ──────────────────────────────────────── */

// GetProjectBudget serves the PROJECT-SCOPED (prompt_lib) read, gated on
// `models.project_context.view` against the project in the path.
//
// It applies the same amount redaction /usage does, and for the same reason:
// the two endpoints serve the SAME spend and limit figures behind the SAME
// gate, so a member refused the amounts on one could simply read them off the
// other. The reference redacts on /usage only, which makes the control
// decorative the moment both endpoints exist; the divergence is deliberate.
func (h *Handler) GetProjectBudget(w http.ResponseWriter, r *http.Request) {
	projectID, ok := pathID(r, "projectID")
	if !ok {
		writeError(w, http.StatusBadRequest, "project id must be a positive integer")
		return
	}
	caller, authenticated := callerID(r)
	if !authenticated {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}

	ctx := r.Context()
	state, err := h.projectBudget(ctx, projectID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read project budget")
		return
	}
	payload, err := structToMap(state)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read project budget")
		return
	}
	visible, err := h.canSeeAmounts(ctx, projectID, caller)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to resolve project role")
		return
	}
	applyAmountVisibility(payload, visible)
	writeJSON(w, http.StatusOK, payload)
}

// GetProjectBudgetAdmin serves the administration-mode read, gated centrally on
// `models.admin.project_budgets.view`. A platform administrator is entitled to
// the cost figures by definition, so it does not redact and carries no
// can_see_amounts field.
func (h *Handler) GetProjectBudgetAdmin(w http.ResponseWriter, r *http.Request) {
	projectID, ok := pathID(r, "projectID")
	if !ok {
		writeError(w, http.StatusBadRequest, "project id must be a positive integer")
		return
	}
	state, err := h.projectBudget(r.Context(), projectID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read project budget")
		return
	}
	writeJSON(w, http.StatusOK, state)
}

// projectPolicySelect reads the two project-only policy columns.
//
// It is a SECOND round trip rather than two more columns on budgetStateSelect,
// and deliberately so: that select is shared verbatim with the per-member read,
// and gateway.user_budget has neither column. Widening the shared select would
// need a second, divergent copy of it — the exact duplication its doc comment
// exists to prevent — to save one indexed point-read on an admin-rate endpoint.
//
// A project with no row returns pgx.ErrNoRows, which is not an error here: it
// is the default state, and it resolves to the same answer the CREATE TABLE
// default would give.
const projectPolicySelect = `
SELECT budget_period, nats_fail_mode
FROM gateway.project_budget
WHERE project_id = $1`

func (h *Handler) projectBudget(ctx context.Context, projectID int64) (projectBudgetState, error) {
	period := periodFor(h.clock())
	state, err := h.readBudgetState(
		ctx, projectLimitJoin, budgetScopeProject, strconv.FormatInt(projectID, 10), period, projectID,
	)
	if err != nil {
		return projectBudgetState{}, err
	}

	// The AUTHORED period and fail mode, read back rather than asserted. A
	// project nobody has configured has no row at all, and that is the default
	// state — defaultBudgetPeriod with an inherited fail mode — not a failure.
	resolved := projectBudgetState{
		ProjectID:    projectID,
		BudgetPeriod: defaultBudgetPeriod,
		budgetState:  state,
	}
	var (
		storedPeriod   *string
		storedFailMode *string
	)
	switch err := h.pool.QueryRow(ctx, projectPolicySelect, projectID).
		Scan(&storedPeriod, &storedFailMode); {
	case err == nil:
		if storedPeriod != nil && *storedPeriod != "" {
			resolved.BudgetPeriod = *storedPeriod
		}
		resolved.NatsFailMode = storedFailMode
	case errors.Is(err, pgx.ErrNoRows):
		// No authored row: the defaults above stand.
	default:
		return projectBudgetState{}, fmt.Errorf("budgets: read project budget policy: %w", err)
	}
	return resolved, nil
}

/* ── PUT the project's budget ──────────────────────────────────────────── */

// budgetWrite is project_budget.py's and user_budget.py's shared payload.
//
// MonthlyLimit is *json.Number so the exact decimal the operator typed reaches
// PostgreSQL as text and is cast to NUMERIC there; an absent field and an
// explicit null both mean "no ceiling", as they do in the reference.
type budgetWrite struct {
	MonthlyLimit *json.Number `json:"monthly_limit"`
	Enabled      *bool        `json:"enabled"`
	Currency     *string      `json:"currency"`
	SoftAlertPct *int         `json:"soft_alert_pct"`

	// The two project-only policy fields. BudgetPeriod is *string because an
	// absent field means "leave the stored period alone"; NatsFailMode is
	// json.RawMessage because it has THREE inputs, not two — absent leaves the
	// stored mode alone, an explicit null clears it back to the platform
	// baseline, and a string sets it. A *string cannot tell the first two
	// apart, and collapsing them would make every limit edit silently reset a
	// fail mode the operator chose earlier.
	BudgetPeriod *string         `json:"budget_period"`
	NatsFailMode json.RawMessage `json:"nats_fail_mode"`
}

// parsedBudgetWrite is a validated payload, with the defaults the reference
// applies already resolved.
type parsedBudgetWrite struct {
	monthlyLimit *string
	enabled      bool
	softAlertPct *int

	// budgetPeriod is nil when the field was absent, which the upsert reads as
	// "keep what is stored".
	budgetPeriod *string
	// natsFailModeSet says the field was PRESENT. natsFailMode is then the
	// value, or nil for an explicit null (inherit the platform baseline).
	natsFailModeSet bool
	natsFailMode    *string
}

// projectPolicyRequested reports whether the payload carried either of the two
// project-only policy fields. The per-member PUT refuses such a payload rather
// than accepting and dropping it: gateway.user_budget has neither column, and
// the gateway reads the fail mode from the OWNING PROJECT's row
// (services/elitea-llm-gateway/internal/failmode/store.go). A 200 over a
// silently discarded field is the shape this repo has shipped before (#128).
func (p parsedBudgetWrite) projectPolicyRequested() bool {
	return p.budgetPeriod != nil || p.natsFailModeSet
}

// defaultBudgetPeriod is gateway.project_budget.budget_period's column default
// and the only period anything enforces.
const defaultBudgetPeriod = "monthly"

// budgetPeriods is the set the write path accepts.
//
// It holds ONE value, and that is a statement about enforcement rather than an
// oversight. The gateway derives the billing window from the calendar month
// unconditionally (llmproxy/budget_gate.go billingPeriodStart/End), and the
// accumulator's unique key is (scope, scope_id, period_start), so a stored
// 'weekly' would be a period nothing computes and nothing bills against — a
// setting on a screen that changes nothing, which is the defect class this
// change exists to remove rather than to add another instance of.
//
// The column carries no CHECK constraint (shared migration 0067 declares only
// `VARCHAR(16) NOT NULL DEFAULT 'monthly'`), so this set is the only thing
// standing between the API and an unenforceable value. The day the gateway
// grows a second window, this set and the gateway's period helper change
// together, and the spec enum below with them.
var budgetPeriods = map[string]struct{}{
	defaultBudgetPeriod: {},
}

// natsFailModes is shared migration 0067's CHECK constraint on
// gateway.project_budget.nats_fail_mode, restated so a bad value is a 400 from
// this handler rather than a 23514 from PostgreSQL surfaced as a 500.
//
// It is also failmode.ModeTieredHybrid / ModeFailOpen / ModeFailClosed in
// services/elitea-llm-gateway/internal/failmode/fsm.go, which is what
// ResolveFailMode matches a per-project override against. A value outside the
// set is not merely refused by the column: it would be ignored by the resolver,
// so the project would silently fall back to the platform baseline.
var natsFailModes = map[string]struct{}{
	"tiered_hybrid": {},
	"fail_open":     {},
	"fail_closed":   {},
}

// sortedKeys renders a value set for an error message, in a stable order so the
// same rejection reads the same way twice.
func sortedKeys(set map[string]struct{}) string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

// decodeBudgetWrite validates a budget payload, answering the request itself on
// rejection.
func decodeBudgetWrite(w http.ResponseWriter, r *http.Request) (parsedBudgetWrite, bool) {
	var body budgetWrite
	decoder := json.NewDecoder(r.Body)
	decoder.UseNumber()
	if err := decoder.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return parsedBudgetWrite{}, false
	}

	parsed := parsedBudgetWrite{enabled: true}
	if body.Enabled != nil {
		parsed.enabled = *body.Enabled
	}
	if body.MonthlyLimit != nil {
		limit, err := body.MonthlyLimit.Float64()
		if err != nil {
			writeError(w, http.StatusBadRequest, "monthly_limit must be a number")
			return parsedBudgetWrite{}, false
		}
		if limit < 0 {
			writeError(w, http.StatusBadRequest, "monthly_limit must be >= 0")
			return parsedBudgetWrite{}, false
		}
		// The float parse is a RANGE check only. What is stored is the original
		// decimal text, cast to NUMERIC by PostgreSQL, so the float never
		// touches the value that gets enforced.
		text := body.MonthlyLimit.String()
		parsed.monthlyLimit = &text
	}
	// A currency other than USD is refused rather than stored: nothing in the
	// money path converts, so an accepted "EUR" would be a label on a limit the
	// gateway still enforces as dollars.
	if body.Currency != nil && !strings.EqualFold(*body.Currency, defaultCurrency) {
		writeError(w, http.StatusBadRequest, "currency must be USD")
		return parsedBudgetWrite{}, false
	}
	if body.SoftAlertPct != nil {
		if *body.SoftAlertPct < 1 || *body.SoftAlertPct > 100 {
			writeError(w, http.StatusBadRequest, "soft_alert_pct must be between 1 and 100")
			return parsedBudgetWrite{}, false
		}
		parsed.softAlertPct = body.SoftAlertPct
	}
	if body.BudgetPeriod != nil {
		period := strings.ToLower(strings.TrimSpace(*body.BudgetPeriod))
		if _, ok := budgetPeriods[period]; !ok {
			writeError(w, http.StatusBadRequest,
				"budget_period must be one of: "+sortedKeys(budgetPeriods))
			return parsedBudgetWrite{}, false
		}
		parsed.budgetPeriod = &period
	}
	if body.NatsFailMode != nil {
		parsed.natsFailModeSet = true
		// `null` clears the override back to the platform baseline. It is
		// decoded here rather than being caught by a *string, because a
		// *string cannot separate `null` from an absent key and the two mean
		// different things on this field.
		if string(body.NatsFailMode) != "null" {
			var mode string
			if err := json.Unmarshal(body.NatsFailMode, &mode); err != nil {
				writeError(w, http.StatusBadRequest, "nats_fail_mode must be a string or null")
				return parsedBudgetWrite{}, false
			}
			mode = strings.ToLower(strings.TrimSpace(mode))
			if _, ok := natsFailModes[mode]; !ok {
				writeError(w, http.StatusBadRequest,
					"nats_fail_mode must be null or one of: "+sortedKeys(natsFailModes))
				return parsedBudgetWrite{}, false
			}
			parsed.natsFailMode = &mode
		}
	}
	return parsed, true
}

// projectBudgetUpsert writes the authored limit AND the derived enforcement
// flag in one statement.
//
// An INSERT that supplies no threshold now stores NULL rather than 80, so the
// project inherits whatever platform default is in force at read time (#322).
// The UPDATE branch still COALESCEs to the EXISTING value, not to NULL: a PUT
// that changes only the limit must not silently move a threshold the operator
// chose earlier.
//
// `is_unlimited` is the column the gateway reads; `enabled` and
// `hard_limit_usd` are what the operator authored. Deriving one from the other
// in SQL, in the same statement, is what stops them from drifting — a second
// statement that set is_unlimited separately could be interrupted between the
// two and leave a project with a limit nothing enforces.
// `budget_period` and `nats_fail_mode` are supplied by the caller now rather
// than hard-coded and omitted. Before this, the INSERT wrote the literal
// 'monthly' and never mentioned nats_fail_mode at all, so the only column the
// gateway consults for its per-project failure policy had NO writer anywhere in
// the platform — the value was reachable by direct SQL and by nothing else.
//
// Both follow the soft_alert_pct precedent on the UPDATE branch: an absent
// field COALESCEs to the EXISTING value. A PUT that changes only the limit must
// not silently move a policy the operator chose earlier. `nats_fail_mode`
// additionally has to honour an EXPLICIT null — "inherit the platform
// baseline" — which COALESCE alone cannot express, so $7 carries "the field was
// present" and the CASE reads it.
const projectBudgetUpsert = `
INSERT INTO gateway.project_budget AS existing
    (project_id, hard_limit_usd, enabled, is_unlimited, soft_alert_pct,
     budget_period, nats_fail_mode, updated_at)
VALUES ($1, $2::numeric, $3, ($2::numeric IS NULL OR NOT $3), $4::smallint,
        COALESCE($5::varchar, '` + defaultBudgetPeriod + `'), $6::varchar, now())
ON CONFLICT (project_id) DO UPDATE SET
    hard_limit_usd = EXCLUDED.hard_limit_usd,
    enabled        = EXCLUDED.enabled,
    is_unlimited   = EXCLUDED.is_unlimited,
    soft_alert_pct = COALESCE($4::smallint, existing.soft_alert_pct),
    budget_period  = COALESCE($5::varchar, existing.budget_period),
    nats_fail_mode = CASE WHEN $7::boolean THEN $6::varchar ELSE existing.nats_fail_mode END,
    updated_at     = now()`

// projectBudgetDelete clears a project back to the platform default by removing
// the authored row. Every read in this package LEFT JOINs from a one-row anchor,
// so an absent row is already the "unlimited, inherit everything" state — which
// is why clearing is a DELETE rather than an UPDATE that writes NULLs into a row
// that then still claims to have been authored.
//
// It does NOT touch gateway.llm_budget_accumulators. That table's
// budget_rule_id carries ON DELETE CASCADE onto this row, but nothing in the
// platform has ever written budget_rule_id — it is NULL on every accumulator
// the scheduler's write-back consumer produces — so the cascade reaches nothing
// and the period's spend survives a clear. TestClearingAProjectBudgetKeepsTheSpend
// pins that, because the day something starts populating budget_rule_id, this
// statement would begin deleting billing history as a side effect of an
// operator clearing a limit.
const projectBudgetDelete = `DELETE FROM gateway.project_budget WHERE project_id = $1`

// PutProjectBudget serves project_budget.py's administration-mode PUT.
//
// It answers with the resulting state rather than the request, which can differ
// from what was sent: a null limit or enabled=false leaves the project
// unlimited, so `effective_limit` and `limit_source` are the fields worth
// reading back.
func (h *Handler) PutProjectBudget(w http.ResponseWriter, r *http.Request) {
	projectID, ok := pathID(r, "projectID")
	if !ok {
		writeError(w, http.StatusBadRequest, "project id must be a positive integer")
		return
	}
	parsed, ok := decodeBudgetWrite(w, r)
	if !ok {
		return
	}

	ctx := r.Context()
	if _, err := h.pool.Exec(ctx, projectBudgetUpsert,
		projectID, parsed.monthlyLimit, parsed.enabled, parsed.softAlertPct,
		parsed.budgetPeriod, parsed.natsFailMode, parsed.natsFailModeSet,
	); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to save project budget")
		return
	}

	state, err := h.projectBudget(ctx, projectID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read project budget")
		return
	}
	writeJSON(w, http.StatusOK, state)
}

// DeleteProjectBudget clears a project's authored budget back to the platform
// default.
//
// Without it a budget could be created but never removed. `enabled: false` was
// the nearest thing available and it is not the same state: it stores
// "deliberately exempt", which is an authored decision that keeps the operator's
// old ceiling on the screen, while a cleared project has no authored row and
// inherits the platform's soft-alert threshold and fail mode as they change.
//
// It answers with the RESULTING state, like the PUT above, and for the same
// reason: after a clear the effective figures are the inherited ones, and a 204
// would leave the caller to guess them or issue a second read.
//
// Deleting a project that has no budget is a success, not a 404. The endpoint
// states an intent — "this project has no authored budget" — and that is
// already true; answering 404 would make a retry after a lost response look
// like a failure.
func (h *Handler) DeleteProjectBudget(w http.ResponseWriter, r *http.Request) {
	projectID, ok := pathID(r, "projectID")
	if !ok {
		writeError(w, http.StatusBadRequest, "project id must be a positive integer")
		return
	}

	ctx := r.Context()
	if _, err := h.pool.Exec(ctx, projectBudgetDelete, projectID); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to clear project budget")
		return
	}

	state, err := h.projectBudget(ctx, projectID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read project budget")
		return
	}
	writeJSON(w, http.StatusOK, state)
}

/* ── the admin listing ─────────────────────────────────────────────────── */

// projectBudgetRow is one row of Admin → Budgets.
type projectBudgetRow struct {
	ProjectID   int64  `json:"project_id"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	OwnerName   string `json:"owner_name"`
	OwnerEmail  string `json:"owner_email"`
	IsPersonal  bool   `json:"is_personal"`
	budgetState
}

type projectBudgetListing struct {
	Rows   []projectBudgetRow `json:"rows"`
	Total  int                `json:"total"`
	Counts map[string]int     `json:"counts"`
}

// personalProjectPredicate classifies a project as personal, the same LIKE the
// admin project listing uses (internal/api/v2/admin/projects.go).
const personalProjectPredicate = `(p.name LIKE 'project_user_%')`

// sortableBudgetColumns is project_budgets.py's SORTABLE_FIELDS, and the reason
// it is only two: limit, spend and utilisation are per-row derivations, so
// ordering by them would sort the page that was already fetched rather than the
// table. An unknown value falls back to name, as the reference's
// `_safe_sort_field` does — silently choosing a different column and reporting
// success is how a sortable header lies.
var sortableBudgetColumns = map[string]string{
	"name": "p.name",
	"id":   "p.id",
}

// projectBudgetPageSQL joins each project on its authored budget and its
// current-period accumulator, so the whole page is one query rather than the
// reference's three fan-out RPCs per page.
const projectBudgetPageSQL = `
SELECT p.id,
       p.name,
       ` + personalProjectPredicate + `                              AS is_personal,
       COALESCE(owner.name, '')                                      AS owner_name,
       COALESCE(owner.email, '')                                     AS owner_email,
       limits.hard_limit_usd::text                                   AS monthly_limit,
       COALESCE(limits.enabled, false)                               AS enabled,
       (NOT COALESCE(limits.is_unlimited, true))                     AS enforced,
       COALESCE(limits.soft_alert_pct, ` + globalWarningPctSQL + `, $1::smallint) AS warning_pct,
       COALESCE(accrued.accumulated_cost, 0)::text                   AS spend,
       (accrued.accumulated_cost IS NOT NULL)                        AS spend_available,
       CASE WHEN NOT COALESCE(limits.is_unlimited, true) AND limits.hard_limit_usd IS NOT NULL
            THEN GREATEST(0, limits.hard_limit_usd - COALESCE(accrued.accumulated_cost, 0))::text
       END                                                           AS remaining,
       CASE WHEN NOT COALESCE(limits.is_unlimited, true) AND limits.hard_limit_usd > 0
            THEN round(COALESCE(accrued.accumulated_cost, 0) / limits.hard_limit_usd * 100, 2)::text
       END                                                           AS percent_used
FROM centry.project p
LEFT JOIN public.auth_core__user owner ON owner.id = p.owner_id
LEFT JOIN gateway.project_budget limits ON limits.project_id = p.id
LEFT JOIN gateway.llm_budget_accumulators accrued
       ON accrued.scope = $2
      AND accrued.scope_id = p.id::text
      AND accrued.period_start = $3::timestamptz`

// ListProjectBudgets serves project_budgets.py's administration-mode listing.
func (h *Handler) ListProjectBudgets(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	limit := pageSize(query.Get("limit"), 20)
	offset := positiveQueryInt(query.Get("offset"), 0)

	listing, err := h.listProjectBudgets(r.Context(), projectBudgetListParams{
		limit:       limit,
		offset:      offset,
		search:      strings.TrimSpace(query.Get("search")),
		projectType: query.Get("project_type"),
		sortBy:      query.Get("sort_by"),
		sortOrder:   query.Get("sort_order"),
	})
	if err != nil {
		// Reported as the failure it is. Swallowing it into an empty page
		// renders identically to "this deployment has no projects", which is
		// the shape #130's post-mortem named as worse than an error.
		writeError(w, http.StatusInternalServerError, "failed to list project budgets")
		return
	}
	writeJSON(w, http.StatusOK, listing)
}

type projectBudgetListParams struct {
	limit       int
	offset      int
	search      string
	projectType string
	sortBy      string
	sortOrder   string
}

func (h *Handler) listProjectBudgets(
	ctx context.Context, params projectBudgetListParams,
) (*projectBudgetListing, error) {
	period := periodFor(h.clock())

	counts, err := h.projectCounts(ctx)
	if err != nil {
		return nil, err
	}

	// The same filter rendered against two different leading-argument counts:
	// the COUNT takes none, the page takes three before the filter's own. A
	// single rendering shared between them would reference a $n the count query
	// never binds.
	countWhere, countArgs := projectBudgetFilters(params, 1)
	where, filterArgs := projectBudgetFilters(params, 4)

	var total int
	if err := h.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM centry.project p`+countWhere, countArgs...,
	).Scan(&total); err != nil {
		return nil, fmt.Errorf("budgets: count filtered projects: %w", err)
	}

	sortColumn, ok := sortableBudgetColumns[params.sortBy]
	if !ok {
		sortColumn = sortableBudgetColumns["name"]
	}
	direction := "ASC"
	if strings.EqualFold(params.sortOrder, "desc") {
		direction = "DESC"
	}

	// The `p.id` tiebreaker is not decoration: personal projects are named
	// project_user_<n> and team names are not unique, so ORDER BY name alone is
	// not a total order and rows repeat across pages.
	args := append([]any{DefaultWarningPct, budgetScopeProject, period.start}, filterArgs...)
	limitPlaceholder := "$" + strconv.Itoa(len(args)+1)
	offsetPlaceholder := "$" + strconv.Itoa(len(args)+2)
	args = append(args, params.limit, params.offset)

	rows, err := h.pool.Query(ctx, projectBudgetPageSQL+where+`
ORDER BY `+sortColumn+` `+direction+` NULLS LAST, p.id `+direction+`
LIMIT `+limitPlaceholder+` OFFSET `+offsetPlaceholder, args...)
	if err != nil {
		return nil, fmt.Errorf("budgets: list project budgets: %w", err)
	}
	defer rows.Close()

	page := make([]projectBudgetRow, 0, params.limit)
	for rows.Next() {
		var (
			row            projectBudgetRow
			monthlyLimit   *string
			enabled        bool
			enforced       bool
			warningPct     int
			spend          string
			spendAvailable bool
			remaining      *string
			percentUsed    *string
		)
		if err := rows.Scan(
			&row.ProjectID, &row.Name, &row.IsPersonal, &row.OwnerName, &row.OwnerEmail,
			&monthlyLimit, &enabled, &enforced, &warningPct, &spend, &spendAvailable, &remaining, &percentUsed,
		); err != nil {
			return nil, fmt.Errorf("budgets: scan project budget row: %w", err)
		}
		row.budgetState = budgetState{
			MonthlyLimit:   numeric(monthlyLimit),
			Currency:       defaultCurrency,
			Enabled:        enabled,
			WarningPct:     warningPct,
			Spend:          numeric(&spend),
			Remaining:      numeric(remaining),
			PercentUsed:    numeric(percentUsed),
			SpendAvailable: spendAvailable,
			Period:         period.label(),
			PeriodStart:    period.firstDay(),
			PeriodEnd:      period.lastDay(),
			ResetsAt:       period.resetsAt(),
			LimitSource:    limitSourceUnlimited,
		}
		if enforced && monthlyLimit != nil {
			row.EffectiveLimit = row.MonthlyLimit
			row.LimitSource = limitSourceExplicit
		}
		// A personal project is really its owner's own budget, so it is
		// labelled by identity rather than by the opaque project_user_N name
		// nobody searches for.
		row.DisplayName = row.Name
		if row.IsPersonal {
			if label := firstNonEmpty(row.OwnerEmail, row.OwnerName); label != "" {
				row.DisplayName = label
			}
		}
		if row.OwnerName == "" {
			row.OwnerName = row.OwnerEmail
		}
		page = append(page, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("budgets: read project budget page: %w", err)
	}
	return &projectBudgetListing{Rows: page, Total: total, Counts: counts}, nil
}

// projectCounts labels the two tabs and is deliberately not narrowed by the
// filters: a tab whose count moved with the search box would be reporting how
// many rows the OTHER tab currently shows.
func (h *Handler) projectCounts(ctx context.Context) (map[string]int, error) {
	var all, personal int
	if err := h.pool.QueryRow(ctx, `
SELECT COUNT(*), COUNT(*) FILTER (WHERE `+personalProjectPredicate+`)
FROM centry.project p`).Scan(&all, &personal); err != nil {
		return nil, fmt.Errorf("budgets: count projects: %w", err)
	}
	return map[string]int{"team": all - personal, "personal": personal}, nil
}

// projectBudgetFilters renders project_type and search as SQL. firstPlaceholder
// is the $n the caller has already consumed, so the fragment can be appended to
// two different queries with different leading argument counts.
//
// The search matches the project name, the id as text, and the owner's name or
// email — the reference resolves the third with a separate auth_search_users
// RPC and ORs the ids in, but only for personal projects; asking it in SQL
// costs nothing and makes a team project findable by its owner too.
func projectBudgetFilters(params projectBudgetListParams, firstPlaceholder int) (string, []any) {
	conditions := make([]string, 0, 2)
	args := make([]any, 0, 1)
	switch params.projectType {
	case "personal":
		conditions = append(conditions, personalProjectPredicate)
	case "team":
		conditions = append(conditions, "NOT "+personalProjectPredicate)
	}
	if params.search != "" {
		args = append(args, "%"+params.search+"%")
		placeholder := "$" + strconv.Itoa(firstPlaceholder)
		conditions = append(conditions, fmt.Sprintf(`(
    p.name ILIKE %[1]s
 OR p.id::text ILIKE %[1]s
 OR EXISTS (
        SELECT 1 FROM public.auth_core__user owner
        WHERE owner.id = p.owner_id
          AND (owner.name ILIKE %[1]s OR owner.email ILIKE %[1]s)
    )
)`, placeholder))
	}
	if len(conditions) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(conditions, " AND "), args
}

// maxPageSize bounds `?limit=`, matching the cap the admin project listing this
// query was modelled on already applies (internal/api/v2/admin/handler.go).
//
// It is not politeness: `limit` is also the capacity the result slice is
// preallocated with, so an unbounded value is an unbounded allocation made
// BEFORE a single row is read — `?limit=100000000` reserves tens of gigabytes
// on a deployment with three projects.
const maxPageSize = 100

// pageSize clamps a caller-supplied page size into [0, maxPageSize], falling
// back for anything unparseable or negative.
func pageSize(raw string, fallback int) int {
	value := positiveQueryInt(raw, fallback)
	if value > maxPageSize {
		return maxPageSize
	}
	return value
}

func positiveQueryInt(raw string, fallback int) int {
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 {
		return fallback
	}
	return value
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
