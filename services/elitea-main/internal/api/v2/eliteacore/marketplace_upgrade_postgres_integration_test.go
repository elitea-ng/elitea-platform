package eliteacore_test

// Real-PostgreSQL coverage for the upgrades of ADR-0051 decision 4 (issue 1630):
// Upgrade to... up, down and across with its plan and change request, the
// upgrade policy and the widening rule that keeps `auto` from taking a version
// that adds a tool, the yank notice and the administrator's suspension, and the
// three-way (and two-way) upstream update of a fork that is never applied
// without a person.

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

// installed hires the listing's current version into project 2 as an install
// bound to a fresh github toolkit, and answers the agent id and the binding key.
func (h *marketplaceHarness) installed(listingID string) (appID int64, bindingKey string, toolkit int) {
	h.t.Helper()
	versionID := h.currentVersionID(listingID)
	toolkit = h.projectToolkit(2, "github")
	for _, item := range h.detail(listingID)["versions"].([]any)[0].(map[string]any)["requirements"].(map[string]any)["items"].([]any) {
		if m := item.(map[string]any); m["kind"] == "toolkit" {
			bindingKey = m["key"].(string)
		}
	}
	status, body := h.do(h.router, http.MethodPost, "/elitea_core/marketplace_hire/prompt_lib/2",
		hireBody(listingID, versionID, "install", map[string]any{"requirement_key": bindingKey, "toolkit_id": strconv.Itoa(toolkit)}))
	if status != http.StatusCreated {
		h.t.Fatalf("hire = %d %v", status, body)
	}
	return mustInt(h.t, body["application_id"].(string)), bindingKey, toolkit
}

func (h *marketplaceHarness) versionIDByName(listingID, name string) string {
	h.t.Helper()
	for _, v := range versionsOf(h.detail(listingID)) {
		if v["name"] == name {
			return v["id"].(string)
		}
	}
	h.t.Fatalf("listing %s has no version %q", listingID, name)
	return ""
}

func (h *marketplaceHarness) setInstructions(agent seededAgent, text string) {
	h.t.Helper()
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE p_2.application_versions SET instructions = $2 WHERE id = $1`, agent.versionID, text); err != nil {
		h.t.Fatalf("edit the source: %v", err)
	}
}

func (h *marketplaceHarness) installedInstructions(appID int64) string {
	h.t.Helper()
	var text string
	if err := h.pool.QueryRow(context.Background(),
		`SELECT instructions FROM p_2.application_versions WHERE application_id = $1 ORDER BY id LIMIT 1`, appID).Scan(&text); err != nil {
		h.t.Fatalf("read the installed instructions: %v", err)
	}
	return text
}

func (h *marketplaceHarness) pinnedVersion(appID int64) string {
	h.t.Helper()
	var version int64
	if err := h.pool.QueryRow(context.Background(),
		`SELECT version_id FROM elitea_marketplace.installs WHERE project_id = 2 AND entity_kind = 'agent' AND entity_id = $1`, appID).Scan(&version); err != nil {
		h.t.Fatalf("read the pinned version: %v", err)
	}
	return strconv.FormatInt(version, 10)
}

func (h *marketplaceHarness) toolMappings(appID int64, toolID int) int {
	h.t.Helper()
	var n int
	if err := h.pool.QueryRow(context.Background(), `
SELECT count(*) FROM p_2.entity_tool_mapping m JOIN p_2.application_versions v ON v.id = m.entity_version_id
WHERE v.application_id = $1 AND m.tool_id = $2`, appID, toolID).Scan(&n); err != nil {
		h.t.Fatalf("count the tool mappings: %v", err)
	}
	return n
}

func upgradeURL(appID int64) string {
	return "/elitea_core/marketplace_upgrade/prompt_lib/2/" + strconv.FormatInt(appID, 10)
}

// TestUpgradeToAnyVersionAtLowTierIsOneClick walks T3: the plan shows the diff
// and the release notes, a low-tier upgrade applies at once and is recorded as
// an approved change request of kind upgrade, and the same route goes down.
func TestUpgradeToAnyVersionAtLowTierIsOneClick(t *testing.T) {
	h := newMarketplaceHarness(t)
	ctx := context.Background()
	agent := h.seedAgent(2, "Issue Reader", map[string][]string{"github": {"list_issues"}})
	h.publish(agent, "v1", "First release.")
	id := h.listingID("Issue Reader")
	appID, _, toolkit := h.installed(id)
	v1 := h.pinnedVersion(appID)

	// Up to date: there is nothing newer to plan.
	if status, body := h.do(h.router, http.MethodGet, upgradeURL(appID), nil); status != http.StatusNotFound || body["error"] != "up_to_date" {
		t.Fatalf("plan on the newest version = %d %v, want 404 up_to_date", status, body)
	}

	h.setInstructions(agent, "summarise the release in one line")
	h.publish(agent, "v2", "Shorter summaries.")
	v2 := h.versionIDByName(id, "v2")

	status, plan := h.do(h.router, http.MethodGet, upgradeURL(appID), nil)
	if status != http.StatusOK {
		t.Fatalf("plan = %d %v", status, plan)
	}
	target := plan["target_version"].(map[string]any)
	if target["id"] != v2 || target["release_notes"] != "Shorter summaries." || plan["direction"] != "up" {
		t.Errorf("plan target = %v direction = %v", target, plan["direction"])
	}
	changes, _ := plan["changes"].([]any)
	foundInstructions := false
	for _, c := range changes {
		if c.(map[string]any)["path"] == "/definition/version/instructions" {
			foundInstructions = true
		}
	}
	if !foundInstructions {
		t.Errorf("plan changes = %v, want the instructions change", changes)
	}
	if plan["needs_approval"] != false || plan["auto_eligible"] != true {
		t.Errorf("plan = needs_approval %v auto_eligible %v, want a one-click upgrade", plan["needs_approval"], plan["auto_eligible"])
	}
	if w, _ := plan["widening"].([]any); len(w) != 0 {
		t.Errorf("widening = %v, want none", w)
	}

	// The installed agent is untouched until the person acts.
	if got := h.installedInstructions(appID); got != "summarise the release" {
		t.Fatalf("instructions before the upgrade = %q", got)
	}
	status, body := h.do(h.router, http.MethodPost, upgradeURL(appID), map[string]any{"version_id": v2})
	if status != http.StatusOK || body["applied"] != true || body["awaiting_approval"] != false || body["change_request_id"] == nil {
		t.Fatalf("upgrade = %d %v, want applied with a recorded change request", status, body)
	}
	if got := h.installedInstructions(appID); got != "summarise the release in one line" {
		t.Errorf("instructions after the upgrade = %q", got)
	}
	if h.pinnedVersion(appID) != v2 {
		t.Errorf("pinned version = %s, want %s", h.pinnedVersion(appID), v2)
	}
	if n := h.toolMappings(appID, toolkit); n != 1 {
		t.Errorf("the bound toolkit is mapped %d times after the upgrade, want 1", n)
	}
	var kind, state string
	if err := h.pool.QueryRow(ctx, `
SELECT kind, state FROM elitea_lifecycle.change_requests WHERE application_id = $1 ORDER BY id DESC LIMIT 1`, appID).
		Scan(&kind, &state); err != nil || kind != "upgrade" || state != "approved" {
		t.Errorf("change request = %q %q (%v), want an approved upgrade", kind, state, err)
	}
	// The origin names the new pin.
	_, origin := h.do(h.router, http.MethodGet, "/elitea_core/marketplace_origin/prompt_lib/2/"+strconv.FormatInt(appID, 10), nil)
	if origin["version_id"] != v2 || origin["latest_version"] != nil {
		t.Errorf("origin = %v, want pinned to v2 and up to date", origin)
	}

	// Down is allowed too.
	status, plan = h.do(h.router, http.MethodGet, upgradeURL(appID)+"?version_id="+v1, nil)
	if status != http.StatusOK || plan["direction"] != "down" {
		t.Fatalf("plan to v1 = %d %v, want direction down", status, plan)
	}
	if status, body = h.do(h.router, http.MethodPost, upgradeURL(appID), map[string]any{"version_id": v1}); status != http.StatusOK || body["applied"] != true {
		t.Fatalf("downgrade = %d %v", status, body)
	}
	if got := h.installedInstructions(appID); got != "summarise the release" {
		t.Errorf("instructions after the downgrade = %q", got)
	}
	if status, body = h.do(h.router, http.MethodPost, upgradeURL(appID), map[string]any{"version_id": v1}); status != http.StatusConflict || body["error"] != "already_pinned" {
		t.Errorf("upgrade to the pinned version = %d %v, want 409 already_pinned", status, body)
	}
}

// TestAutoUpgradeNeverTakesAWiderVersion is the widening rule: under `auto` a
// version that changes nothing but text is taken by itself, and a version that
// adds a tool is not, whatever the policy says. It needs a person.
func TestAutoUpgradeNeverTakesAWiderVersion(t *testing.T) {
	h := newMarketplaceHarness(t)
	ctx := context.Background()
	agent := h.seedAgent(2, "Issue Reader", map[string][]string{"github": {"list_issues"}})
	h.publish(agent, "v1", "")
	id := h.listingID("Issue Reader")
	appID, _, toolkit := h.installed(id)
	app := strconv.FormatInt(appID, 10)
	policyURL := "/elitea_core/marketplace_upgrade_policy/prompt_lib/2/" + app

	if status, _ := h.do(h.router, http.MethodPut, policyURL, map[string]any{"policy": "sometimes"}); status != http.StatusBadRequest {
		t.Errorf("an unknown policy answered %d, want 400", status)
	}
	if status, body := h.do(h.router, http.MethodPut, policyURL, map[string]any{"policy": "auto"}); status != http.StatusOK || body["policy"] != "auto" {
		t.Fatalf("set the policy = %d %v", status, body)
	}
	_, origin := h.do(h.router, http.MethodGet, "/elitea_core/marketplace_origin/prompt_lib/2/"+app, nil)
	if origin["upgrade_policy"] != "auto" {
		t.Errorf("origin policy = %v, want auto", origin["upgrade_policy"])
	}

	// A text-only version is taken by itself, on publish.
	h.setInstructions(agent, "summarise the release briefly")
	h.publish(agent, "v2", "")
	v2 := h.versionIDByName(id, "v2")
	if h.pinnedVersion(appID) != v2 || h.installedInstructions(appID) != "summarise the release briefly" {
		t.Fatalf("the auto policy did not take v2: pinned %s, instructions %q", h.pinnedVersion(appID), h.installedInstructions(appID))
	}
	// A version that adds a write tool is not.
	if _, err := h.pool.Exec(ctx, `
UPDATE p_2.entity_tool_mapping SET selected_tools = '["list_issues", "create_issue"]'::jsonb WHERE entity_version_id = $1`, agent.versionID); err != nil {
		t.Fatal(err)
	}
	h.publish(agent, "v3", "Can now open issues.")
	v3 := h.versionIDByName(id, "v3")
	if h.pinnedVersion(appID) != v2 {
		t.Fatalf("the auto policy took v3, which adds a write tool: pinned %s", h.pinnedVersion(appID))
	}
	_, origin = h.do(h.router, http.MethodGet, "/elitea_core/marketplace_origin/prompt_lib/2/"+app, nil)
	wider, _ := origin["update_widening"].([]any)
	if len(wider) == 0 {
		t.Errorf("update_widening = %v, want what v3 adds", origin["update_widening"])
	}
	var told int
	if err := h.pool.QueryRow(ctx, `
SELECT count(*) FROM centry.notifications WHERE event_type = 'marketplace_upgrade_needs_approval'`).Scan(&told); err != nil || told == 0 {
		t.Errorf("notifications about the wider version = %d (%v), want at least one", told, err)
	}
	status, plan := h.do(h.router, http.MethodGet, upgradeURL(appID), nil)
	if status != http.StatusOK || plan["auto_eligible"] != false || plan["needs_approval"] != true {
		t.Errorf("plan for v3 = %d auto_eligible %v needs_approval %v", status, plan["auto_eligible"], plan["needs_approval"])
	}

	// A person can still upgrade; it waits for an approver and applies on approval.
	hirer := h.newRouter(11)
	status, body := h.do(hirer, http.MethodPost, upgradeURL(appID), map[string]any{"version_id": v3})
	if status != http.StatusOK || body["applied"] != false || body["awaiting_approval"] != true || body["change_request_id"] == nil {
		t.Fatalf("upgrade to v3 = %d %v, want a change request awaiting approval", status, body)
	}
	if h.pinnedVersion(appID) != v2 {
		t.Fatalf("the agent moved before the approval")
	}
	changeID := mustInt(t, body["change_request_id"].(string))
	if _, err := h.lifecycle.ApproveChangeRequest(ctx, repos.Approval{ProjectID: 2, ApplicationID: appID, ChangeID: changeID, ApproverID: 11}); err == nil {
		t.Fatal("the proposer approved their own upgrade")
	}
	outcome, err := h.lifecycle.ApproveChangeRequest(ctx, repos.Approval{ProjectID: 2, ApplicationID: appID, ChangeID: changeID, ApproverID: 12, Governance: true})
	if err != nil {
		t.Fatalf("approve the upgrade: %v", err)
	}
	if outcome.Change.State != agentlifecycle.ChangeApproved {
		t.Fatalf("upgrade state after approval = %q", outcome.Change.State)
	}
	if h.pinnedVersion(appID) != v3 {
		t.Errorf("pinned version after the approval = %s, want %s", h.pinnedVersion(appID), v3)
	}
	if n := h.toolMappings(appID, toolkit); n != 1 {
		t.Errorf("the toolkit is mapped %d times after the approved upgrade, want 1", n)
	}
	var tier string
	if err := h.pool.QueryRow(ctx, `SELECT risk_tier FROM elitea_lifecycle.agent_registry WHERE tenant_project_id = 2 AND application_id = $1`, appID).
		Scan(&tier); err != nil || tier == "" {
		t.Errorf("registry tier = %q (%v)", tier, err)
	}
}

// TestUpgradeIsRefusedForAYankedTargetAndForAFork proves the refusals.
func TestUpgradeIsRefusedForAYankedTargetAndForAFork(t *testing.T) {
	h := newMarketplaceHarness(t)
	agent := h.seedAgent(2, "Plain Helper", nil)
	h.publish(agent, "v1", "")
	id := h.listingID("Plain Helper")
	v1 := h.currentVersionID(id)
	status, body := h.do(h.router, http.MethodPost, "/elitea_core/marketplace_hire/prompt_lib/2", hireBody(id, v1, "install"))
	if status != http.StatusCreated {
		t.Fatalf("hire = %d %v", status, body)
	}
	appID := mustInt(t, body["application_id"].(string))
	h.setInstructions(agent, "second text")
	h.publish(agent, "v2", "")
	v2 := h.versionIDByName(id, "v2")
	stateURL := "/elitea_core/marketplace_listing_version/prompt_lib/2/" + id + "/" + v2
	if status, _ := h.do(h.router, http.MethodPost, stateURL, map[string]any{"state": "yanked", "reason": "broken"}); status != http.StatusOK {
		t.Fatalf("yank v2 = %d", status)
	}
	if status, body = h.do(h.router, http.MethodPost, upgradeURL(appID), map[string]any{"version_id": v2}); status != http.StatusConflict || body["error"] != "version_not_hireable" {
		t.Errorf("upgrade to a yanked version = %d %v, want 409 version_not_hireable", status, body)
	}
	// A fork has no upgrade: its update is the upstream review.
	status, body = h.do(h.router, http.MethodPost, "/elitea_core/marketplace_hire/prompt_lib/2", hireBody(id, v1, "fork"))
	if status != http.StatusCreated {
		t.Fatalf("fork = %d %v", status, body)
	}
	forkID := mustInt(t, body["application_id"].(string))
	if status, body = h.do(h.router, http.MethodPost, upgradeURL(forkID), map[string]any{"version_id": v1}); status != http.StatusConflict || body["error"] != "fork_has_no_upgrade" {
		t.Errorf("upgrade of a fork = %d %v, want 409 fork_has_no_upgrade", status, body)
	}
}

type fakeSuspender struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeSuspender) SuspendAgent(_ context.Context, actor, project, app int64, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, strconv.FormatInt(project, 10)+"/"+strconv.FormatInt(app, 10)+"/"+reason)
	_ = actor
	return nil
}

type adminResolver struct{}

func (adminResolver) ResolvePermissions(context.Context, auth.User, string, string) (auth.PermissionResolution, error) {
	return auth.PermissionResolution{UserID: 1, Permissions: []string{eliteacore.PublishedAgentsListPermission}}, nil
}

// TestYankTellsHoldersAndAnAdministratorMayAlsoSuspend proves the yank notice:
// every project that holds the version gets a notification and a banner; the
// suspension, reason listing_yanked, is an administrator's and optional.
func TestYankTellsHoldersAndAnAdministratorMayAlsoSuspend(t *testing.T) {
	h := newMarketplaceHarness(t)
	ctx := context.Background()
	h.publish(h.seedAgent(2, "Plain Helper", nil), "v1", "")
	id := h.listingID("Plain Helper")
	v1 := h.currentVersionID(id)
	status, body := h.do(h.newRouter(11), http.MethodPost, "/elitea_core/marketplace_hire/prompt_lib/2", hireBody(id, v1, "install"))
	if status != http.StatusCreated {
		t.Fatalf("hire = %d %v", status, body)
	}
	appID := mustInt(t, body["application_id"].(string))
	stateURL := "/elitea_core/marketplace_listing_version/prompt_lib/2/" + id + "/" + v1

	// A publisher who is not an administrator cannot ask for a suspension.
	if status, _ := h.do(h.router, http.MethodPost, stateURL, map[string]any{"state": "yanked", "reason": "bad", "suspend": true}); status != http.StatusForbidden {
		t.Fatalf("suspend by a publisher answered %d, want 403", status)
	}
	if status, _ := h.do(h.router, http.MethodPost, stateURL, map[string]any{"state": "deprecated", "suspend": true}); status != http.StatusBadRequest && status != http.StatusForbidden {
		t.Errorf("suspend without a yank answered %d, want a refusal", status)
	}
	if status, _ := h.do(h.router, http.MethodPost, stateURL, map[string]any{"state": "yanked", "reason": "leaks data"}); status != http.StatusOK {
		t.Fatalf("yank = %d", status)
	}
	var message string
	if err := h.pool.QueryRow(ctx, `
SELECT meta->>'message' FROM centry.notifications WHERE event_type = 'marketplace_version_yanked' AND user_id = 11`).Scan(&message); err != nil ||
		!strings.Contains(message, "was yanked") || !strings.Contains(message, "leaks data") {
		t.Fatalf("yank notification = %q (%v)", message, err)
	}
	_, origin := h.do(h.router, http.MethodGet, "/elitea_core/marketplace_origin/prompt_lib/2/"+strconv.FormatInt(appID, 10), nil)
	if origin["version_state"] != "yanked" || origin["version_state_reason"] != "leaks data" {
		t.Errorf("origin = %v, want the yank banner's state and reason", origin)
	}
	// Yanking again does not tell them again.
	if status, _ := h.do(h.router, http.MethodPost, stateURL, map[string]any{"state": "yanked", "reason": "leaks data"}); status != http.StatusOK {
		t.Fatalf("second yank = %d", status)
	}
	var n int
	if err := h.pool.QueryRow(ctx, `SELECT count(*) FROM centry.notifications WHERE event_type = 'marketplace_version_yanked' AND user_id = 11`).Scan(&n); err != nil || n != 1 {
		t.Errorf("yank notifications for the hirer = %d (%v), want 1", n, err)
	}

	// An administrator yanking with suspend suspends the installed agent.
	if status, _ := h.do(h.router, http.MethodPost, stateURL, map[string]any{"state": "available"}); status != http.StatusOK {
		t.Fatalf("restore = %d", status)
	}
	suspender := &fakeSuspender{}
	handler := eliteacore.NewHandler(h.pool, eliteacore.WithPublishGovernor(h.lifecycle),
		eliteacore.WithPermissionResolver(adminResolver{}), eliteacore.WithListingSuspender(suspender))
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(auth.ContextWithUser(r.Context(), auth.User{ID: "1"})))
		})
	})
	router.Post("/elitea_core/marketplace_listing_version/prompt_lib/{projectID}/{listingID}/{versionID}", handler.MarketplaceVersionState)
	if status, body := h.do(router, http.MethodPost, stateURL, map[string]any{"state": "yanked", "reason": "leaks data", "suspend": true}); status != http.StatusOK {
		t.Fatalf("administrator yank with suspend = %d %v", status, body)
	}
	if len(suspender.calls) != 1 || !strings.HasPrefix(suspender.calls[0], "2/"+strconv.FormatInt(appID, 10)+"/listing_yanked") {
		t.Errorf("suspensions = %v, want the installed agent with reason listing_yanked", suspender.calls)
	}
}

// TestForkUpstreamUpdateIsThreeWayAndNeverAutomatic walks T4: a fork records its
// origin version; a new version shows Upstream update available; the review
// lists conflicts first; the merge is a new draft plus a change request, and the
// fork's origin moves only when it is applied.
func TestForkUpstreamUpdateIsThreeWayAndNeverAutomatic(t *testing.T) {
	h := newMarketplaceHarness(t)
	ctx := context.Background()
	agent := h.seedAgent(2, "Release Writer", nil)
	h.publish(agent, "v1", "")
	id := h.listingID("Release Writer")
	v1 := h.currentVersionID(id)
	status, body := h.do(h.router, http.MethodPost, "/elitea_core/marketplace_hire/prompt_lib/2", hireBody(id, v1, "fork"))
	if status != http.StatusCreated {
		t.Fatalf("fork = %d %v", status, body)
	}
	forkID := mustInt(t, body["application_id"].(string))
	app := strconv.FormatInt(forkID, 10)
	url := "/elitea_core/marketplace_upstream/prompt_lib/2/" + app

	// The fork edits its welcome message and its instructions; upstream changes
	// its instructions and its agent type.
	if _, err := h.pool.Exec(ctx, `
UPDATE p_2.application_versions SET instructions = 'our instructions', welcome_message = 'our welcome' WHERE application_id = $1`, forkID); err != nil {
		t.Fatal(err)
	}
	h.setInstructions(agent, "their instructions")
	if _, err := h.pool.Exec(ctx, `UPDATE p_2.application_versions SET welcome_message = 'their welcome' WHERE id = $1`, agent.versionID); err != nil {
		t.Fatal(err)
	}
	// The fork leaves the welcome message as the base had it, so only the
	// instructions conflict.
	if _, err := h.pool.Exec(ctx, `UPDATE p_2.application_versions SET welcome_message = 'hello' WHERE application_id = $1`, forkID); err != nil {
		t.Fatal(err)
	}
	h.publish(agent, "v2", "New wording.")
	v2 := h.versionIDByName(id, "v2")

	_, origin := h.do(h.router, http.MethodGet, "/elitea_core/marketplace_origin/prompt_lib/2/"+app, nil)
	if latest, _ := origin["latest_version"].(map[string]any); latest["id"] != v2 || origin["upgrade_policy"] != "" {
		t.Fatalf("origin = %v, want an upstream update and no policy for a fork", origin)
	}
	status, review := h.do(h.router, http.MethodGet, url, nil)
	if status != http.StatusOK || review["mode"] != "three_way" {
		t.Fatalf("review = %d %v, want a three-way view", status, review)
	}
	fields := review["fields"].([]any)
	first := fields[0].(map[string]any)
	if first["key"] != "instructions" || first["status"] != "conflict" {
		t.Errorf("first field = %v, want the instructions conflict listed first", first)
	}
	byKey := map[string]map[string]any{}
	for _, f := range fields {
		m := f.(map[string]any)
		byKey[m["key"].(string)] = m
	}
	if byKey["welcome_message"]["status"] != "theirs_only" || byKey["welcome_message"]["default"] != "theirs" {
		t.Errorf("welcome_message = %v, want a clean upstream change", byKey["welcome_message"])
	}
	if byKey["agent_type"]["status"] != "same" {
		t.Errorf("agent_type = %v, want same", byKey["agent_type"])
	}

	// A merge with an unresolved conflict is refused, and nothing is written.
	status, body = h.do(h.router, http.MethodPost, url, map[string]any{"version_id": v2, "resolutions": map[string]any{}})
	if status != http.StatusConflict || body["error"] != "unresolved_conflicts" {
		t.Fatalf("merge with a conflict = %d %v, want 409 unresolved_conflicts", status, body)
	}
	// Nothing moved.
	_, origin = h.do(h.router, http.MethodGet, "/elitea_core/marketplace_origin/prompt_lib/2/"+app, nil)
	if origin["version_id"] != v1 {
		t.Fatalf("the fork's origin moved without a merge: %v", origin["version_id"])
	}

	// The fork is above low tier: the merge waits for an approver.
	if _, err := h.pool.Exec(ctx, `UPDATE elitea_lifecycle.agent_registry SET risk_tier = 'medium' WHERE tenant_project_id = 2 AND application_id = $1`, forkID); err != nil {
		t.Fatal(err)
	}
	author := h.newRouter(11)
	status, body = h.do(author, http.MethodPost, url, map[string]any{"version_id": v2, "resolutions": map[string]any{
		"instructions": map[string]any{"choice": "edit", "value": "merged instructions"},
	}})
	if status != http.StatusCreated || body["applied"] != false || body["awaiting_approval"] != true || body["change_request_id"] == nil {
		t.Fatalf("merge = %d %v, want a draft awaiting approval", status, body)
	}
	draftID := mustInt(t, body["draft_version_id"].(string))
	var draftInstructions, draftWelcome, draftStatus string
	if err := h.pool.QueryRow(ctx, `SELECT instructions, welcome_message, status FROM p_2.application_versions WHERE id = $1`, draftID).
		Scan(&draftInstructions, &draftWelcome, &draftStatus); err != nil {
		t.Fatal(err)
	}
	if draftInstructions != "merged instructions" || draftWelcome != "their welcome" || draftStatus != "draft" {
		t.Errorf("draft = %q %q %q, want the edit, their welcome and a draft", draftInstructions, draftWelcome, draftStatus)
	}
	_, origin = h.do(h.router, http.MethodGet, "/elitea_core/marketplace_origin/prompt_lib/2/"+app, nil)
	if origin["version_id"] != v1 {
		t.Fatalf("the fork's origin moved before the approval: %v", origin["version_id"])
	}
	changeID := mustInt(t, body["change_request_id"].(string))
	var kind string
	if err := h.pool.QueryRow(ctx, `SELECT kind FROM elitea_lifecycle.change_requests WHERE id = $1`, changeID).Scan(&kind); err != nil || kind != "upstream_merge" {
		t.Fatalf("change request kind = %q (%v), want upstream_merge", kind, err)
	}
	if _, err := h.lifecycle.ApproveChangeRequest(ctx, repos.Approval{ProjectID: 2, ApplicationID: forkID, ChangeID: changeID, ApproverID: 12, Governance: true}); err != nil {
		t.Fatalf("approve the merge: %v", err)
	}
	_, origin = h.do(h.router, http.MethodGet, "/elitea_core/marketplace_origin/prompt_lib/2/"+app, nil)
	if origin["version_id"] != v2 || origin["latest_version"] != nil {
		t.Errorf("origin after the approval = %v, want the fork on v2", origin)
	}
}

// TestForkWithNoOriginVersionGetsATwoWayDiff proves the legacy fork: it names
// only its parent, so the review has no base and every difference is shown.
func TestForkWithNoOriginVersionGetsATwoWayDiff(t *testing.T) {
	h := newMarketplaceHarness(t)
	ctx := context.Background()
	agent := h.seedAgent(2, "Release Writer", nil)
	h.publish(agent, "v1", "")
	id := h.listingID("Release Writer")
	// A fork made before forks recorded their origin: meta names only the parent.
	var catalogueAgent int64
	if err := h.pool.QueryRow(ctx, `SELECT entity_id FROM elitea_marketplace.listings WHERE id = $1`, id).Scan(&catalogueAgent); err != nil {
		t.Fatal(err)
	}
	var forkApp int64
	if err := h.pool.QueryRow(ctx, `INSERT INTO p_2.applications (name, description, owner_id) VALUES ('Old fork', 'd', 2) RETURNING id`).Scan(&forkApp); err != nil {
		t.Fatal(err)
	}
	if _, err := h.pool.Exec(ctx, `
INSERT INTO p_2.application_versions (application_id, name, status, author_id, instructions, agent_type, welcome_message, meta)
VALUES ($1, 'base', 'draft', 7, 'our own instructions', 'agent', 'hello',
        jsonb_build_object('parent_entity_id', $2::text, 'parent_project_id', $3::text))`,
		forkApp, strconv.FormatInt(catalogueAgent, 10), "1"); err != nil {
		t.Fatal(err)
	}
	app := strconv.FormatInt(forkApp, 10)
	status, review := h.do(h.router, http.MethodGet, "/elitea_core/marketplace_upstream/prompt_lib/2/"+app, nil)
	if status != http.StatusOK || review["mode"] != "two_way" || review["base"] != nil {
		t.Fatalf("review = %d %v, want a two-way view with no base", status, review)
	}
	first := review["fields"].([]any)[0].(map[string]any)
	if first["key"] != "instructions" || first["status"] != "differs" || first["default"] != "ours" {
		t.Errorf("first field = %v, want instructions differing, ours by default", first)
	}
	// Merging records the fork's first origin version, at low tier applied by
	// the person who merged.
	status, body := h.do(h.router, http.MethodPost, "/elitea_core/marketplace_upstream/prompt_lib/2/"+app, map[string]any{"resolutions": map[string]any{
		"instructions": map[string]any{"choice": "theirs"},
	}})
	if status != http.StatusCreated || body["applied"] != true {
		t.Fatalf("merge = %d %v", status, body)
	}
	_, origin := h.do(h.router, http.MethodGet, "/elitea_core/marketplace_origin/prompt_lib/2/"+app, nil)
	if origin["mode"] != "forked" || origin["base_known"] != true || origin["version_id"] == "" {
		t.Errorf("origin after the first merge = %v, want a recorded origin version", origin)
	}
}
