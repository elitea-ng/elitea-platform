package repos

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/connectorapproval"
)

// The on-ramp to approved_only (shared 0207, #1501): the connectors in use,
// the count no approved request covers, and the idempotent, audited
// inventory requests a different approver must decide. CI-only: skipped
// without the integration database.

const inventoryMCPServer = "https://mcp.example.com/mcp"

func seedConnectorsInUse(t *testing.T, store *ConnectorRequests) {
	t.Helper()
	seedCurrentAgentContinuationSchema(t, store.pool)
	if _, err := store.pool.Exec(t.Context(), `
DELETE FROM p_1.elitea_tools;
INSERT INTO p_1.elitea_tools (id, type, name, description, settings, author_id, meta, created_at) VALUES
    (71, 'jira', 'jira', '', '{"selected_tools":["search_issues"],"base_url":"https://jira.example.com"}'::jsonb, 11, '{}'::jsonb, now() AT TIME ZONE 'UTC'),
    (72, 'mcp', 'docs', '', '{"url":"https://MCP.example.com:443/mcp/","selected_tools":["search"]}'::jsonb, 11, '{}'::jsonb, now() AT TIME ZONE 'UTC'),
    (73, 'memory', 'mem', '', '{}'::jsonb, 11, '{}'::jsonb, now() AT TIME ZONE 'UTC');
WITH server AS (
    INSERT INTO elitea_mcp.registered_servers (project_id, name, server_url) VALUES (1, 'mcp', 'https://mcp.example.com/mcp/')
    RETURNING id)
INSERT INTO elitea_mcp.registered_tools (server_id, name, description, input_schema, ordinal)
SELECT id, 'search', 'Search the docs', '{"type":"object","properties":{"q":{"type":"string"}}}'::jsonb, 0 FROM server
UNION ALL
SELECT id, 'delete_page', 'Delete a page', '{"type":"object"}'::jsonb, 1 FROM server`); err != nil {
		t.Fatal(err)
	}
}

func TestConnectorInventory_CreatesAuditedRequestsOnceAndKeepsFourEyes(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	_, store := newConnectorTestStore(t, pool, false)
	seedConnectorsInUse(t, store)

	inventory, err := store.Inventory(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(inventory.InUse) != 2 || len(inventory.WithoutApproval) != 2 {
		t.Fatalf("inventory %+v, want jira and the custom MCP server, both unapproved", inventory)
	}

	const admin, otherApprover int64 = 12, 13
	outcome, err := store.CreateInventoryRequests(t.Context(), admin)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(outcome.Created) != 2 || len(outcome.Skipped) != 0 || len(outcome.Failed) != 0 {
		t.Fatalf("outcome %+v", outcome)
	}
	byKind := map[connectorapproval.TargetKind]connectorapproval.Request{}
	for _, request := range outcome.Created {
		if request.State != connectorapproval.StateSubmitted || request.Origin != connectorapproval.OriginInventory ||
			request.RequestedBy != 0 || request.CreatedBy != admin || !slices.Equal(request.ProjectIDs, []int64{1}) {
			t.Fatalf("inventory request %+v", request)
		}
		byKind[request.TargetKind] = request
	}
	jira := byKind[connectorapproval.TargetToolkitType]
	if jira.Target != "jira" || len(jira.Tools) != 1 || jira.Tools[0].Name != "search_issues" ||
		!slices.Contains(jira.EgressHosts, "jira.example.com") {
		t.Fatalf("jira request %+v", jira)
	}
	mcp := byKind[connectorapproval.TargetCustomMCP]
	if mcp.Target != inventoryMCPServer || len(mcp.Tools) != 1 || mcp.Tools[0].Name != "search" ||
		mcp.Tools[0].Digest == "" || !slices.Contains(mcp.EgressHosts, "mcp.example.com") {
		t.Fatalf("custom MCP request %+v (only the selected, discovered tool, pinned)", mcp)
	}
	if got := connectorAuditActions(t, pool); len(got) != 4 {
		t.Fatalf("audit actions %v, want created and submitted per request", got)
	}

	// Idempotent: the open requests are skipped.
	again, err := store.CreateInventoryRequests(t.Context(), admin)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Created) != 0 || len(again.Skipped) != 2 {
		t.Fatalf("second run %+v", again)
	}

	// Four eyes: the administrator who created a request cannot decide it,
	// in code and in the CHECK.
	_, err = store.Approve(t.Context(), jira.ID, admin, "")
	requireConnectorRefusal(t, err, connectorapproval.SelfApprovalCode)
	_, err = pool.Exec(t.Context(), `
UPDATE centry.connector_requests SET state = 'approved', decided_by = created_by, decided_at = now() WHERE id = $1`, jira.ID)
	requirePgCode(t, err, "23514", "an inventory request decided by its creator")

	for _, request := range []connectorapproval.Request{jira, mcp} {
		if _, err := store.Approve(t.Context(), request.ID, otherApprover, ""); err != nil {
			t.Fatalf("approve %s: %v", request.Target, err)
		}
	}
	inventory, err = store.Inventory(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(inventory.InUse) != 2 || len(inventory.WithoutApproval) != 0 {
		t.Fatalf("after approval %+v", inventory)
	}
	// An approved connector is skipped too.
	third, err := store.CreateInventoryRequests(t.Context(), admin)
	if err != nil {
		t.Fatal(err)
	}
	if len(third.Created) != 0 || len(third.Skipped) != 2 {
		t.Fatalf("third run %+v", third)
	}
}

// TestConnectorInventory_UserRequestNeedsARequester: a member's request
// without requested_by, or an inventory request without created_by, is
// refused by the database.
func TestConnectorInventory_UserRequestNeedsARequester(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	newConnectorTestStore(t, pool, false)
	for _, origin := range []string{"user", "inventory"} {
		_, err := pool.Exec(t.Context(), `
INSERT INTO centry.connector_requests (project_id, target_kind, target, toolkit_type, project_ids, origin)
VALUES (1, 'toolkit_type', 'jira', 'jira', '{1}', $1)`, origin)
		requirePgCode(t, err, "23514", origin+" request without its actor")
	}
}

// seedJiraInProjects configures jira in each project (p_1 from the
// continuation seed, the others created here).
func seedJiraInProjects(t *testing.T, store *ConnectorRequests, projects ...int64) {
	t.Helper()
	seedCurrentAgentContinuationSchema(t, store.pool)
	if _, err := store.pool.Exec(t.Context(), `DELETE FROM p_1.elitea_tools`); err != nil {
		t.Fatal(err)
	}
	for _, id := range projects {
		if id != 1 {
			if _, err := store.pool.Exec(t.Context(), fmt.Sprintf(`
INSERT INTO centry.project (id, create_success, suspended) VALUES (%[1]d, TRUE, FALSE) ON CONFLICT DO NOTHING;
CREATE SCHEMA IF NOT EXISTS p_%[1]d;
CREATE TABLE IF NOT EXISTS p_%[1]d.elitea_tools (LIKE p_1.elitea_tools INCLUDING DEFAULTS)`, id)); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := store.pool.Exec(t.Context(), fmt.Sprintf(`
INSERT INTO p_%d.elitea_tools (type, name, description, settings, author_id, meta, created_at)
VALUES ('jira', 'jira', '', '{"selected_tools":["search_issues"],"base_url":"https://jira.example.com"}'::jsonb, 11, '{}'::jsonb, now() AT TIME ZONE 'UTC')`, id)); err != nil {
			t.Fatal(err)
		}
	}
}

// #1537 review item 4: jira in projects 1, 2 and 3 with an approval for 1
// only is not covered; the on-ramp requests it for 2 and 3, and the
// inventory and the on-ramp agree on the rule.
func TestConnectorInventory_RequestsTheUncoveredProjects(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	_, store := newConnectorTestStore(t, pool, false)
	seedJiraInProjects(t, store, 1, 2, 3)
	if _, err := store.Approve(t.Context(), approvedToolkitRequest(t, store, "jira").ID, connectorApprover, ""); err != nil {
		t.Fatal(err)
	}

	inventory, err := store.Inventory(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(inventory.WithoutApproval) != 1 || inventory.WithoutApproval[0].Target != "jira" {
		t.Fatalf("inventory %+v, want jira without approval for every project", inventory)
	}
	outcome, err := store.CreateInventoryRequests(t.Context(), 13)
	if err != nil {
		t.Fatal(err)
	}
	if len(outcome.Created) != 1 || !slices.Equal(outcome.Created[0].ProjectIDs, []int64{2, 3}) {
		t.Fatalf("outcome %+v, want one request for projects 2 and 3", outcome)
	}
	again, err := store.CreateInventoryRequests(t.Context(), 13)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Created) != 0 || len(again.Skipped) != 1 {
		t.Fatalf("second run %+v, want jira skipped as covered", again)
	}
}

// #1537 review item 5: a connector whose draft cannot be built does not
// stop the others; what can be sanitised is, and named in the
// justification.
func TestConnectorInventory_OneBadConnectorDoesNotAbortTheOthers(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	_, store := newConnectorTestStore(t, pool, false)
	seedConnectorsInUse(t, store)
	if _, err := pool.Exec(t.Context(), `
INSERT INTO p_1.elitea_tools (id, type, name, description, settings, author_id, meta, created_at) VALUES
    (74, 'github', 'github', '', '{"selected_tools":["get me", "list_repos"],"base_url":"https://api.github.com","proxy":"not a host!"}'::jsonb, 11, '{}'::jsonb, now() AT TIME ZONE 'UTC')`); err != nil {
		t.Fatal(err)
	}
	// A registered tool whose description is past the bound: dropped from
	// the custom MCP request (it could not be pinned as served).
	if _, err := pool.Exec(t.Context(), `
UPDATE elitea_mcp.registered_tools SET description = repeat('x', 5000) WHERE name = 'search'`); err != nil {
		t.Fatal(err)
	}
	outcome, err := store.CreateInventoryRequests(t.Context(), 13)
	if err != nil {
		t.Fatalf("the on-ramp failed as a whole: %v", err)
	}
	byTarget := map[string]connectorapproval.Request{}
	for _, request := range outcome.Created {
		byTarget[request.Target] = request
	}
	github, ok := byTarget["github"]
	if !ok || len(github.Tools) != 1 || github.Tools[0].Name != "list_repos" {
		t.Fatalf("github request %+v, want the valid tool kept", github)
	}
	mcp, ok := byTarget[inventoryMCPServer]
	if !ok || len(mcp.Tools) != 0 || !strings.Contains(mcp.Justification, `dropped the tool "search"`) {
		t.Fatalf("custom MCP request %+v, want the unpinnable tool dropped and named", mcp)
	}
	if _, ok := byTarget["jira"]; !ok {
		t.Fatalf("jira was not requested: %+v", outcome)
	}
}
