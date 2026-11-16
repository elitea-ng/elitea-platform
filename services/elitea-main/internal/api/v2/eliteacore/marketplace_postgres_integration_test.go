package eliteacore_test

// Real-PostgreSQL coverage for the Marketplace (ADR-0051 decisions 2 and 3,
// issue 1629): the listing and snapshot a publish records, the facet search, the
// hire (Install and Fork) with bindings, the approval of a hire above low tier,
// the personal project rule, and the unpublish that delists without deleting
// what a project holds.
//
// The schema is the production migration chain, as catalog_mirror's test builds
// it: p_1 is the public project and p_2 an ordinary project. Every assertion
// reads back through the route the UI calls or reads the rows the route wrote.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/marketplace"
)

const marketplacePublisher = 7

type marketplaceHarness struct {
	t         *testing.T
	pool      *pgxpool.Pool
	handler   *eliteacore.Handler
	lifecycle *repos.AgentLifecycle
	router    chi.Router
}

func newMarketplaceHarness(t *testing.T) *marketplaceHarness {
	t.Helper()
	pool := newCatalogMirrorPool(t)
	lifecycle, err := repos.ComposeAgentLifecycle(pool, nil, nil)
	if err != nil {
		t.Fatalf("compose the agent lifecycle: %v", err)
	}
	handler := eliteacore.NewHandler(pool, eliteacore.WithPublishGovernor(lifecycle),
		eliteacore.WithWorkforceManagerCheck(managesAgents))
	lifecycle.ComposePublisher(handler)
	lifecycle.ComposeMarketplaceApplier(handler)
	h := &marketplaceHarness{t: t, pool: pool, handler: handler, lifecycle: lifecycle}
	h.router = h.newRouter(marketplacePublisher)
	return h
}

// newRouter mounts the Marketplace and publish routes as the caller.
func (h *marketplaceHarness) newRouter(userID int) chi.Router {
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user := auth.User{ID: strconv.Itoa(userID)}
			next.ServeHTTP(w, r.WithContext(auth.ContextWithUser(r.Context(), user)))
		})
	})
	router.Post("/elitea_core/fork/prompt_lib/{projectID}", h.handler.Fork)
	router.Post("/elitea_core/publish/prompt_lib/{projectID}/{versionID}", h.handler.Publish)
	router.Post("/elitea_core/unpublish/prompt_lib/{projectID}/{versionID}", h.handler.Unpublish)
	router.Get("/elitea_core/marketplace_listings/prompt_lib/{projectID}", h.handler.MarketplaceSearch)
	router.Get("/elitea_core/marketplace_listing/prompt_lib/{projectID}/{listingID}", h.handler.MarketplaceListing)
	router.Patch("/elitea_core/marketplace_listing/prompt_lib/{projectID}/{listingID}", h.handler.MarketplaceListingUpdate)
	router.Post("/elitea_core/marketplace_listing_version/prompt_lib/{projectID}/{listingID}/{versionID}", h.handler.MarketplaceVersionState)
	router.Post("/elitea_core/marketplace_hire/prompt_lib/{projectID}", h.handler.MarketplaceHire)
	router.Get("/elitea_core/marketplace_origin/prompt_lib/{projectID}/{applicationID}", h.handler.MarketplaceOrigin)
	router.Get("/elitea_core/marketplace_upgrade/prompt_lib/{projectID}/{applicationID}", h.handler.MarketplaceUpgradePlan)
	router.Post("/elitea_core/marketplace_upgrade/prompt_lib/{projectID}/{applicationID}", h.handler.MarketplaceUpgrade)
	router.Put("/elitea_core/marketplace_upgrade_policy/prompt_lib/{projectID}/{applicationID}", h.handler.MarketplaceUpgradePolicy)
	router.Get("/elitea_core/marketplace_upstream/prompt_lib/{projectID}/{applicationID}", h.handler.MarketplaceUpstream)
	router.Post("/elitea_core/marketplace_upstream/prompt_lib/{projectID}/{applicationID}", h.handler.MarketplaceUpstream)
	return router
}

func (h *marketplaceHarness) do(router chi.Router, method, target string, body any) (int, map[string]any) {
	h.t.Helper()
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		encoded, err := json.Marshal(body)
		if err != nil {
			h.t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	request := httptest.NewRequest(method, target, reader)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	var decoded map[string]any
	_ = json.Unmarshal(recorder.Body.Bytes(), &decoded) // an error body may not be an object
	return recorder.Code, decoded
}

// agent seeds one agent with a draft version in a project, with the toolkits
// named by tools (type to selected tool names).
type seededAgent struct {
	projectID int
	appID     int
	versionID int
}

func (h *marketplaceHarness) seedAgent(projectID int, name string, tools map[string][]string) seededAgent {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	schema := fmt.Sprintf("p_%d", projectID)
	agent := seededAgent{projectID: projectID}
	if err := h.pool.QueryRow(ctx, fmt.Sprintf(`
INSERT INTO %s.applications (name, description, owner_id) VALUES ($1, 'seeded for the marketplace', $2) RETURNING id`, schema),
		name, projectID).Scan(&agent.appID); err != nil {
		h.t.Fatalf("seed application: %v", err)
	}
	if err := h.pool.QueryRow(ctx, fmt.Sprintf(`
INSERT INTO %s.application_versions (application_id, name, status, author_id, instructions, agent_type, welcome_message, meta)
VALUES ($1, 'latest', 'draft', $2, 'summarise the release', 'agent', 'hello', '{"category":"Development"}'::jsonb)
RETURNING id`, schema), agent.appID, marketplacePublisher).Scan(&agent.versionID); err != nil {
		h.t.Fatalf("seed application version: %v", err)
	}
	for toolType, selected := range tools {
		var toolID int
		selectedJSON, _ := json.Marshal(selected)
		if err := h.pool.QueryRow(ctx, fmt.Sprintf(`
INSERT INTO %s.elitea_tools (name, type, owner_id, author_id, settings)
VALUES ($1, $2, $3, $4, '{"token":"never-copied"}'::jsonb) RETURNING id`, schema),
			toolType+" toolkit", toolType, projectID, marketplacePublisher).Scan(&toolID); err != nil {
			h.t.Fatalf("seed toolkit: %v", err)
		}
		if _, err := h.pool.Exec(ctx, fmt.Sprintf(`
INSERT INTO %s.entity_tool_mapping (entity_version_id, entity_id, entity_type, tool_id, selected_tools)
VALUES ($1, $2, 'agent', $3, $4::jsonb)`, schema), agent.versionID, agent.appID, toolID, string(selectedJSON)); err != nil {
			h.t.Fatalf("seed tool mapping: %v", err)
		}
	}
	return agent
}

func (h *marketplaceHarness) publish(agent seededAgent, versionName, notes string) {
	h.t.Helper()
	token := eliteacore.NewHandler(h.pool).PublishValidationTokenFor(
		context.Background(), fmt.Sprintf("p_%d", agent.projectID), strconv.Itoa(agent.versionID))
	status, body := h.do(h.router, http.MethodPost,
		fmt.Sprintf("/elitea_core/publish/prompt_lib/%d/%d", agent.projectID, agent.versionID),
		map[string]any{"version_name": versionName, "validation_token": token, "category": "Development", "release_notes": notes})
	if status != http.StatusOK {
		h.t.Fatalf("publish status = %d, body = %v", status, body)
	}
}

func (h *marketplaceHarness) search(query string) map[string]any {
	h.t.Helper()
	status, body := h.do(h.router, http.MethodGet, "/elitea_core/marketplace_listings/prompt_lib/2"+query, nil)
	if status != http.StatusOK {
		h.t.Fatalf("search %q status = %d, body = %v", query, status, body)
	}
	return body
}

func rowsOf(body map[string]any) []map[string]any {
	out := []map[string]any{}
	rows, _ := body["rows"].([]any)
	for _, row := range rows {
		if m, ok := row.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func (h *marketplaceHarness) listingID(name string) string {
	h.t.Helper()
	for _, row := range rowsOf(h.search("?query=" + strings.ReplaceAll(name, " ", "+"))) {
		if row["name"] == name {
			return row["id"].(string)
		}
	}
	h.t.Fatalf("no listing named %q", name)
	return ""
}

func (h *marketplaceHarness) detail(listingID string) map[string]any {
	h.t.Helper()
	status, body := h.do(h.router, http.MethodGet, "/elitea_core/marketplace_listing/prompt_lib/2/"+listingID, nil)
	if status != http.StatusOK {
		h.t.Fatalf("listing %s status = %d, body = %v", listingID, status, body)
	}
	return body
}

func versionsOf(detail map[string]any) []map[string]any {
	out := []map[string]any{}
	versions, _ := detail["versions"].([]any)
	for _, v := range versions {
		out = append(out, v.(map[string]any))
	}
	return out
}

// TestPublishRecordsTheListingSnapshotAndRequirements is the acceptance test for
// the publish half: a publish leaves a listing, a version with release notes, a
// digest and requirements that name toolkit types and tools and never a
// credential.
func TestPublishRecordsTheListingSnapshotAndRequirements(t *testing.T) {
	h := newMarketplaceHarness(t)
	agent := h.seedAgent(2, "Changelog Writer", map[string][]string{"github": {"list_issues", "create_issue"}})
	h.publish(agent, "v1", "First release.")

	id := h.listingID("Changelog Writer")
	detail := h.detail(id)
	versions := versionsOf(detail)
	if len(versions) != 1 {
		t.Fatalf("versions = %v, want 1", versions)
	}
	v := versions[0]
	if v["name"] != "v1" || v["state"] != "current" || v["release_notes"] != "First release." {
		t.Errorf("version = %v", v)
	}
	if digest, _ := v["digest"].(string); len(digest) != 64 {
		t.Errorf("digest = %q, want 64 hex characters", digest)
	}
	reqs, _ := v["requirements"].(map[string]any)
	if reqs["model_class"] != "tool_calling" || reqs["has_write_tools"] != true {
		t.Errorf("requirements = %v, want tool_calling with write tools", reqs)
	}
	items, _ := reqs["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("requirement items = %v", items)
	}
	item := items[0].(map[string]any)
	if item["kind"] != "toolkit" || item["type"] != "github" {
		t.Errorf("requirement = %v", item)
	}
	raw, _ := json.Marshal(detail)
	if strings.Contains(string(raw), "never-copied") {
		t.Errorf("the listing page carries a toolkit setting: %s", raw)
	}
	var definition string
	if err := h.pool.QueryRow(context.Background(),
		`SELECT definition::text FROM elitea_marketplace.listing_versions`).Scan(&definition); err != nil {
		t.Fatalf("read the snapshot: %v", err)
	}
	if strings.Contains(definition, "never-copied") {
		t.Errorf("the snapshot carries a toolkit setting: %s", definition)
	}
}

// TestFacetSearch walks the filters, the sorts and the counts, and proves that
// a LIKE metacharacter in the query is a literal.
func TestFacetSearch(t *testing.T) {
	h := newMarketplaceHarness(t)
	h.publish(h.seedAgent(2, "Changelog Writer", map[string][]string{"github": {"create_issue"}}), "v1", "")
	h.publish(h.seedAgent(2, "Flake Finder", map[string][]string{"jira": {"search_issues"}}), "v1", "")
	h.publish(h.seedAgent(2, "Plain Helper", nil), "v1", "")

	all := h.search("")
	if int(all["total"].(float64)) != 3 {
		t.Fatalf("total = %v, want 3", all["total"])
	}
	if n := len(rowsOf(h.search("?requires=github"))); n != 1 {
		t.Errorf("requires=github = %d rows, want 1", n)
	}
	if n := len(rowsOf(h.search("?query=flake"))); n != 1 {
		t.Errorf("query=flake = %d rows, want 1", n)
	}
	for _, literal := range []string{"%25", "_", "%25%25"} {
		if n := len(rowsOf(h.search("?query=" + literal))); n != 0 {
			t.Errorf("query=%s matched %d rows; a LIKE metacharacter must be literal", literal, n)
		}
	}
	if n := len(rowsOf(h.search("?kind=skill"))); n != 0 {
		t.Errorf("kind=skill = %d rows, want 0", n)
	}
	names := []string{}
	for _, row := range rowsOf(h.search("?sort=name")) {
		names = append(names, row["name"].(string))
	}
	if strings.Join(names, ",") != "Changelog Writer,Flake Finder,Plain Helper" {
		t.Errorf("sort=name = %v", names)
	}
	facets := all["facets"].(map[string]any)
	if len(facets["required_types"].([]any)) != 2 {
		t.Errorf("required_types facet = %v, want github and jira", facets["required_types"])
	}
	// The requires facet ignores its own filter, so a click shows the others.
	narrowed := h.search("?requires=github")["facets"].(map[string]any)
	if len(narrowed["required_types"].([]any)) != 2 {
		t.Errorf("required_types facet under requires=github = %v, want both types", narrowed["required_types"])
	}
	for _, bad := range []string{"?sort=best", "?kind=pipeline", "?max_tier=extreme", "?min_rating=9", "?limit=1000"} {
		status, _ := h.do(h.router, http.MethodGet, "/elitea_core/marketplace_listings/prompt_lib/2"+bad, nil)
		if status != http.StatusBadRequest {
			t.Errorf("%s answered %d, want 400", bad, status)
		}
	}
}

func hireBody(listingID, versionID, mode string, bindings ...map[string]any) map[string]any {
	return map[string]any{"listing_id": listingID, "version_id": versionID, "mode": mode, "bindings": bindings}
}

func (h *marketplaceHarness) currentVersionID(listingID string) string {
	h.t.Helper()
	for _, v := range versionsOf(h.detail(listingID)) {
		if v["state"] == "current" {
			return v["id"].(string)
		}
	}
	h.t.Fatalf("listing %s has no current version", listingID)
	return ""
}

func (h *marketplaceHarness) projectToolkit(projectID int, toolType string) int {
	h.t.Helper()
	var id int
	if err := h.pool.QueryRow(context.Background(), fmt.Sprintf(`
INSERT INTO p_%d.elitea_tools (name, type, owner_id, author_id) VALUES ($1, $2, $3, 1) RETURNING id`, projectID),
		"project "+toolType, toolType, projectID).Scan(&id); err != nil {
		h.t.Fatalf("seed a project toolkit: %v", err)
	}
	return id
}

// TestInstallPinsTheVersionAndIsReadOnly walks T1: hire a read-only agent. The
// agent is created from the snapshot, with the binding, and its origin.
func TestInstallPinsTheVersionAndIsReadOnly(t *testing.T) {
	h := newMarketplaceHarness(t)
	agent := h.seedAgent(2, "Issue Reader", map[string][]string{"github": {"list_issues"}})
	h.publish(agent, "v1", "")
	id := h.listingID("Issue Reader")
	versionID := h.currentVersionID(id)
	toolkit := h.projectToolkit(2, "github")
	var key string
	for _, item := range h.detail(id)["versions"].([]any)[0].(map[string]any)["requirements"].(map[string]any)["items"].([]any) {
		key = item.(map[string]any)["key"].(string)
	}

	// An unbound required toolkit refuses the hire.
	status, body := h.do(h.router, http.MethodPost, "/elitea_core/marketplace_hire/prompt_lib/2", hireBody(id, versionID, "install"))
	if status != http.StatusConflict || body["error"] != "binding_missing" {
		t.Fatalf("unbound hire = %d %v, want 409 binding_missing", status, body)
	}
	// A binding to the wrong type is refused.
	wrong := h.projectToolkit(2, "jira")
	status, body = h.do(h.router, http.MethodPost, "/elitea_core/marketplace_hire/prompt_lib/2",
		hireBody(id, versionID, "install", map[string]any{"requirement_key": key, "toolkit_id": strconv.Itoa(wrong)}))
	if status != http.StatusBadRequest || body["error"] != "binding_invalid" {
		t.Fatalf("mistyped binding = %d %v, want 400 binding_invalid", status, body)
	}

	status, body = h.do(h.router, http.MethodPost, "/elitea_core/marketplace_hire/prompt_lib/2",
		hireBody(id, versionID, "install", map[string]any{"requirement_key": key, "toolkit_id": strconv.Itoa(toolkit)}))
	if status != http.StatusCreated {
		t.Fatalf("hire = %d %v", status, body)
	}
	if body["tools_enabled"] != true || body["awaiting_approval"] != false {
		t.Errorf("a low-tier read-only hire = %v, want tools enabled with no approval", body)
	}
	appID := body["application_id"].(string)

	ctx := context.Background()
	var meta, instructions string
	if err := h.pool.QueryRow(ctx, `SELECT meta::text, instructions FROM p_2.application_versions WHERE application_id = $1`, appID).
		Scan(&meta, &instructions); err != nil {
		t.Fatalf("read the installed version: %v", err)
	}
	if instructions != "summarise the release" {
		t.Errorf("instructions = %q, built from the snapshot", instructions)
	}
	for _, want := range []string{`"origin_kind": "installed"`, `"origin_listing_id": "` + id + `"`, `"origin_version_id": "` + versionID + `"`} {
		if !strings.Contains(meta, want) {
			t.Errorf("meta = %s, want it to contain %s", meta, want)
		}
	}
	if strings.Contains(meta, "parent_entity_id") {
		t.Errorf("an install is not a fork: %s", meta)
	}
	var mapped int
	if err := h.pool.QueryRow(ctx, `
SELECT count(*) FROM p_2.entity_tool_mapping m JOIN p_2.application_versions v ON v.id = m.entity_version_id
WHERE v.application_id = $1 AND m.tool_id = $2`, appID, toolkit).Scan(&mapped); err != nil || mapped != 1 {
		t.Errorf("the project toolkit is bound %d times (%v), want 1", mapped, err)
	}

	// The registry row exists at the listing's tier, and no change request was needed.
	var tier string
	if err := h.pool.QueryRow(ctx, `SELECT risk_tier FROM elitea_lifecycle.agent_registry WHERE tenant_project_id = 2 AND application_id = $1`, appID).
		Scan(&tier); err != nil || tier != "low" {
		t.Errorf("registry tier = %q (%v), want low", tier, err)
	}

	// The origin route and the hired-here facet report it.
	status, origin := h.do(h.router, http.MethodGet, "/elitea_core/marketplace_origin/prompt_lib/2/"+appID, nil)
	if status != http.StatusOK || origin["mode"] != "installed" || origin["version_state"] != "available" {
		t.Errorf("origin = %d %v", status, origin)
	}
	if n := len(rowsOf(h.search("?hired_here=true"))); n != 1 {
		t.Errorf("hired_here = %d rows, want 1", n)
	}
	appIDInt, _ := strconv.ParseInt(appID, 10, 64)
	if !marketplace.IsInstalledApplication(ctx, h.pool, "p_2", appIDInt) {
		t.Errorf("the application is not reported as installed")
	}

	// A save that repeats the stored definition is no change; a different
	// instruction is.
	var versionRow int64
	if err := h.pool.QueryRow(ctx, `SELECT id FROM p_2.application_versions WHERE application_id = $1`, appID).Scan(&versionRow); err != nil {
		t.Fatal(err)
	}
	if marketplace.DefinitionChanged(ctx, h.pool, "p_2", versionRow, map[string]any{"instructions": "summarise the release", "welcome_message": "hello"}) {
		t.Errorf("a save that repeats the stored definition counted as a change")
	}
	if !marketplace.DefinitionChanged(ctx, h.pool, "p_2", versionRow, map[string]any{"instructions": "something else"}) {
		t.Errorf("a changed instruction was not seen as a change")
	}

	// A new listing version leaves the installed agent unchanged.
	if _, err := h.pool.Exec(ctx, `UPDATE p_2.application_versions SET instructions = 'v2 text' WHERE id = $1`, agent.versionID); err != nil {
		t.Fatal(err)
	}
	h.publish(agent, "v2", "Changed the text.")
	if err := h.pool.QueryRow(ctx, `SELECT instructions FROM p_2.application_versions WHERE id = $1`, versionRow).Scan(&instructions); err != nil ||
		instructions != "summarise the release" {
		t.Errorf("the installed instructions = %q (%v), want the pinned text", instructions, err)
	}
	_, origin = h.do(h.router, http.MethodGet, "/elitea_core/marketplace_origin/prompt_lib/2/"+appID, nil)
	if latest, _ := origin["latest_version"].(map[string]any); latest["name"] != "v2" {
		t.Errorf("latest_version = %v, want v2", origin["latest_version"])
	}
}

// TestHireAboveLowTierNeedsApproval is the gate: a medium-tier agent, or one
// with a write tool, is created with every tool disabled and a hire change
// request, and the final approval enables the tools.
func TestHireAboveLowTierNeedsApproval(t *testing.T) {
	h := newMarketplaceHarness(t)
	agent := h.seedAgent(2, "Release Writer", map[string][]string{"github": {"create_release"}})
	h.publish(agent, "v1", "")
	id := h.listingID("Release Writer")
	versionID := h.currentVersionID(id)
	toolkit := h.projectToolkit(2, "github")
	var key string
	for _, item := range h.detail(id)["versions"].([]any)[0].(map[string]any)["requirements"].(map[string]any)["items"].([]any) {
		key = item.(map[string]any)["key"].(string)
	}

	// The writing hirer is 11, a member of a shared project (not personal).
	hirer := h.newRouter(11)
	status, body := h.do(hirer, http.MethodPost, "/elitea_core/marketplace_hire/prompt_lib/2",
		hireBody(id, versionID, "install", map[string]any{"requirement_key": key, "toolkit_id": strconv.Itoa(toolkit)}))
	if status != http.StatusCreated {
		t.Fatalf("hire = %d %v", status, body)
	}
	if body["tools_enabled"] != false || body["awaiting_approval"] != true || body["change_request_id"] == nil {
		t.Fatalf("a write-tool hire = %v, want tools disabled and a change request", body)
	}
	appID, _ := strconv.ParseInt(body["application_id"].(string), 10, 64)
	changeID, _ := strconv.ParseInt(body["change_request_id"].(string), 10, 64)

	ctx := context.Background()
	var mapped int
	if err := h.pool.QueryRow(ctx, `
SELECT count(*) FROM p_2.entity_tool_mapping m JOIN p_2.application_versions v ON v.id = m.entity_version_id
WHERE v.application_id = $1`, appID).Scan(&mapped); err != nil || mapped != 0 {
		t.Fatalf("tools attached before approval = %d (%v), want 0", mapped, err)
	}
	var kind, state string
	if err := h.pool.QueryRow(ctx, `SELECT kind, state FROM elitea_lifecycle.change_requests WHERE id = $1`, changeID).
		Scan(&kind, &state); err != nil || kind != "hire" || state != "submitted" {
		t.Fatalf("change request = %q %q (%v), want a submitted hire", kind, state, err)
	}

	// The proposer cannot approve; another person can.
	if _, err := h.lifecycle.ApproveChangeRequest(ctx, repos.Approval{ProjectID: 2, ApplicationID: appID, ChangeID: changeID, ApproverID: 11}); err == nil {
		t.Fatal("the hirer approved their own hire")
	}
	outcome, err := h.lifecycle.ApproveChangeRequest(ctx, repos.Approval{ProjectID: 2, ApplicationID: appID, ChangeID: changeID, ApproverID: 12, Governance: true})
	if err != nil {
		t.Fatalf("approve the hire: %v", err)
	}
	if outcome.Change.State != agentlifecycle.ChangeApproved {
		t.Fatalf("hire state after approval = %q (complete=%v)", outcome.Change.State, outcome.Complete)
	}
	if err := h.pool.QueryRow(ctx, `
SELECT count(*) FROM p_2.entity_tool_mapping m JOIN p_2.application_versions v ON v.id = m.entity_version_id
WHERE v.application_id = $1 AND m.tool_id = $2`, appID, toolkit).Scan(&mapped); err != nil || mapped != 1 {
		t.Errorf("tools attached after approval = %d (%v), want 1", mapped, err)
	}
	var enabled bool
	if err := h.pool.QueryRow(ctx, `SELECT tools_enabled FROM elitea_marketplace.installs WHERE entity_id = $1`, appID).Scan(&enabled); err != nil || !enabled {
		t.Errorf("tools_enabled = %v (%v), want true", enabled, err)
	}
}

// TestHireIsRefusedForAYankedVersion proves the version states: deprecated is
// hireable, yanked is not, and the publisher alone restores their own yank.
func TestHireIsRefusedForAYankedVersion(t *testing.T) {
	h := newMarketplaceHarness(t)
	agent := h.seedAgent(2, "Plain Helper", nil)
	h.publish(agent, "v1", "")
	id := h.listingID("Plain Helper")
	versionID := h.currentVersionID(id)
	stateURL := "/elitea_core/marketplace_listing_version/prompt_lib/2/" + id + "/" + versionID

	if status, _ := h.do(h.router, http.MethodPost, stateURL, map[string]any{"state": "deprecated", "reason": "old"}); status != http.StatusOK {
		t.Fatalf("deprecate = %d", status)
	}
	if status, body := h.do(h.router, http.MethodPost, "/elitea_core/marketplace_hire/prompt_lib/2", hireBody(id, versionID, "install")); status != http.StatusCreated {
		t.Fatalf("a deprecated version is hireable: %d %v", status, body)
	}
	if status, _ := h.do(h.router, http.MethodPost, stateURL, map[string]any{"state": "yanked", "reason": "bad"}); status != http.StatusOK {
		t.Fatalf("yank = %d", status)
	}
	status, body := h.do(h.router, http.MethodPost, "/elitea_core/marketplace_hire/prompt_lib/2", hireBody(id, versionID, "install"))
	if status != http.StatusConflict || body["error"] != "version_not_hireable" {
		t.Fatalf("hire of a yanked version = %d %v, want 409 version_not_hireable", status, body)
	}
	// A stranger may not change the state.
	stranger := h.newRouter(99)
	if status, _ := h.do(stranger, http.MethodPost, stateURL, map[string]any{"state": "available"}); status != http.StatusForbidden {
		t.Errorf("a stranger changed a version state: %d", status)
	}
	if status, _ := h.do(h.router, http.MethodPost, stateURL, map[string]any{"state": "bogus"}); status != http.StatusBadRequest {
		t.Errorf("an unknown state answered %d, want 400", status)
	}
}

// TestForkRecordsTheOriginVersion is the fork half of the origin: the fork is
// editable and records the listing, the version and the digest.
func TestForkRecordsTheOriginVersion(t *testing.T) {
	h := newMarketplaceHarness(t)
	agent := h.seedAgent(2, "Plain Helper", nil)
	h.publish(agent, "v1", "")
	id := h.listingID("Plain Helper")
	versionID := h.currentVersionID(id)

	status, body := h.do(h.router, http.MethodPost, "/elitea_core/marketplace_hire/prompt_lib/2", hireBody(id, versionID, "fork"))
	if status != http.StatusCreated {
		t.Fatalf("fork = %d %v", status, body)
	}
	appID := body["application_id"].(string)
	var meta string
	if err := h.pool.QueryRow(context.Background(), `SELECT meta::text FROM p_2.application_versions WHERE application_id = $1`, appID).Scan(&meta); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"origin_kind": "forked"`, `"origin_listing_id": "` + id + `"`, `"origin_version_id": "` + versionID + `"`, `"origin_digest"`, `"parent_entity_id"`} {
		if !strings.Contains(meta, want) {
			t.Errorf("fork meta = %s, want %s", meta, want)
		}
	}
	appIDInt, _ := strconv.ParseInt(appID, 10, 64)
	if marketplace.IsInstalledApplication(context.Background(), h.pool, "p_2", appIDInt) {
		t.Errorf("a fork is editable, but the application reads as installed")
	}
}

// TestForkOfAPublishedVersionRecordsItsListingVersion is the other fork path:
// the export-based Handler.Fork, whose copy recorded no source version until
// now. A fork of a catalogue version that has a listing records the listing, the
// version and the digest, and counts as a hire.
func TestForkOfAPublishedVersionRecordsItsListingVersion(t *testing.T) {
	h := newMarketplaceHarness(t)
	h.publish(h.seedAgent(2, "Plain Helper", nil), "v1", "")
	id := h.listingID("Plain Helper")
	versionID := h.currentVersionID(id)
	var twinApp string
	if err := h.pool.QueryRow(context.Background(), `SELECT entity_id::text FROM elitea_marketplace.listings WHERE id = $1`, id).Scan(&twinApp); err != nil {
		t.Fatal(err)
	}
	status, body := h.do(h.router, http.MethodPost, "/elitea_core/fork/prompt_lib/2", map[string]any{
		"applications": []any{map[string]any{
			"id": twinApp, "owner_id": "1", "name": "Forked Helper", "description": "d",
			"versions": []any{map[string]any{"id": versionID, "name": "v1", "agent_type": "agent", "instructions": "do the thing"}},
		}},
	})
	if status != http.StatusCreated {
		t.Fatalf("fork = %d %v", status, body)
	}
	var meta string
	if err := h.pool.QueryRow(context.Background(),
		`SELECT v.meta::text FROM p_2.application_versions v JOIN p_2.applications a ON a.id = v.application_id WHERE a.name = 'Forked Helper'`).Scan(&meta); err != nil {
		t.Fatalf("read the fork: %v", err)
	}
	for _, want := range []string{`"origin_kind": "forked"`, `"origin_listing_id": "` + id + `"`, `"origin_version_id": "` + versionID + `"`, `"origin_listing_name": "Plain Helper"`} {
		if !strings.Contains(meta, want) {
			t.Errorf("fork meta = %s, want %s", meta, want)
		}
	}
	var installs int
	if err := h.pool.QueryRow(context.Background(), `SELECT count(*) FROM elitea_marketplace.installs WHERE mode = 'fork'`).Scan(&installs); err != nil || installs != 1 {
		t.Errorf("fork hires recorded = %d (%v), want 1", installs, err)
	}
}

// TestPersonalProjectRefusesOrSelfApproves covers the third rule of the hire:
// a personal project has no second approver.
func TestPersonalProjectRefusesOrSelfApproves(t *testing.T) {
	h := newMarketplaceHarness(t)
	agent := h.seedAgent(2, "Release Writer", map[string][]string{"github": {"create_release"}})
	h.publish(agent, "v1", "")
	id := h.listingID("Release Writer")
	versionID := h.currentVersionID(id)
	ctx := context.Background()
	if _, err := h.pool.Exec(ctx, `UPDATE centry.project SET name = 'project_user_11' WHERE id = 2`); err != nil {
		t.Fatalf("make project 2 personal: %v", err)
	}
	toolkit := h.projectToolkit(2, "github")
	var key string
	for _, item := range h.detail(id)["versions"].([]any)[0].(map[string]any)["requirements"].(map[string]any)["items"].([]any) {
		key = item.(map[string]any)["key"].(string)
	}
	hire := hireBody(id, versionID, "install", map[string]any{"requirement_key": key, "toolkit_id": strconv.Itoa(toolkit)})
	hirer := h.newRouter(11)

	status, body := h.do(hirer, http.MethodPost, "/elitea_core/marketplace_hire/prompt_lib/2", hire)
	if status != http.StatusConflict || body["error"] != "hire_needs_shared_project" {
		t.Fatalf("personal hire = %d %v, want 409 hire_needs_shared_project", status, body)
	}
	var apps int
	if err := h.pool.QueryRow(ctx, `SELECT count(*) FROM p_2.applications WHERE name = 'Release Writer' AND id <> $1`, agent.appID).Scan(&apps); err != nil || apps != 0 {
		t.Fatalf("a refused hire left %d agents (%v)", apps, err)
	}

	if _, err := h.pool.Exec(ctx, `
INSERT INTO centry.platform_config (section, key, value, updated_at)
VALUES ('agent_lifecycle', 'marketplace_personal_self_approve', 'true'::jsonb, now())`); err != nil {
		t.Fatalf("turn the policy on: %v", err)
	}
	status, body = h.do(hirer, http.MethodPost, "/elitea_core/marketplace_hire/prompt_lib/2", hire)
	if status != http.StatusCreated || body["tools_enabled"] != true || body["awaiting_approval"] != false {
		t.Fatalf("self-approved hire = %d %v", status, body)
	}
	var selfApproved bool
	var state string
	if err := h.pool.QueryRow(ctx, `SELECT self_approved, state FROM elitea_lifecycle.change_requests WHERE kind = 'hire'`).Scan(&selfApproved, &state); err != nil ||
		!selfApproved || state != "approved" {
		t.Errorf("hire request = self_approved %v state %q (%v), want an approved self-approval", selfApproved, state, err)
	}
	var audited int
	if err := h.pool.QueryRow(ctx, `
SELECT count(*) FROM elitea_runtime.security_audit_outbox WHERE event::text LIKE '%marketplace_personal_self_approve%'`).Scan(&audited); err != nil {
		t.Fatalf("read the audit outbox: %v", err)
	}
	if audited == 0 {
		t.Errorf("the self-approval left no audit event")
	}
}

// TestUnpublishDelistsAndKeepsWhatAProjectHolds is the retention rule: the chat
// twin is deleted, the listing leaves search, and the snapshot stays while a
// project has it installed; the installed agent still runs on it.
func TestUnpublishDelistsAndKeepsWhatAProjectHolds(t *testing.T) {
	h := newMarketplaceHarness(t)
	agent := h.seedAgent(2, "Plain Helper", nil)
	h.publish(agent, "v1", "")
	id := h.listingID("Plain Helper")
	versionID := h.currentVersionID(id)
	status, body := h.do(h.router, http.MethodPost, "/elitea_core/marketplace_hire/prompt_lib/2", hireBody(id, versionID, "install"))
	if status != http.StatusCreated {
		t.Fatalf("hire = %d %v", status, body)
	}
	appID := body["application_id"].(string)

	// Publish created the published clone in p_2; find it and unpublish it.
	var clone int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT id FROM p_2.application_versions WHERE application_id = $1 AND status = 'published'`, agent.appID).Scan(&clone); err != nil {
		t.Fatalf("find the published clone: %v", err)
	}
	status, body = h.do(h.router, http.MethodPost, fmt.Sprintf("/elitea_core/unpublish/prompt_lib/2/%d", clone), map[string]any{})
	if status != http.StatusOK {
		t.Fatalf("unpublish = %d %v", status, body)
	}

	if n := len(rowsOf(h.search(""))); n != 0 {
		t.Errorf("a delisted listing is in search: %d rows", n)
	}
	var twins int
	if err := h.pool.QueryRow(context.Background(), `SELECT count(*) FROM p_1.application_versions WHERE shared_owner_id = 2`).Scan(&twins); err != nil || twins != 0 {
		t.Errorf("chat twin versions after unpublish = %d (%v), want 0", twins, err)
	}
	var snapshots int
	if err := h.pool.QueryRow(context.Background(), `SELECT count(*) FROM elitea_marketplace.listing_versions WHERE withdrawn_at IS NOT NULL`).Scan(&snapshots); err != nil || snapshots != 1 {
		t.Errorf("withdrawn snapshots = %d (%v), want 1", snapshots, err)
	}
	// The project that has it hired still reads the listing, and it is delisted.
	detail := h.detail(id)
	if listing := detail["listing"].(map[string]any); listing["state"] != "delisted" {
		t.Errorf("listing state = %v, want delisted", listing["state"])
	}
	status, origin := h.do(h.router, http.MethodGet, "/elitea_core/marketplace_origin/prompt_lib/2/"+appID, nil)
	if status != http.StatusOK || origin["listing_state"] != "delisted" {
		t.Errorf("origin after unpublish = %d %v, want the withdrawn state", status, origin)
	}
	// The purge keeps a snapshot a project holds.
	if _, err := marketplace.PurgeUnheld(context.Background(), h.pool, -1); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if err := h.pool.QueryRow(context.Background(), `SELECT count(*) FROM elitea_marketplace.listing_versions`).Scan(&snapshots); err != nil || snapshots != 1 {
		t.Errorf("snapshots after purge = %d (%v), want the held one kept", snapshots, err)
	}
	// The same install row blocks a direct delete.
	if _, err := h.pool.Exec(context.Background(), `DELETE FROM elitea_marketplace.listing_versions`); err == nil {
		t.Errorf("a held snapshot was deleted")
	}
	// A second publish brings the listing back.
	var draft int
	if err := h.pool.QueryRow(context.Background(), `SELECT id FROM p_2.application_versions WHERE id = $1`, agent.versionID).Scan(&draft); err != nil {
		t.Fatal(err)
	}
	h.publish(agent, "v1b", "Again.")
	if n := len(rowsOf(h.search(""))); n != 1 {
		t.Errorf("a republished listing is back in search: %d rows, want 1", n)
	}
}

// TestListingsAreBuiltForVersionsPublishedBeforeTheMarketplace drops the rows a
// publish wrote and the backfill marker, and reads again: the first read builds
// them from the catalogue.
func TestListingsAreBuiltForVersionsPublishedBeforeTheMarketplace(t *testing.T) {
	h := newMarketplaceHarness(t)
	h.publish(h.seedAgent(2, "Changelog Writer", map[string][]string{"github": {"create_issue"}}), "v1", "")
	ctx := context.Background()
	if _, err := h.pool.Exec(ctx, `DELETE FROM elitea_marketplace.listings`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.pool.Exec(ctx, `DELETE FROM elitea_marketplace.backfills`); err != nil {
		t.Fatal(err)
	}
	// A new handler has not backfilled yet.
	fresh := &marketplaceHarness{t: t, pool: h.pool, handler: eliteacore.NewHandler(h.pool, eliteacore.WithPublishGovernor(h.lifecycle))}
	fresh.router = fresh.newRouter(marketplacePublisher)
	rows := rowsOf(fresh.search(""))
	if len(rows) != 1 || rows[0]["name"] != "Changelog Writer" {
		t.Fatalf("rows after the backfill = %v, want the published agent", rows)
	}
	if types, _ := rows[0]["required_types"].([]any); len(types) != 1 || types[0] != "github" {
		t.Errorf("backfilled required_types = %v, want github", rows[0]["required_types"])
	}
	// A second read does not duplicate it.
	if n := len(rowsOf(fresh.search(""))); n != 1 {
		t.Errorf("rows after a second read = %d, want 1", n)
	}
}

// TestFeaturedFlagNeedsAnAdministrator pins the editorial flag: without the
// administration permission the PATCH is refused.
func TestFeaturedFlagNeedsAnAdministrator(t *testing.T) {
	h := newMarketplaceHarness(t)
	h.publish(h.seedAgent(2, "Plain Helper", nil), "v1", "")
	id := h.listingID("Plain Helper")
	status, _ := h.do(h.router, http.MethodPatch, "/elitea_core/marketplace_listing/prompt_lib/2/"+id, map[string]any{"featured": true})
	if status != http.StatusForbidden {
		t.Errorf("featuring without the permission answered %d, want 403", status)
	}
	if ok, err := marketplace.SetFeatured(context.Background(), h.pool, mustInt(t, id), true, 1); err != nil || !ok {
		t.Fatalf("set featured: %v %v", ok, err)
	}
	if n := len(rowsOf(h.search("?featured=true"))); n != 1 {
		t.Errorf("featured filter = %d rows, want 1", n)
	}
}

// TestInstallCarriesNestedAgentsWithTheirOwnToolkits proves the snapshot keeps
// an agent used as a tool: the install writes it as an embedded agent wired to
// its parent, and the toolkit bound to its requirement attaches to the child.
func TestInstallCarriesNestedAgentsWithTheirOwnToolkits(t *testing.T) {
	h := newMarketplaceHarness(t)
	ctx := context.Background()
	child := h.seedAgent(2, "Child Helper", map[string][]string{"jira": {"search_issues"}})
	parent := h.seedAgent(2, "Parent Helper", nil)
	// The parent uses the child as a tool, as the agent relation route writes it.
	var toolID int
	if err := h.pool.QueryRow(ctx, `
INSERT INTO p_2.elitea_tools (name, type, owner_id, author_id, settings)
VALUES ('Child Helper', 'application', 2, $1, jsonb_build_object('application_id', $2::int, 'application_version_id', $3::int))
RETURNING id`, marketplacePublisher, child.appID, child.versionID).Scan(&toolID); err != nil {
		t.Fatalf("seed the sub-agent tool: %v", err)
	}
	if _, err := h.pool.Exec(ctx, `
INSERT INTO p_2.entity_tool_mapping (entity_version_id, entity_id, entity_type, tool_id) VALUES ($1, $2, 'agent', $3)`,
		parent.versionID, parent.appID, toolID); err != nil {
		t.Fatalf("map the sub-agent tool: %v", err)
	}
	h.publish(parent, "v1", "")
	id := h.listingID("Parent Helper")
	versionID := h.currentVersionID(id)

	var key string
	kinds := map[string]bool{}
	for _, item := range h.detail(id)["versions"].([]any)[0].(map[string]any)["requirements"].(map[string]any)["items"].([]any) {
		m := item.(map[string]any)
		kinds[m["kind"].(string)] = true
		if m["kind"] == "toolkit" {
			key = m["key"].(string)
		}
	}
	if !kinds["agent"] || !kinds["toolkit"] || key == "" {
		t.Fatalf("requirements = %v, want the nested agent and its toolkit", kinds)
	}
	toolkit := h.projectToolkit(2, "jira")
	status, body := h.do(h.router, http.MethodPost, "/elitea_core/marketplace_hire/prompt_lib/2",
		hireBody(id, versionID, "install", map[string]any{"requirement_key": key, "toolkit_id": strconv.Itoa(toolkit)}))
	if status != http.StatusCreated {
		t.Fatalf("hire = %d %v", status, body)
	}
	appID := body["application_id"].(string)

	var embedded int
	if err := h.pool.QueryRow(ctx, `
SELECT count(*) FROM p_2.entity_tool_mapping m
JOIN p_2.application_versions pv ON pv.id = m.entity_version_id AND pv.application_id = $1
JOIN p_2.elitea_tools t ON t.id = m.tool_id AND t.type = 'application'
JOIN p_2.application_versions cv ON cv.id = (t.settings->>'application_version_id')::int
WHERE cv.status = 'embedded'`, appID).Scan(&embedded); err != nil || embedded != 1 {
		t.Fatalf("embedded sub-agents wired under the install = %d (%v), want 1", embedded, err)
	}
	var childTools int
	if err := h.pool.QueryRow(ctx, `
SELECT count(*) FROM p_2.entity_tool_mapping m
JOIN p_2.application_versions cv ON cv.id = m.entity_version_id AND cv.status = 'embedded'
WHERE m.tool_id = $1 AND cv.application_id NOT IN ($2, $3)`, toolkit, child.appID, parent.appID).Scan(&childTools); err != nil || childTools != 1 {
		t.Fatalf("the bound toolkit on the nested agent = %d (%v), want 1", childTools, err)
	}
}

func mustInt(t *testing.T, text string) int64 {
	t.Helper()
	n, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// seedSkillListing inserts a skill listing the way a publish would leave it,
// with its public twin id, and returns the listing id.
func (h *marketplaceHarness) seedSkillListing(name string, sourceEntity, twin int) string {
	h.t.Helper()
	var id int64
	if err := h.pool.QueryRow(context.Background(), `
INSERT INTO elitea_marketplace.listings (kind, source_project_id, source_entity_id, entity_id, name, search_text, publisher_id)
VALUES ('skill', 2, $1, $2, $3, lower($3), $4) RETURNING id`,
		sourceEntity, twin, name, marketplacePublisher).Scan(&id); err != nil {
		h.t.Fatalf("seed a skill listing: %v", err)
	}
	return strconv.FormatInt(id, 10)
}

func (h *marketplaceHarness) like(entityName string, entityID, userID int) {
	h.t.Helper()
	if _, err := h.pool.Exec(context.Background(), `
INSERT INTO p_1.social_likes (entity_name, entity_id, user_id, created_at) VALUES ($1, $2, $3, now() AT TIME ZONE 'UTC')`,
		entityName, entityID, userID); err != nil {
		h.t.Fatalf("seed a like: %v", err)
	}
}

// Skills epic 2169, S6 (ADR-0051 amendment 1, A1.5): a skill listing carries the
// like count and the caller's own like, read from the public project's
// social_likes under the entity name `skill`, and the Most liked sort orders it.
// An agent's like (entity `application`) must not count for a skill with the same
// twin id, and the other way round.
func TestSkillListingsCarryLikesAndSortByThem(t *testing.T) {
	h := newMarketplaceHarness(t)
	agent := h.seedAgent(2, "Liked Agent", nil)
	h.publish(agent, "v1", "")
	agentTwin, _ := strconv.Atoi(rowsOf(h.search("?kind=agent"))[0]["entity_id"].(string))

	h.seedSkillListing("Quiet Skill", 91, 501)
	h.seedSkillListing("Loved Skill", 92, 502)
	// The caller (7) likes both skills; user 8 also likes the second one.
	h.like("skill", 501, marketplacePublisher)
	h.like("skill", 502, marketplacePublisher)
	h.like("skill", 502, 8)
	// An agent like with the id of the first skill's twin must not count for the skill.
	h.like("application", 501, 9)
	// A skill like with the agent twin's id must not count for the agent.
	h.like("skill", agentTwin, 9)

	skills := rowsOf(h.search("?kind=skill&sort=likes"))
	if len(skills) != 2 || skills[0]["name"] != "Loved Skill" || skills[1]["name"] != "Quiet Skill" {
		t.Fatalf("kind=skill&sort=likes = %v, want Loved Skill then Quiet Skill", skills)
	}
	if skills[0]["likes"].(float64) != 2 || skills[1]["likes"].(float64) != 1 {
		t.Errorf("skill likes = %v and %v, want 2 and 1", skills[0]["likes"], skills[1]["likes"])
	}
	if skills[0]["is_liked"] != true || skills[1]["is_liked"] != true {
		t.Errorf("is_liked = %v and %v, want the caller's own likes", skills[0]["is_liked"], skills[1]["is_liked"])
	}
	agents := rowsOf(h.search("?kind=agent"))
	if len(agents) != 1 || agents[0]["likes"].(float64) != 0 || agents[0]["is_liked"] != false {
		t.Errorf("agent row = %v, want no likes (a skill like is not an agent like)", agents)
	}

	// The sort orders both kinds together when no kind is chosen.
	all := rowsOf(h.search("?sort=likes"))
	if len(all) != 3 || all[0]["name"] != "Loved Skill" {
		t.Fatalf("sort=likes = %v, want Loved Skill first", all)
	}
}
