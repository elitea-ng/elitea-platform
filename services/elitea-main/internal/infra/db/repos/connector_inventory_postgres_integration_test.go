package repos

import (
	"slices"
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
	if len(outcome.Created) != 2 || outcome.Skipped != 0 {
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
	if len(again.Created) != 0 || again.Skipped != 2 {
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
	if len(third.Created) != 0 || third.Skipped != 2 {
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
