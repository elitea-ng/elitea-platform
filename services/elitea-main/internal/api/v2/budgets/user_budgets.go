package budgets

// Per-member budgets.
//
//	GET  /elitea_core/user_budget/{mode}/{project_id}/user_budget/{user_id}  ← user_budget.py
//	PUT  /elitea_core/user_budget/administration/{project_id}/user_budget/{user_id}
//	GET  /elitea_core/user_budgets/{mode}/{project_id}                       ← user_budgets.py
//
// # These limits ARE enforced (issue #321)
//
// They were not, for as long as the gateway's admission check knew a single
// scope. A project admin could set a member cap, get a 200, watch the value
// round-trip, and that member could spend the whole project budget. Every read
// carried `"enforced": false` to say so, which was honest but was still a
// control that did nothing.
//
// The gateway now admits and bills a second scope keyed (project, member):
// llmproxy/budget_gate.go asks gateway.user_budget after the project ceiling
// admits, refuses an over-cap member with 402 `member_budget_exceeded`, and
// bills the member accumulator alongside the project one under its own event
// id. `enforced` is therefore true.
//
// It is still a FIELD rather than a constant in a doc comment, and
// TestUserBudgetReportsThatItIsEnforced still pins it, so the day per-member
// enforcement is removed or bypassed the payload has to be changed
// deliberately — the property #135 records as the one a doc comment loses.

import (
	"context"
	"fmt"
	"net/http"
)

// userLimitJoin is readBudgetState's per-member limit table. $5/$6 are the
// project and user ids; see projectLimitJoin for the project counterpart and
// for why both joins must expose `is_unlimited`.
//
// gateway.user_budget has no is_unlimited column, so it is derived here with
// the same rule the project upsert applies — and, since #321, with the same
// rule the gateway's own per-member snapshot derives it with
// (failmode/store.go userSnapshotSQL). The two must not drift: this expression
// is what the API reports as `enforced`, and that one is what actually refuses
// the call.
const userLimitJoin = `LEFT JOIN (
    SELECT project_id, user_id, hard_limit_usd, enabled, soft_alert_pct,
           (hard_limit_usd IS NULL OR NOT enabled) AS is_unlimited
    FROM gateway.user_budget
) AS limits ON limits.project_id = $5::integer AND limits.user_id = $6::integer`

// userBudgetState is one member's budget within a project.
type userBudgetState struct {
	ProjectID int64 `json:"project_id"`
	UserID    int64 `json:"user_id"`
	// Enforced reports whether the gateway holds calls to this cap. It is a
	// field rather than a comment so a client renders what is true of the
	// deployment it is talking to, not what was true when the client shipped.
	Enforced bool `json:"enforced"`
	budgetState
}

// userBudgetRow is one row of the members listing.
type userBudgetRow struct {
	ProjectID int64    `json:"project_id"`
	UserID    int64    `json:"user_id"`
	Name      string   `json:"name"`
	Email     string   `json:"email"`
	Roles     []string `json:"roles"`
	Enforced  bool     `json:"enforced"`
	budgetState
}

type userBudgetListing struct {
	Rows       []userBudgetRow `json:"rows"`
	Total      int             `json:"total"`
	WarningPct int             `json:"warning_pct"`
}

/* ── GET one member's budget ───────────────────────────────────────────── */

// GetUserBudget serves the PROJECT-SCOPED (prompt_lib) read.
//
// The gate router.go applies to it is a project-membership permission, so it
// admits every member of the project — which is not the same as "may read this
// row". A member may read only their OWN, unless they are an admin of the
// project; without this check any member could read a colleague's spend by
// editing the URL.
//
// The mode is fixed by WHICH HANDLER is registered, not read from a `{mode}`
// path parameter: a handler that trusted the URL would let any member ask for
// `administration` and skip the check outright.
func (h *Handler) GetUserBudget(w http.ResponseWriter, r *http.Request) {
	projectID, userID, ok := h.memberPath(w, r)
	if !ok {
		return
	}

	caller, authenticated := callerID(r)
	if !authenticated {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if caller != userID {
		admin, err := h.isProjectAdmin(r.Context(), projectID, caller)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to resolve project role")
			return
		}
		if !admin {
			writeError(w, http.StatusForbidden, "Forbidden")
			return
		}
	}

	h.writeUserBudget(w, r, projectID, userID)
}

// GetUserBudgetAdmin serves the administration-mode read, which is centrally
// gated on `models.admin.project_budgets.view` and therefore carries no
// membership check — a platform administrator is not a member of the projects
// they administer.
func (h *Handler) GetUserBudgetAdmin(w http.ResponseWriter, r *http.Request) {
	projectID, userID, ok := h.memberPath(w, r)
	if !ok {
		return
	}
	h.writeUserBudget(w, r, projectID, userID)
}

func (h *Handler) memberPath(w http.ResponseWriter, r *http.Request) (int64, int64, bool) {
	projectID, ok := pathID(r, "projectID")
	if !ok {
		writeError(w, http.StatusBadRequest, "project id must be a positive integer")
		return 0, 0, false
	}
	userID, ok := pathID(r, "userID")
	if !ok {
		writeError(w, http.StatusBadRequest, "user id must be a positive integer")
		return 0, 0, false
	}
	return projectID, userID, true
}

func (h *Handler) writeUserBudget(w http.ResponseWriter, r *http.Request, projectID, userID int64) {
	state, err := h.userBudget(r.Context(), projectID, userID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read member budget")
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (h *Handler) userBudget(ctx context.Context, projectID, userID int64) (userBudgetState, error) {
	period := periodFor(h.clock())
	state, err := h.readBudgetState(
		ctx, userLimitJoin, budgetScopeUser, userBudgetScopeID(projectID, userID), period,
		projectID, userID,
	)
	if err != nil {
		return userBudgetState{}, err
	}
	return userBudgetState{
		ProjectID: projectID,
		UserID:    userID,
		// True since #321: llmproxy/budget_gate.go checkMemberBudget reads this
		// exact row on every /llm call that carries a member id.
		Enforced:    true,
		budgetState: state,
	}, nil
}

/* ── PUT one member's budget ───────────────────────────────────────────── */

// userBudgetUpsert mirrors projectBudgetUpsert minus is_unlimited: the gateway
// derives that flag from (hard_limit_usd IS NULL OR NOT enabled) when it reads
// the row, so there is nothing to store and nothing that can go stale against
// the two columns it is derived from.
const userBudgetUpsert = `
INSERT INTO gateway.user_budget AS existing
    (project_id, user_id, hard_limit_usd, enabled, soft_alert_pct, updated_at)
VALUES ($1, $2, $3::numeric, $4, $5::smallint, now())
ON CONFLICT (project_id, user_id) DO UPDATE SET
    hard_limit_usd = EXCLUDED.hard_limit_usd,
    enabled        = EXCLUDED.enabled,
    soft_alert_pct = COALESCE($5::smallint, existing.soft_alert_pct),
    updated_at     = now()`

// PutUserBudget serves user_budget.py's administration-mode PUT.
func (h *Handler) PutUserBudget(w http.ResponseWriter, r *http.Request) {
	projectID, ok := pathID(r, "projectID")
	if !ok {
		writeError(w, http.StatusBadRequest, "project id must be a positive integer")
		return
	}
	userID, ok := pathID(r, "userID")
	if !ok {
		writeError(w, http.StatusBadRequest, "user id must be a positive integer")
		return
	}
	parsed, ok := decodeBudgetWrite(w, r)
	if !ok {
		return
	}
	// The two project-only policy fields are REFUSED here, not ignored.
	//
	// gateway.user_budget has neither column (shared migration 0067), and the
	// gateway reads a project's fail mode from the OWNING PROJECT's row on the
	// member path too (failmode/store.go: "taken from the OWNING PROJECT's
	// row. A member cap sits inside it"). A member accrues over the same
	// calendar window as the project for the same reason. So there is nothing
	// here for either field to mean, and accepting one would answer 200 to a
	// request whose whole content was discarded.
	if parsed.projectPolicyRequested() {
		writeError(w, http.StatusBadRequest,
			"budget_period and nats_fail_mode are project-scoped; set them on the project budget")
		return
	}

	ctx := r.Context()
	if _, err := h.pool.Exec(ctx, userBudgetUpsert,
		projectID, userID, parsed.monthlyLimit, parsed.enabled, parsed.softAlertPct,
	); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to save member budget")
		return
	}

	state, err := h.userBudget(ctx, projectID, userID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read member budget")
		return
	}
	writeJSON(w, http.StatusOK, state)
}

// userBudgetDelete clears one member's authored cap. See projectBudgetDelete
// for why clearing is a DELETE: the per-member read LEFT JOINs from the same
// one-row anchor, so an absent row already IS "this member has no cap of their
// own and is bounded by the project ceiling alone".
const userBudgetDelete = `DELETE FROM gateway.user_budget WHERE project_id = $1 AND user_id = $2`

// DeleteUserBudget clears one member's budget back to the project default.
//
// It matters more here than at project scope, because a member cap is ENFORCED
// since #321: the gateway refuses an over-cap member with 402
// `member_budget_exceeded`. Before this route the only way to lift a cap set by
// mistake was `enabled: false`, which stores a different fact — "this member is
// deliberately exempt" — and leaves the wrong number visible beside it.
//
// Deleting a member who has no cap is a success, for the reason
// DeleteProjectBudget gives.
func (h *Handler) DeleteUserBudget(w http.ResponseWriter, r *http.Request) {
	projectID, ok := pathID(r, "projectID")
	if !ok {
		writeError(w, http.StatusBadRequest, "project id must be a positive integer")
		return
	}
	userID, ok := pathID(r, "userID")
	if !ok {
		writeError(w, http.StatusBadRequest, "user id must be a positive integer")
		return
	}

	ctx := r.Context()
	if _, err := h.pool.Exec(ctx, userBudgetDelete, projectID, userID); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to clear member budget")
		return
	}

	state, err := h.userBudget(ctx, projectID, userID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read member budget")
		return
	}
	writeJSON(w, http.StatusOK, state)
}

/* ── the members listing ───────────────────────────────────────────────── */

// userBudgetPageSQL lists every member of a project with their authored limit
// and current-period spend.
//
// The member set and the role aggregation are the ones the project member
// listing already uses (internal/api/v2/eliteacore/handler.go): project role
// assignments, with each project's own service account excluded — the
// `filter_system_user=True` the reference passes.
//
// The roles aggregate is a GROUP BY inside a sub-select, not a JOIN that
// multiplies the member out once per role. A plain join here is what made the
// pre-A14 admin user listing report a two-role user twice while its separate
// COUNT disagreed.
//
// It is driven FROM the assignment table, not from auth_core__user: the member
// set is `assignment.project_id = $1`, so the planner starts from that index and
// touches only this project's members. Selecting from every user and filtering
// membership per row scales with the deployment's user count instead — ~15k
// rows scanned to return five, on the dump this was measured against.
const userBudgetPageSQL = `
WITH members AS (
    SELECT assignment.user_id,
           array_agg(DISTINCT project_role.name) AS names
    FROM public.auth_core__project_user_role AS assignment
    JOIN public.auth_core__project_role AS project_role
      ON project_role.id = assignment.role_id
     AND project_role.project_id = assignment.project_id
    WHERE assignment.project_id = $1
    GROUP BY assignment.user_id
)
SELECT member.id,
       COALESCE(member.name, '')                                     AS name,
       COALESCE(member.email, '')                                    AS email,
       members.names                                                 AS roles,
       limits.hard_limit_usd::text                                   AS monthly_limit,
       COALESCE(limits.enabled, false)                               AS enabled,
       COALESCE(limits.soft_alert_pct, ` + globalWarningPctSQL + `, $2::smallint) AS warning_pct,
       COALESCE(accrued.accumulated_cost, 0)::text                   AS spend,
       (accrued.accumulated_cost IS NOT NULL)                        AS spend_available,
       -- Enforcement, derived exactly as userLimitJoin derives it, so the
       -- listing and the single-member read cannot disagree about whether a
       -- ceiling applies. gateway.user_budget has no is_unlimited column, and
       -- the gateway derives it the same way on the admission path.
       CASE WHEN limits.enabled AND limits.hard_limit_usd IS NOT NULL
            THEN GREATEST(0, limits.hard_limit_usd - COALESCE(accrued.accumulated_cost, 0))::text
       END                                                           AS remaining,
       CASE WHEN limits.enabled AND limits.hard_limit_usd > 0
            THEN round(COALESCE(accrued.accumulated_cost, 0) / limits.hard_limit_usd * 100, 2)::text
       END                                                           AS percent_used,
       -- The threshold crossing, computed here as well as in
       -- budgetStateSelect. It is not optional: budgetState carries
       -- warning_active, so a listing that left it to the zero value would
       -- report false for every member who is over their threshold — the
       -- "absence reads as correctness" shape, in a field a banner branches on.
       COALESCE(
           CASE WHEN limits.enabled AND limits.hard_limit_usd > 0
                THEN round(COALESCE(accrued.accumulated_cost, 0) / limits.hard_limit_usd * 100, 2)
                     >= COALESCE(limits.soft_alert_pct, ` + globalWarningPctSQL + `, $2::smallint)
           END, false)                                               AS warning_active
FROM members
JOIN public.auth_core__user member ON member.id = members.user_id
LEFT JOIN gateway.user_budget limits
       ON limits.project_id = $1 AND limits.user_id = member.id
LEFT JOIN gateway.llm_budget_accumulators accrued
       ON accrued.scope = $3
      AND accrued.scope_id = $1::text || ':' || member.id::text
      AND accrued.period_start = $4::timestamptz
WHERE member.email NOT LIKE '%@centry.user'
ORDER BY name, member.id`

// ListUserBudgets serves the project-scoped members listing. It is restricted
// to admins OF THAT PROJECT: an ordinary member may see their own usage and not
// their colleagues', so the whole table is admin-only.
func (h *Handler) ListUserBudgets(w http.ResponseWriter, r *http.Request) {
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
	admin, err := h.isProjectAdmin(r.Context(), projectID, caller)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to resolve project role")
		return
	}
	if !admin {
		writeError(w, http.StatusForbidden, "Forbidden")
		return
	}

	h.writeUserBudgetListing(w, r, projectID)
}

// ListUserBudgetsAdmin serves the administration-mode members listing, gated
// centrally rather than on project membership.
func (h *Handler) ListUserBudgetsAdmin(w http.ResponseWriter, r *http.Request) {
	projectID, ok := pathID(r, "projectID")
	if !ok {
		writeError(w, http.StatusBadRequest, "project id must be a positive integer")
		return
	}
	h.writeUserBudgetListing(w, r, projectID)
}

func (h *Handler) writeUserBudgetListing(w http.ResponseWriter, r *http.Request, projectID int64) {
	listing, err := h.listUserBudgets(r.Context(), projectID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list member budgets")
		return
	}
	writeJSON(w, http.StatusOK, listing)
}

func (h *Handler) listUserBudgets(ctx context.Context, projectID int64) (*userBudgetListing, error) {
	period := periodFor(h.clock())

	rows, err := h.pool.Query(ctx, userBudgetPageSQL,
		projectID, DefaultWarningPct, budgetScopeUser, period.start)
	if err != nil {
		return nil, fmt.Errorf("budgets: list member budgets: %w", err)
	}
	defer rows.Close()

	page := make([]userBudgetRow, 0)
	for rows.Next() {
		var (
			row            userBudgetRow
			monthlyLimit   *string
			enabled        bool
			warningPct     int
			spend          string
			spendAvailable bool
			remaining      *string
			percentUsed    *string
			warningActive  bool
		)
		if err := rows.Scan(
			&row.UserID, &row.Name, &row.Email, &row.Roles,
			&monthlyLimit, &enabled, &warningPct, &spend, &spendAvailable, &remaining, &percentUsed,
			&warningActive,
		); err != nil {
			return nil, fmt.Errorf("budgets: scan member budget row: %w", err)
		}
		row.ProjectID = projectID
		// Same value the single-member read reports, for the same reason. The
		// two are set from one constant truth about the deployment; a listing
		// that disagreed with the detail view would be worse than either.
		row.Enforced = true
		if row.Name == "" {
			row.Name = row.Email
		}
		row.budgetState = budgetState{
			MonthlyLimit:   numeric(monthlyLimit),
			Currency:       defaultCurrency,
			Enabled:        enabled,
			WarningPct:     warningPct,
			Spend:          numeric(&spend),
			Remaining:      numeric(remaining),
			PercentUsed:    numeric(percentUsed),
			WarningActive:  warningActive,
			SpendAvailable: spendAvailable,
			Period:         period.label(),
			PeriodStart:    period.firstDay(),
			PeriodEnd:      period.lastDay(),
			ResetsAt:       period.resetsAt(),
			LimitSource:    limitSourceUnlimited,
		}
		if enabled && monthlyLimit != nil {
			row.EffectiveLimit = row.MonthlyLimit
			row.LimitSource = limitSourceExplicit
		}
		page = append(page, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("budgets: read member budget page: %w", err)
	}

	// The listing's own warning threshold is the PROJECT's, not a member's:
	// it labels the table, and each row already carries its own.
	warningPct, err := h.projectWarningPct(ctx, projectID)
	if err != nil {
		return nil, err
	}
	return &userBudgetListing{Rows: page, Total: len(page), WarningPct: warningPct}, nil
}

func (h *Handler) projectWarningPct(ctx context.Context, projectID int64) (int, error) {
	var pct int
	err := h.pool.QueryRow(ctx, `
SELECT COALESCE(
    (SELECT soft_alert_pct FROM gateway.project_budget WHERE project_id = $1),
    `+globalWarningPctSQL+`,
    $2::smallint
)`, projectID, DefaultWarningPct).Scan(&pct)
	if err != nil {
		return 0, fmt.Errorf("budgets: read project warning threshold: %w", err)
	}
	return pct, nil
}
