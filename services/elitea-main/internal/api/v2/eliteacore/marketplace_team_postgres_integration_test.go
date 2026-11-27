package eliteacore_test

// Real-PostgreSQL coverage for saved teams in the Marketplace (ADR-0051
// amendment "Teams in the Marketplace", issue 3137; shared 0286): a team is
// published only when its members are listed, it is found with kind=team, its
// page resolves the members as their listings, and a hire (Install or Fork)
// creates the member agents and a saved team pinned to them, with the installs
// that record it.

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentteams"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/turntaking"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/marketplace"
)

func (h *marketplaceHarness) teamRouter() chi.Router {
	router := h.newRouter(marketplacePublisher)
	router.Post("/elitea_core/marketplace_team_publish/prompt_lib/{projectID}/{teamID}", h.handler.MarketplaceTeamPublish)
	router.Post("/elitea_core/marketplace_team_hire/prompt_lib/{projectID}", h.handler.MarketplaceTeamHire)
	return router
}

// seedTeam saves a team of the agents in project 2, the second one the lead.
func (h *marketplaceHarness) seedTeam(name string, agents ...seededAgent) int64 {
	h.t.Helper()
	team := agentteams.Team{Name: name, Description: "seeded team", Policy: turntaking.PolicyLead, Lead: len(agents)}
	for index, agent := range agents {
		team.Members = append(team.Members, agentteams.Member{
			ApplicationID: int64(agent.appID), ProjectID: int64(agent.projectID), VersionID: int64(agent.versionID),
			Name: fmt.Sprintf("member %d", index+1),
		})
	}
	if err := team.Normalize(); err != nil {
		h.t.Fatalf("normalize the team: %v", err)
	}
	stored, err := repos.NewAgentTeams(h.pool).CreateTeam(context.Background(), 2, marketplacePublisher, team)
	if err != nil {
		h.t.Fatalf("save the team: %v", err)
	}
	return stored.ID
}

func TestMarketplaceTeamPublishAndHire(t *testing.T) {
	h := newMarketplaceHarness(t)
	router := h.teamRouter()
	ctx := context.Background()
	writer := h.seedAgent(2, "Team Writer", map[string][]string{"github": {"list_issues"}})
	reviewer := h.seedAgent(2, "Team Reviewer", nil)
	teamID := h.seedTeam("Release Crew", writer, reviewer)
	publishURL := fmt.Sprintf("/elitea_core/marketplace_team_publish/prompt_lib/2/%d", teamID)

	// No member is listed: the publish is refused and names both.
	status, body := h.do(router, http.MethodPost, publishURL, map[string]any{})
	if status != http.StatusConflict || body["error"] != "team_members_unlisted" {
		t.Fatalf("publish with unlisted members = %d %v, want 409 team_members_unlisted", status, body)
	}
	if members, _ := body["members"].([]any); len(members) != 2 {
		t.Fatalf("unlisted members = %v, want both", body["members"])
	}

	// One listed: still refused, naming only the other.
	h.publish(writer, "v1", "")
	status, body = h.do(router, http.MethodPost, publishURL, map[string]any{})
	members, _ := body["members"].([]any)
	if status != http.StatusConflict || len(members) != 1 || members[0].(map[string]any)["position"].(float64) != 2 {
		t.Fatalf("publish with one unlisted member = %d %v, want 409 naming position 2", status, body)
	}

	h.publish(reviewer, "v1", "")
	status, body = h.do(router, http.MethodPost, publishURL, map[string]any{"release_notes": "First crew.", "category": "Development"})
	if status != http.StatusCreated || body["version_id"] != "1" || body["version_name"] != "v1" {
		t.Fatalf("publish = %d %v, want 201 version 1", status, body)
	}
	teamListing := body["listing_id"].(string)
	teamVersion := body["version_id"].(string)

	// kind=team finds the team alone; the kind facet counts it.
	rows := rowsOf(h.search("?kind=team"))
	if len(rows) != 1 || rows[0]["id"] != teamListing || rows[0]["kind"] != "team" {
		t.Fatalf("kind=team = %v, want the team listing", rows)
	}
	if rows[0]["required_types"].([]any)[0] != "github" {
		t.Errorf("team required types = %v, want the members' union", rows[0]["required_types"])
	}
	if n := len(rowsOf(h.search("?kind=agent"))); n != 2 {
		t.Errorf("kind=agent = %d rows, want the two members", n)
	}

	// The page resolves the members as their listings.
	detail := h.detail(teamListing)
	team, _ := detail["team"].(map[string]any)
	if team == nil || team["floor_policy"] != "lead" || team["lead_position"].(float64) != 2 {
		t.Fatalf("team detail = %v", detail["team"])
	}
	detailMembers := team["members"].([]any)
	first := detailMembers[0].(map[string]any)
	if len(detailMembers) != 2 || first["name"] != "Team Writer" || first["hireable"] != true || first["hired_application_id"] != nil {
		t.Fatalf("team members = %v", detailMembers)
	}
	writerListing := first["listing_id"].(string)
	writerVersion := first["version_id"].(string)
	key := first["requirements"].(map[string]any)["items"].([]any)[0].(map[string]any)["key"].(string)
	toolkit := h.projectToolkit(2, "github")

	// The writer's connection is required by the team hire too.
	hireURL := "/elitea_core/marketplace_team_hire/prompt_lib/2"
	status, body = h.do(router, http.MethodPost, hireURL, map[string]any{
		"listing_id": teamListing, "version_id": teamVersion, "mode": "install", "name": "Crew A"})
	if status != http.StatusConflict || body["error"] != "binding_missing" || body["position"].(float64) != 1 {
		t.Fatalf("team hire without the writer's binding = %d %v, want 409 binding_missing at position 1", status, body)
	}

	// Install, with the writer already installed once: the team reuses it.
	status, body = h.do(h.router, http.MethodPost, "/elitea_core/marketplace_hire/prompt_lib/2",
		hireBody(writerListing, writerVersion, "install", map[string]any{"requirement_key": key, "toolkit_id": strconv.Itoa(toolkit)}))
	if status != http.StatusCreated {
		t.Fatalf("hire the writer = %d %v", status, body)
	}
	installedWriter := body["application_id"].(string)
	status, body = h.do(router, http.MethodPost, hireURL, map[string]any{
		"listing_id": teamListing, "version_id": teamVersion, "mode": "install", "name": "Crew A",
		"members": []any{map[string]any{"position": 1, "bindings": []any{map[string]any{"requirement_key": key, "toolkit_id": strconv.Itoa(toolkit)}}}}})
	if status != http.StatusCreated {
		t.Fatalf("team install = %d %v", status, body)
	}
	hired := body["members"].([]any)
	if hired[0].(map[string]any)["reused"] != true || hired[0].(map[string]any)["application_id"] != installedWriter {
		t.Errorf("team install member 1 = %v, want the installed writer reused", hired[0])
	}
	if hired[1].(map[string]any)["reused"] != false {
		t.Errorf("team install member 2 = %v, want a new hire", hired[1])
	}
	installTeam := h.assertHiredTeam(body, "Crew A", "install", teamListing, teamVersion)

	// Fork makes new editable agents for every member, never reusing.
	status, body = h.do(router, http.MethodPost, hireURL, map[string]any{
		"listing_id": teamListing, "version_id": teamVersion, "mode": "fork", "name": "Crew B",
		"members": []any{map[string]any{"position": 1, "bindings": []any{map[string]any{"requirement_key": key, "disabled": true}}}}})
	if status != http.StatusCreated {
		t.Fatalf("team fork = %d %v", status, body)
	}
	forked := body["members"].([]any)
	for _, raw := range forked {
		member := raw.(map[string]any)
		if member["reused"] != false || member["application_id"] == installedWriter {
			t.Errorf("fork member = %v, want a new agent", member)
		}
	}
	if unbound := forked[0].(map[string]any)["unbound"].([]any); len(unbound) != 1 || unbound[0] != key {
		t.Errorf("fork member 1 unbound = %v, want the disabled requirement", unbound)
	}
	forkTeam := h.assertHiredTeam(body, "Crew B", "fork", teamListing, teamVersion)
	var origin string
	if err := h.pool.QueryRow(ctx, `SELECT meta->>'origin_kind' FROM p_2.application_versions WHERE application_id = $1`,
		forked[1].(map[string]any)["application_id"]).Scan(&origin); err != nil || origin != "forked" {
		t.Errorf("forked member origin = %q (%v), want forked", origin, err)
	}
	if installTeam == forkTeam {
		t.Errorf("both hires answered team %d", installTeam)
	}

	// The same name again is refused, and nothing of the refused hire is kept.
	var agentsBefore int
	_ = h.pool.QueryRow(ctx, `SELECT count(*) FROM p_2.applications`).Scan(&agentsBefore)
	status, body = h.do(router, http.MethodPost, hireURL, map[string]any{
		"listing_id": teamListing, "version_id": teamVersion, "mode": "fork", "name": "Crew B",
		"members": []any{map[string]any{"position": 1, "bindings": []any{map[string]any{"requirement_key": key, "disabled": true}}}}})
	if status != http.StatusConflict || body["error"] != "team_name_taken" {
		t.Fatalf("team hire with a taken name = %d %v, want 409 team_name_taken", status, body)
	}
	var agentsAfter int
	_ = h.pool.QueryRow(ctx, `SELECT count(*) FROM p_2.applications`).Scan(&agentsAfter)
	if agentsAfter != agentsBefore {
		t.Errorf("a refused team hire left %d agents behind", agentsAfter-agentsBefore)
	}

	// A team listing is not hired through the agent hire.
	status, body = h.do(h.router, http.MethodPost, "/elitea_core/marketplace_hire/prompt_lib/2", hireBody(teamListing, teamVersion, "install"))
	if status != http.StatusBadRequest {
		t.Errorf("agent hire of a team listing = %d %v, want 400", status, body)
	}

	// A republish adds version 2.
	status, body = h.do(router, http.MethodPost, publishURL, map[string]any{"version_name": "spring"})
	if status != http.StatusCreated || body["version_id"] != "2" || body["version_name"] != "spring" {
		t.Errorf("republish = %d %v, want version 2 named spring", status, body)
	}

	// The daily reconcile reads a hired team from elitea_chat.agent_teams, not
	// from a tenant table: a live team stays, a deleted one is dismissed.
	if _, err := h.pool.Exec(ctx, `UPDATE elitea_marketplace.installs SET created_at = clock_timestamp() - interval '2 days'`); err != nil {
		t.Fatal(err)
	}
	if err := repos.NewAgentTeams(h.pool).DeleteTeam(ctx, 2, forkTeam); err != nil {
		t.Fatalf("delete the forked team: %v", err)
	}
	if _, err := marketplace.ReconcileDismissals(ctx, h.pool); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	for teamID, wantDismissed := range map[int64]bool{installTeam: false, forkTeam: true} {
		var dismissed bool
		if err := h.pool.QueryRow(ctx, `SELECT dismissed_at IS NOT NULL FROM elitea_marketplace.installs
WHERE project_id = 2 AND entity_kind = 'team' AND entity_id = $1`, teamID).Scan(&dismissed); err != nil || dismissed != wantDismissed {
			t.Errorf("team %d dismissed = %v (%v), want %v", teamID, dismissed, err, wantDismissed)
		}
	}
}

// assertHiredTeam checks the saved team a hire created and its install row, and
// answers the team id.
func (h *marketplaceHarness) assertHiredTeam(body map[string]any, name, mode, listingID, versionID string) int64 {
	h.t.Helper()
	ctx := context.Background()
	teamID, err := strconv.ParseInt(body["team_id"].(string), 10, 64)
	if err != nil || body["name"] != name || body["mode"] != mode {
		h.t.Fatalf("team hire answer = %v", body)
	}
	team, err := repos.NewAgentTeams(h.pool).GetTeam(ctx, 2, teamID)
	if err != nil {
		h.t.Fatalf("read the hired team: %v", err)
	}
	if team.Name != name || team.Policy != turntaking.PolicyLead || team.Lead != 2 || len(team.Members) != 2 {
		h.t.Fatalf("hired team = %+v", team)
	}
	hired := body["members"].([]any)
	for index, member := range team.Members {
		answer := hired[index].(map[string]any)
		if strconv.FormatInt(member.ApplicationID, 10) != answer["application_id"] ||
			strconv.FormatInt(member.VersionID, 10) != answer["version_id"] || member.ProjectID != 2 {
			h.t.Errorf("team member %d = %+v, want the hired agent %v", index+1, member, answer)
		}
		var exists bool
		if err := h.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM p_2.application_versions WHERE id = $1 AND application_id = $2)`,
			member.VersionID, member.ApplicationID).Scan(&exists); err != nil || !exists {
			h.t.Errorf("team member %d pins version %d, which is not its agent's (%v)", index+1, member.VersionID, err)
		}
	}
	var gotListing, gotVersion int64
	var gotMode string
	if err := h.pool.QueryRow(ctx, `
SELECT listing_id, version_id, mode FROM elitea_marketplace.installs
WHERE project_id = 2 AND entity_kind = 'team' AND entity_id = $1`, teamID).Scan(&gotListing, &gotVersion, &gotMode); err != nil {
		h.t.Fatalf("read the team install: %v", err)
	}
	if strconv.FormatInt(gotListing, 10) != listingID || strconv.FormatInt(gotVersion, 10) != versionID || gotMode != mode {
		h.t.Errorf("team install = %d/%d %s, want %s/%s %s", gotListing, gotVersion, gotMode, listingID, versionID, mode)
	}
	return teamID
}
