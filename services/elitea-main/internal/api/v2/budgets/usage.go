package budgets

// Usage.
//
//	GET /elitea_core/usage/prompt_lib/{project_id}/usage?scope=project|user
//	                                                       ← elitea_core/api/v2/usage.py
//
// Backs Settings → Usage: the current period's spend against whichever budget
// applies, for the whole project or for the calling member alone.
//
// # Amount redaction
//
// A member who is neither a project admin nor the owner of the personal project
// being reported on does not see platform cost figures. They receive the SAME
// payload with the money fields removed and `can_see_amounts: false`;
// percentages, thresholds and the period are still there, so the usage bar
// still renders. This is the reference's rule, kept because it is the one the
// deployment's users have been operating under.
//
// The removal happens on the assembled payload rather than by never fetching
// the numbers, exactly as the reference does — one code path produces the
// figures, one step decides who sees them, so the two cannot disagree about
// what a redacted field is.
//
// # The dimensional views
//
// The per-day series, the per-model table and the token totals come from
// gateway.llm_usage_events (issue #320) and are assembled in
// usage_dimensions.go. They are ABSENT, not empty arrays, when the ledger holds
// nothing for the scope — see the package doc for why an empty array is a
// different and wrong claim.
//
// The per-day and per-model SPEND figures are money, so they are redacted for a
// caller who may not see amounts. The token and request counts are not: a
// member is allowed to know how much they used, on the same principle that
// leaves them the percentage and the period.

import (
	"context"
	"fmt"
	"net/http"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

const (
	usageScopeProject = "project"
	usageScopeUser    = "user"
)

// amountFields are the cost figures stripped from a redacted payload. The
// percentage and the token-free period metadata survive: a member is allowed to
// know they are near the limit, not what the limit costs.
var amountFields = []string{"monthly_limit", "effective_limit", "spend", "remaining", "currency"}

// dimensionListFields are the payload keys holding a list of rows that each
// carry their own `spend`. Redaction has to reach INSIDE them: stripping the
// top-level spend while leaving a per-day series of the same money would make
// the control decorative, which is the reason canSeeAmounts is shared with the
// project budget read in the first place.
var dimensionListFields = []string{"daily", "models"}

// GetUsage serves usage.py's prompt_lib GET.
func (h *Handler) GetUsage(w http.ResponseWriter, r *http.Request) {
	projectID, ok := pathID(r, "projectID")
	if !ok {
		writeError(w, http.StatusBadRequest, "project id must be a positive integer")
		return
	}

	scope := r.URL.Query().Get("scope")
	if scope == "" {
		scope = usageScopeProject
	}
	if scope != usageScopeProject && scope != usageScopeUser {
		writeError(w, http.StatusBadRequest, "scope must be 'project' or 'user'")
		return
	}

	caller, authenticated := callerID(r)
	if !authenticated {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}

	ctx := r.Context()
	payload, err := h.usagePayload(w, r, projectID, caller, scope)
	if err != nil {
		return
	}

	// The dimensional block is scoped the same way the meter is: the project
	// scope reports the whole project's calls, the user scope reports the
	// caller's own. The member filter uses the CALLER's id, not a path
	// parameter, so this endpoint cannot be pointed at a colleague.
	var memberFilter *int64
	if scope == usageScopeUser {
		memberFilter = &caller
	}
	dims, err := h.readUsageDimensions(ctx, projectID, memberFilter, periodFor(h.clock()))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read usage")
		return
	}
	if err := mergeDimensions(payload, dims); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read usage")
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

// canSeeAmounts decides whether this caller may see platform cost figures for
// this project: project admins may, and so does the owner of a personal
// project, whose spend is their own and where token-based integrations land.
//
// It is shared with the project-scoped budget read rather than living in this
// file, because the two endpoints serve the SAME numbers behind the SAME gate.
// While only /usage redacted, a member refused the amounts here could read them
// straight off /project_budget/prompt_lib/{id}/budget and the control was
// decorative. The rule itself is repos.AmountVisibility, which the model
// catalogue's prices ask too, for the same reason.
func (h *Handler) canSeeAmounts(ctx context.Context, projectID, userID int64) (bool, error) {
	visible, err := repos.NewAmountVisibility(h.pool).CanSeeAmounts(ctx, projectID, userID)
	if err != nil {
		return false, fmt.Errorf("budgets: %w", err)
	}
	return visible, nil
}

// applyAmountVisibility stamps can_see_amounts and strips the cost fields when
// it is false.
func applyAmountVisibility(payload map[string]any, visible bool) {
	payload["can_see_amounts"] = visible
	if !visible {
		redactAmounts(payload)
	}
}

// usagePayload assembles the scope's budget state as a mutable map, so
// redaction is a removal rather than a second, parallel struct that could
// disagree with the first about which fields are money.
//
// It answers the request itself on failure and returns the error so the caller
// stops; every response this package writes is written exactly once.
func (h *Handler) usagePayload(
	w http.ResponseWriter, r *http.Request, projectID, userID int64, scope string,
) (map[string]any, error) {
	ctx := r.Context()
	if scope == usageScopeUser {
		state, err := h.userBudget(ctx, projectID, userID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to read usage")
			return nil, err
		}
		payload, err := structToMap(state)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to read usage")
			return nil, err
		}
		payload["scope"] = usageScopeUser
		return payload, nil
	}

	state, err := h.projectBudget(ctx, projectID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read usage")
		return nil, err
	}
	payload, err := structToMap(state)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read usage")
		return nil, err
	}
	payload["scope"] = usageScopeProject
	// The project scope has no user of its own; the reference reports null
	// rather than omitting the key, so a client can key off one shape.
	payload["user_id"] = nil
	return payload, nil
}

func redactAmounts(payload map[string]any) {
	for _, field := range amountFields {
		delete(payload, field)
	}
	for _, field := range dimensionListFields {
		rows, ok := payload[field].([]any)
		if !ok {
			continue
		}
		for _, row := range rows {
			if entry, ok := row.(map[string]any); ok {
				delete(entry, "spend")
			}
		}
	}
}

// mergeDimensions folds the dimensional block into the assembled payload as
// top-level keys, so the response keeps ONE shape rather than growing a nested
// object the reference never had.
//
// It goes through the block's own JSON tags for the same reason structToMap
// does: the redaction step below addresses these fields by name, and a payload
// whose keys were spelled by hand here and by tag there would eventually
// disagree about which of them is money.
func mergeDimensions(payload map[string]any, dims usageDimensions) error {
	encoded, err := structToMap(dims)
	if err != nil {
		return err
	}
	for key, value := range encoded {
		payload[key] = value
	}
	return nil
}
