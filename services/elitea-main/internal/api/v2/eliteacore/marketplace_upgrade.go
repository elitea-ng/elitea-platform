package eliteacore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentrelease"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/marketplace"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// Upgrades of installed agents (ADR-0051 decision 4): the plan a person reads
// before moving to a version, the upgrade itself, and the policy that says
// whether the platform may move an agent by itself. A fork has none of this: it
// is updated through its upstream review (marketplace_upstream.go).

type planVersion struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	State        string `json:"state"`
	StateReason  string `json:"state_reason"`
	ReleaseNotes string `json:"release_notes"`
	PublishedAt  string `json:"published_at"`
}

type planFeedback struct {
	RatingAvg   *float64 `json:"rating_avg"`
	RatingCount int      `json:"rating_count"`
}

type upgradePlan struct {
	CurrentVersion  planVersion               `json:"current_version"`
	TargetVersion   planVersion               `json:"target_version"`
	Direction       string                    `json:"direction"`
	Changes         []agentrelease.Change     `json:"changes"`
	Widening        []marketplace.Widening    `json:"widening"`
	Requirements    marketplace.Requirements  `json:"requirements"`
	Bindings        []bindingJSON             `json:"bindings"`
	NewRequirements []marketplace.Requirement `json:"new_requirements"`
	NeedsApproval   bool                      `json:"needs_approval"`
	AutoEligible    bool                      `json:"auto_eligible"`
	Hireable        bool                      `json:"hireable"`
	Feedback        planFeedback              `json:"feedback"`
}

func toPlanVersion(v marketplace.Version) planVersion {
	state := v.State
	if v.Withdrawn && state == marketplace.VersionAvailable {
		state = marketplace.VersionYanked
	}
	return planVersion{
		ID: marketplace.IDText(v.ID), Name: v.Name, State: state, StateReason: v.StateReason,
		ReleaseNotes: v.ReleaseNotes, PublishedAt: v.PublishedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
}

// registryTier is the tier the agent holds in the registry; an agent with no
// row is low.
func registryTier(ctx context.Context, q relationQuerier, projectID, applicationID int64) string {
	var tier string
	if err := q.QueryRow(ctx, `
SELECT risk_tier FROM elitea_lifecycle.agent_registry WHERE tenant_project_id = $1 AND application_id = $2`,
		projectID, applicationID).Scan(&tier); err != nil || marketplace.TierRank(tier) < 0 {
		return "low"
	}
	return tier
}

// carriedBindings are the held bindings that still name a requirement of the
// target, then the ones the person chose, which win.
func carriedBindings(held []marketplace.Binding, target marketplace.Requirements, chosen []hireBindingBody) []hireBindingBody {
	in := map[string]bool{}
	for _, item := range target.Items {
		in[item.Key] = true
	}
	byKey := map[string]hireBindingBody{}
	var order []string
	for _, b := range held {
		if !in[b.RequirementKey] {
			continue
		}
		item := hireBindingBody{RequirementKey: b.RequirementKey, Disabled: b.Disabled}
		if b.ToolkitID > 0 {
			item.ToolkitID = strconv.FormatInt(b.ToolkitID, 10)
		}
		byKey[b.RequirementKey] = item
		order = append(order, b.RequirementKey)
	}
	for _, b := range chosen {
		if !in[b.RequirementKey] {
			continue
		}
		if _, had := byKey[b.RequirementKey]; !had {
			order = append(order, b.RequirementKey)
		}
		byKey[b.RequirementKey] = b
	}
	out := make([]hireBindingBody, 0, len(order))
	for _, key := range order {
		out = append(out, byKey[key])
	}
	return out
}

// unboundRequirements are the requirements of the target that need a person to
// bind them: a toolkit or MCP requirement with no usable binding yet.
func unboundRequirements(target marketplace.Requirements, carried []hireBindingBody) []marketplace.Requirement {
	bound := map[string]bool{}
	for _, b := range carried {
		if b.Disabled || b.ToolkitID != "" {
			bound[b.RequirementKey] = true
		}
	}
	out := []marketplace.Requirement{}
	for _, item := range target.Items {
		if (item.Kind == marketplace.RequirementToolkit || item.Kind == marketplace.RequirementMCP) && !bound[item.Key] {
			out = append(out, item)
		}
	}
	return out
}

func parseUpgradeIDs(w http.ResponseWriter, r *http.Request) (projectID, appID int64, schema string, ok bool) {
	projectIDText := chi.URLParam(r, "projectID")
	schema, ok = tenantSchema(w, projectIDText)
	if !ok {
		return 0, 0, "", false
	}
	projectID = parseProjectID(projectIDText)
	appID, err := strconv.ParseInt(chi.URLParam(r, "applicationID"), 10, 64)
	if err != nil || appID <= 0 || projectID <= 0 {
		hireError(w, http.StatusBadRequest, "invalid_request", "The project or application id is not valid.")
		return 0, 0, "", false
	}
	return projectID, appID, schema, true
}

// MarketplaceUpgradePlan answers GET
// /elitea_core/marketplace_upgrade/prompt_lib/{projectID}/{applicationID}?version_id=.
// version_id names the target; without it the plan is for the newest hireable
// version. The target may be older or on the same listing's other branch: a
// person can go up, down or across.
func (h *Handler) MarketplaceUpgradePlan(w http.ResponseWriter, r *http.Request) {
	if h.pool == nil {
		hireError(w, http.StatusServiceUnavailable, "unavailable", "The marketplace needs a database.")
		return
	}
	ctx := r.Context()
	projectID, appID, schema, ok := parseUpgradeIDs(w, r)
	if !ok {
		return
	}
	held, err := marketplace.LoadHeld(ctx, h.pool, projectID, appID, false)
	if errors.Is(err, marketplace.ErrNotFound) {
		hireError(w, http.StatusNotFound, "not_found", "The agent was not hired from the marketplace.")
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "marketplace upgrade plan: read the install failed", "application_id", appID, "error", err)
		hireError(w, http.StatusInternalServerError, "internal_error", "Failed to read the upgrade.")
		return
	}
	if held.Mode != marketplace.ModeInstall {
		hireError(w, http.StatusConflict, "fork_has_no_upgrade", "A fork takes upstream updates through its review, not an upgrade.")
		return
	}
	targetID, err := strconv.ParseInt(r.URL.Query().Get("version_id"), 10, 64)
	if err != nil || targetID <= 0 {
		newest, nerr := marketplace.NewerVersion(ctx, h.pool, held.ListingID, held.VersionID)
		if errors.Is(nerr, marketplace.ErrNotFound) {
			hireError(w, http.StatusNotFound, "up_to_date", "The agent is on the newest version.")
			return
		}
		if nerr != nil {
			slog.ErrorContext(ctx, "marketplace upgrade plan: find the newest version failed", "error", nerr)
			hireError(w, http.StatusInternalServerError, "internal_error", "Failed to read the upgrade.")
			return
		}
		targetID = newest.VersionID
	}
	plan, status, code, msg := h.buildUpgradePlan(ctx, schema, projectID, appID, held, targetID)
	if code != "" {
		hireError(w, status, code, msg)
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

func (h *Handler) buildUpgradePlan(ctx context.Context, schema string, projectID, appID int64, held marketplace.Held, targetID int64) (upgradePlan, int, string, string) {
	fail := func(status int, code, msg string) (upgradePlan, int, string, string) {
		return upgradePlan{}, status, code, msg
	}
	target, err := marketplace.LoadVersion(ctx, h.pool, held.ListingID, targetID)
	if errors.Is(err, marketplace.ErrNotFound) {
		return fail(http.StatusNotFound, "not_found", "The listing version does not exist.")
	}
	prev, prevErr := marketplace.LoadVersion(ctx, h.pool, held.ListingID, held.VersionID)
	currentInfo, curErr := marketplace.LoadVersionInfo(ctx, h.pool, held.ListingID, held.VersionID)
	targetInfo, tgtErr := marketplace.LoadVersionInfo(ctx, h.pool, held.ListingID, targetID)
	if err != nil || prevErr != nil || curErr != nil || tgtErr != nil {
		slog.ErrorContext(ctx, "marketplace upgrade plan: read the versions failed",
			"application_id", appID, "error", errors.Join(err, prevErr, curErr, tgtErr))
		return fail(http.StatusInternalServerError, "internal_error", "Failed to read the upgrade.")
	}
	changes, err := marketplace.Changes(prev, target)
	if err != nil {
		slog.ErrorContext(ctx, "marketplace upgrade plan: diff failed", "application_id", appID, "error", err)
		return fail(http.StatusInternalServerError, "internal_error", "Failed to read the upgrade.")
	}
	avg, count, _ := marketplace.RatingOf(ctx, h.pool, held.ListingID) // a missing rating reads as none
	tier := registryTier(ctx, h.pool, projectID, appID)
	carried := carriedBindings(held.Bindings, target.Requirements, nil)
	direction := "same"
	switch {
	case marketplace.Newer(targetInfo, currentInfo):
		direction = "up"
	case marketplace.Newer(currentInfo, targetInfo):
		direction = "down"
	}
	eligible, _ := marketplace.AutoUpgradeAllowed(tier, prev.Requirements, target.Requirements)
	plan := upgradePlan{
		CurrentVersion:  toPlanVersion(currentInfo),
		TargetVersion:   toPlanVersion(targetInfo),
		Direction:       direction,
		Changes:         changes,
		Widening:        marketplace.WideningOf(prev.Requirements, target.Requirements),
		Requirements:    target.Requirements,
		Bindings:        bindingsResponse(target, boundFrom(carried), true).Bindings,
		NewRequirements: unboundRequirements(target.Requirements, carried),
		NeedsApproval:   marketplace.NeedsApproval(tier, target.Requirements),
		AutoEligible:    eligible && direction == "up",
		Hireable:        target.Hireable(),
		Feedback:        planFeedback{RatingAvg: avg, RatingCount: count},
	}
	if plan.Requirements.Items == nil {
		plan.Requirements.Items = []marketplace.Requirement{}
	}
	_ = schema
	return plan, http.StatusOK, "", ""
}

// boundFrom turns carried request bindings back into stored ones for the plan.
func boundFrom(carried []hireBindingBody) []marketplace.Binding {
	out := make([]marketplace.Binding, 0, len(carried))
	for _, b := range carried {
		item := marketplace.Binding{RequirementKey: b.RequirementKey, Disabled: b.Disabled}
		if id, err := strconv.ParseInt(b.ToolkitID, 10, 64); err == nil && id > 0 {
			item.ToolkitID = id
		}
		out = append(out, item)
	}
	return out
}

type upgradeBody struct {
	VersionID string            `json:"version_id"`
	Bindings  []hireBindingBody `json:"bindings"`
}

// MarketplaceUpgrade answers POST
// /elitea_core/marketplace_upgrade/prompt_lib/{projectID}/{applicationID}.
//
// It applies at once when the agent and the target are low tier and the target
// holds no write tool (one click), recording the request as approved. Otherwise
// it records a change request of kind `upgrade` that a person approves, and the
// agent keeps running on its pinned version until then.
func (h *Handler) MarketplaceUpgrade(w http.ResponseWriter, r *http.Request) {
	if h.pool == nil {
		hireError(w, http.StatusServiceUnavailable, "unavailable", "The marketplace needs a database.")
		return
	}
	ctx := r.Context()
	projectID, appID, schema, ok := parseUpgradeIDs(w, r)
	if !ok {
		return
	}
	userID, hasUser := importPrincipalUserID(ctx)
	if !hasUser {
		hireError(w, http.StatusUnauthorized, "unauthorized", "An authenticated person is required to upgrade.")
		return
	}
	var body upgradeBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		hireError(w, http.StatusBadRequest, "invalid_request", "The request body is not valid.")
		return
	}
	targetID, err := strconv.ParseInt(body.VersionID, 10, 64)
	if err != nil || targetID <= 0 {
		hireError(w, http.StatusBadRequest, "invalid_request", "version_id must be an id.")
		return
	}
	held, err := marketplace.LoadHeld(ctx, h.pool, projectID, appID, false)
	if errors.Is(err, marketplace.ErrNotFound) {
		hireError(w, http.StatusNotFound, "not_found", "The agent was not hired from the marketplace.")
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "marketplace upgrade: read the install failed", "application_id", appID, "error", err)
		hireError(w, http.StatusInternalServerError, "internal_error", "Failed to upgrade.")
		return
	}
	if held.Mode != marketplace.ModeInstall {
		hireError(w, http.StatusConflict, "fork_has_no_upgrade", "A fork takes upstream updates through its review, not an upgrade.")
		return
	}
	if !held.ToolsEnabled {
		hireError(w, http.StatusConflict, "hire_pending", "The hire of this agent is not approved yet. Approve it first.")
		return
	}
	if targetID == held.VersionID {
		hireError(w, http.StatusConflict, "already_pinned", "The agent is already on this version.")
		return
	}
	target, err := marketplace.LoadVersion(ctx, h.pool, held.ListingID, targetID)
	if errors.Is(err, marketplace.ErrNotFound) {
		hireError(w, http.StatusNotFound, "not_found", "The listing version does not exist.")
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "marketplace upgrade: read the version failed", "version_id", targetID, "error", err)
		hireError(w, http.StatusInternalServerError, "internal_error", "Failed to upgrade.")
		return
	}
	if !target.Hireable() {
		hireError(w, http.StatusConflict, "version_not_hireable", "This version was yanked, withdrawn or delisted. Choose another version.")
		return
	}
	bindings, code, msg := h.checkBindings(ctx, schema, target.Requirements, carriedBindings(held.Bindings, target.Requirements, body.Bindings))
	if code != "" {
		status := http.StatusConflict
		switch code {
		case "binding_invalid":
			status = http.StatusBadRequest
		case "internal_error":
			status = http.StatusInternalServerError
		}
		hireError(w, status, code, msg)
		return
	}
	result, status, code, msg := h.recordAndApplyUpgrade(ctx, upgradeRequest{
		ProjectID: projectID, ApplicationID: appID, Held: held, Target: target, Bindings: bindings, UserID: userID.Int64(),
	})
	if code != "" {
		hireError(w, status, code, msg)
		return
	}
	if h.events != nil {
		h.events.Emit(ctx, chi.URLParam(r, "projectID"), "agent.upgraded", map[string]any{
			"application_id": appID, "listing_id": held.ListingID, "version_id": targetID, "applied": result["applied"],
		})
	}
	writeJSON(w, http.StatusOK, result)
}

type upgradeRequest struct {
	ProjectID, ApplicationID int64
	Held                     marketplace.Held
	Target                   marketplace.StoredVersion
	Bindings                 []marketplace.Binding
	UserID                   int64
	// ApprovalReason names the rule that lets the request apply without a
	// person's second click: the policy of an automatic upgrade. Empty for a
	// person's own upgrade.
	ApprovalReason string
	// Automatic is an upgrade the platform makes by the `auto` policy: it
	// applies only when it needs no approval.
	Automatic bool
}

// recordAndApplyUpgrade records the change request and applies the upgrade when
// it needs no approval, in one transaction. The answer is the response body, or
// the status, code and message of a refusal.
func (h *Handler) recordAndApplyUpgrade(ctx context.Context, req upgradeRequest) (map[string]any, int, string, string) {
	internal := func() (map[string]any, int, string, string) {
		return nil, http.StatusInternalServerError, "internal_error", "Failed to upgrade."
	}
	tier := registryTier(ctx, h.pool, req.ProjectID, req.ApplicationID)
	needsApproval := marketplace.NeedsApproval(tier, req.Target.Requirements)
	if req.Automatic && needsApproval {
		return nil, http.StatusConflict, "needs_approval", "An automatic upgrade never needs an approval."
	}
	selfApprove := !needsApproval
	reason := "direct_low_tier"
	if req.Automatic {
		reason = req.ApprovalReason
	}
	if needsApproval {
		personal, err := marketplace.IsPersonalProject(ctx, h.pool, req.ProjectID)
		if err != nil {
			slog.ErrorContext(ctx, "marketplace upgrade: read the project failed", "project_id", req.ProjectID, "error", err)
			return internal()
		}
		if personal {
			allowed, status, code, msg := h.personalSelfApprove(ctx)
			if code != "" {
				return nil, status, code, msg
			}
			if !allowed {
				return nil, http.StatusConflict, "hire_needs_shared_project",
					"This upgrade needs an approval, and a personal project has no second approver. Use a shared project."
			}
			selfApprove, reason = true, ""
		}
	}
	governor, hasGovernor := h.governor.(hireGovernor)
	if needsApproval && !selfApprove && !hasGovernor {
		return nil, http.StatusServiceUnavailable, "unavailable", "Approvals are not available on this deployment."
	}
	payload, err := json.Marshal(marketplace.UpgradePayload{Bindings: req.Bindings})
	if err != nil {
		return internal()
	}
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return internal()
	}
	defer func() { _ = tx.Rollback(ctx) }()
	result := map[string]any{
		"application_id": marketplace.IDText(req.ApplicationID), "version_id": marketplace.IDText(req.Target.VersionID),
		"applied": selfApprove, "awaiting_approval": !selfApprove,
	}
	if hasGovernor {
		change, err := governor.RecordMarketplaceChangeTx(ctx, tx, agentlifecycle.NewMarketplaceChange{
			ProjectID: req.ProjectID, ApplicationID: req.ApplicationID, Tier: agentlifecycle.Tier(marketplace.MaxTier(tier, req.Target.Requirements.Tier)),
			ProposedBy: req.UserID, NeedsChange: true, Kind: agentlifecycle.KindUpgrade,
			Summary:   fmt.Sprintf("Upgrade %s to %s.", req.Target.ListingName, req.Target.Name),
			ListingID: req.Held.ListingID, VersionID: req.Target.VersionID, Digest: req.Target.Digest,
			SelfApprove: selfApprove, ApprovalReason: reason, Payload: payload,
		})
		if err != nil {
			if refusal, isRefusal := agentlifecycle.AsRefusal(err); isRefusal {
				return nil, refusal.Status(), refusal.Code, refusal.Message
			}
			slog.ErrorContext(ctx, "marketplace upgrade: record the change request failed", "error", err)
			return internal()
		}
		if change.ID > 0 {
			result["change_request_id"] = marketplace.IDText(change.ID)
		}
	}
	if selfApprove {
		if err := h.ApplyMarketplaceTx(ctx, tx, agentlifecycle.MarketplaceApply{
			ProjectID: req.ProjectID, ApplicationID: req.ApplicationID, Kind: agentlifecycle.KindUpgrade,
			ListingID: req.Held.ListingID, VersionID: req.Target.VersionID, Digest: req.Target.Digest,
			Payload: payload, ActorID: req.UserID,
		}); err != nil {
			if refusal, isRefusal := agentlifecycle.AsRefusal(err); isRefusal {
				return nil, refusal.Status(), refusal.Code, refusal.Message
			}
			if errors.Is(err, marketplace.ErrNotHireable) {
				return nil, http.StatusConflict, "version_not_hireable", "This version was yanked, withdrawn or delisted. Choose another version."
			}
			slog.ErrorContext(ctx, "marketplace upgrade: apply failed", "application_id", req.ApplicationID, "error", err)
			return internal()
		}
	}
	if err := tx.Commit(ctx); err != nil {
		slog.ErrorContext(ctx, "marketplace upgrade: commit failed", "error", err)
		return internal()
	}
	return result, http.StatusOK, "", ""
}

type policyBody struct {
	Policy string `json:"policy"`
}

// MarketplaceUpgradePolicy answers PUT
// /elitea_core/marketplace_upgrade_policy/prompt_lib/{projectID}/{applicationID}.
func (h *Handler) MarketplaceUpgradePolicy(w http.ResponseWriter, r *http.Request) {
	if h.pool == nil {
		hireError(w, http.StatusServiceUnavailable, "unavailable", "The marketplace needs a database.")
		return
	}
	ctx := r.Context()
	projectID, appID, _, ok := parseUpgradeIDs(w, r)
	if !ok {
		return
	}
	var body policyBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !marketplace.ValidPolicy(body.Policy) {
		hireError(w, http.StatusBadRequest, "invalid_request", "policy must be pinned, notify or auto.")
		return
	}
	found, err := marketplace.SetPolicy(ctx, h.pool, projectID, appID, body.Policy)
	if err != nil {
		slog.ErrorContext(ctx, "marketplace upgrade policy: write failed", "application_id", appID, "error", err)
		hireError(w, http.StatusInternalServerError, "internal_error", "Failed to save the policy.")
		return
	}
	if !found {
		hireError(w, http.StatusNotFound, "not_found", "Only an agent installed from the marketplace has an upgrade policy.")
		return
	}
	// A newer version may already exist: settle the agent under its new policy.
	h.settleInstall(ctx, projectID, appID)
	writeJSON(w, http.StatusOK, map[string]any{"policy": body.Policy})
}

// settleInstall applies the upgrade policy of one install against the newest
// version of its listing: `notify` tells its people once per version, `auto`
// upgrades when the rule allows it and otherwise tells them why not. It never
// fails the caller: a problem is logged and the agent stays pinned.
func (h *Handler) settleInstall(ctx context.Context, projectID, appID int64) {
	held, err := marketplace.LoadHeld(ctx, h.pool, projectID, appID, false)
	if err != nil || held.Mode != marketplace.ModeInstall || held.Policy == marketplace.PolicyPinned || !held.ToolsEnabled {
		return
	}
	newer, err := marketplace.NewerVersion(ctx, h.pool, held.ListingID, held.VersionID)
	if errors.Is(err, marketplace.ErrNotFound) {
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "marketplace settle: find the newer version failed", "application_id", appID, "error", err)
		return
	}
	prev, err := marketplace.LoadVersion(ctx, h.pool, held.ListingID, held.VersionID)
	if err != nil {
		slog.ErrorContext(ctx, "marketplace settle: read the pinned version failed", "application_id", appID, "error", err)
		return
	}
	owners := marketplace.OwnersOf(ctx, h.pool, projectID, appID, installerOf(ctx, h.pool, held))
	base := map[string]any{
		"project_id": projectID, "application_id": appID, "listing_id": held.ListingID,
		"version_id": newer.VersionID, "listing_name": newer.ListingName, "version_name": newer.Name,
	}
	notifyAll := func(kind, text string) {
		meta := map[string]any{"message": text}
		for k, v := range base {
			meta[k] = v
		}
		for _, owner := range owners {
			key := fmt.Sprintf("%s-%d-%d-%d-%d", kind, projectID, appID, newer.VersionID, owner)
			if err := marketplace.Notify(ctx, h.pool, key, projectID, owner, kind, meta); err != nil {
				slog.WarnContext(ctx, "marketplace settle: notify failed", "application_id", appID, "error", err)
			}
		}
	}
	if held.Policy == marketplace.PolicyNotify {
		notifyAll(marketplace.NotifyUpdateAvailable, fmt.Sprintf("%s %s is available for an agent you installed.", newer.ListingName, newer.Name))
		return
	}
	tier := registryTier(ctx, h.pool, projectID, appID)
	allowed, blockedBy := marketplace.AutoUpgradeAllowed(tier, prev.Requirements, newer.Requirements)
	if !allowed {
		why := ""
		if len(blockedBy) > 0 {
			why = " It adds: " + blockedBy[0].Detail + "."
		}
		notifyAll(marketplace.NotifyUpgradeNeedsHuman,
			fmt.Sprintf("%s %s needs a person to approve the upgrade.%s", newer.ListingName, newer.Name, why))
		return
	}
	installer := installerOf(ctx, h.pool, held)
	if installer <= 0 {
		return
	}
	result, _, code, msg := h.recordAndApplyUpgrade(ctx, upgradeRequest{
		ProjectID: projectID, ApplicationID: appID, Held: held, Target: newer,
		Bindings: boundFrom(carriedBindings(held.Bindings, newer.Requirements, nil)), UserID: installer, ApprovalReason: "marketplace_auto_upgrade", Automatic: true,
	})
	if code != "" {
		slog.WarnContext(ctx, "marketplace settle: the automatic upgrade was refused", "application_id", appID, "code", code, "msg", msg)
		return
	}
	_ = result
	notifyAll(marketplace.NotifyUpgradeApplied, fmt.Sprintf("%s was upgraded to %s by its upgrade policy.", newer.ListingName, newer.Name))
	if h.events != nil {
		h.events.Emit(ctx, strconv.FormatInt(projectID, 10), "agent.upgraded", map[string]any{
			"application_id": appID, "listing_id": held.ListingID, "version_id": newer.VersionID, "applied": true, "automatic": true,
		})
	}
}

// installerOf is the person who hired the agent, who stands as the proposer of
// an automatic upgrade.
func installerOf(ctx context.Context, q relationQuerier, held marketplace.Held) int64 {
	var by int64
	if err := q.QueryRow(ctx, `
SELECT installed_by FROM elitea_marketplace.installs WHERE project_id = $1 AND entity_kind = 'agent' AND entity_id = $2`,
		held.ProjectID, held.EntityID).Scan(&by); err != nil {
		return 0
	}
	return by
}

// settleListingHolders settles every install of the listing that a source agent
// published, after a publish committed. It runs on the publish path, so it is
// bounded and logs rather than fails.
func (h *Handler) settleListingHolders(ctx context.Context, projectID string, appID int) {
	if h.pool == nil {
		return
	}
	var listingID int64
	err := h.pool.QueryRow(ctx, `
SELECT id FROM elitea_marketplace.listings WHERE kind = 'agent' AND source_project_id = $1 AND source_entity_id = $2`,
		parseProjectID(projectID), int64(appID)).Scan(&listingID)
	if errors.Is(err, pgx.ErrNoRows) {
		return
	}
	if err != nil {
		slog.WarnContext(ctx, "marketplace settle: find the listing failed", "project_id", projectID, "error", err)
		return
	}
	rows, err := h.pool.Query(ctx, `
SELECT project_id, entity_id FROM elitea_marketplace.installs
WHERE listing_id = $1 AND entity_kind = 'agent' AND mode = 'install' AND upgrade_policy <> 'pinned'
ORDER BY project_id, entity_id LIMIT 500`, listingID)
	if err != nil {
		slog.WarnContext(ctx, "marketplace settle: list the installs failed", "listing_id", listingID, "error", err)
		return
	}
	type ref struct{ project, app int64 }
	var refs []ref
	for rows.Next() {
		var item ref
		if err := rows.Scan(&item.project, &item.app); err != nil {
			rows.Close()
			return
		}
		refs = append(refs, item)
	}
	rows.Close()
	for _, item := range refs {
		h.settleInstall(ctx, item.project, item.app)
	}
}

// personalSelfApprove reads the platform policy that lets a person approve their
// own Marketplace change in a personal project, which has no second approver.
func (h *Handler) personalSelfApprove(ctx context.Context) (allowed bool, status int, code, msg string) {
	allowed, err := platformconfig.LoadMarketplacePersonalSelfApprove(ctx, h.pool)
	if err != nil {
		slog.ErrorContext(ctx, "marketplace: read the self-approve policy failed", "error", err)
		return false, http.StatusInternalServerError, "internal_error", "Failed to read the approval policy."
	}
	return allowed, 0, "", ""
}
