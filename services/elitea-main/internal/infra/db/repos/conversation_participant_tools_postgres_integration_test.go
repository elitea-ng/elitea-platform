package repos

// The conversation read resolves an agent participant's TOOL LIST.
//
// A participant's `meta` is written once, when the agent is attached, and it
// held the display name and nothing else. The legacy conversation read served
// the agent's resolved tools with the participant, and a client that draws the
// participant rail had nowhere else to read them from (issue 856). A snapshot
// taken at attach time could not answer it either: an author changes an agent's
// toolkits afterwards, and the conversation must show what the agent HAS.
//
// The cases below cover the four answers the read can give: the toolkits of a
// healthy participant, an empty list where there is nothing to resolve, a
// participant whose settings lost their version — which must still come back,
// because a conversation that will not open when one attached agent is broken
// is worse than a rail with no tool list — and a tenant with no toolkit tables
// at all, where the enrichment must stand aside rather than take the
// conversation down.
//
// THE TOOLKIT TABLES ARE SEEDED HERE. `elitea_tools` and `entity_tool_mapping`
// are pylon-owned in the tenant history (tenant/0124 alters `elitea_tools` and
// only where it already exists), so the migrated template this package builds
// has neither. The seed below declares the two columns each query names, in the
// shape internal/infra/db/migrations/001_initial.sql gives them.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"testing"
)

// seedToolkitTables creates the two pylon-owned tables the enrichment reads.
func seedToolkitTables(t *testing.T, repo *ConversationsRepo) {
	t.Helper()
	if _, err := repo.pool.Exec(context.Background(), `
CREATE TABLE IF NOT EXISTS p_1.elitea_tools (
    id SERIAL PRIMARY KEY,
    name VARCHAR(128) NOT NULL,
    type VARCHAR(64) NOT NULL,
    description TEXT,
    owner_id INTEGER NOT NULL,
    author_id INTEGER NOT NULL,
    settings JSONB DEFAULT '{}'::jsonb
);
CREATE TABLE IF NOT EXISTS p_1.entity_tool_mapping (
    id SERIAL PRIMARY KEY,
    entity_version_id INTEGER NOT NULL,
    entity_id INTEGER NOT NULL DEFAULT 0,
    entity_type VARCHAR(50) NOT NULL,
    tool_id INTEGER NOT NULL REFERENCES p_1.elitea_tools(id) ON DELETE CASCADE,
    selected_tools JSONB DEFAULT '[]'::jsonb
)`); err != nil {
		t.Fatalf("seed the toolkit tables: %v", err)
	}
}

// attachToolkits attaches the named toolkits to one agent version.
func attachToolkits(t *testing.T, repo *ConversationsRepo, versionID int, toolkits map[string]string) {
	t.Helper()
	ctx := context.Background()
	for toolkitName, toolkitType := range toolkits {
		var toolID int
		if err := repo.pool.QueryRow(ctx, `
INSERT INTO p_1.elitea_tools (name, type, description, owner_id, author_id)
VALUES ($1, $2, '', 1, 1) RETURNING id`, toolkitName, toolkitType).Scan(&toolID); err != nil {
			t.Fatalf("seed toolkit %s: %v", toolkitName, err)
		}
		if _, err := repo.pool.Exec(ctx, `
INSERT INTO p_1.entity_tool_mapping (entity_version_id, entity_id, entity_type, tool_id, selected_tools)
VALUES ($1, 1, 'agent', $2, '["read"]'::jsonb)`, versionID, toolID); err != nil {
			t.Fatalf("attach toolkit %s: %v", toolkitName, err)
		}
	}
}

func attachAgentParticipant(t *testing.T, repo *ConversationsRepo, conversationID, agentName string, entityID int, settings map[string]any) {
	t.Helper()
	body := map[string]any{
		"entity_name": "application",
		"entity_meta": map[string]any{"id": entityID, "project_id": 1, "name": agentName},
	}
	if settings != nil {
		body["entity_settings"] = settings
	}
	if err := repo.AddParticipant(context.Background(), "1", conversationID, body); err != nil {
		t.Fatalf("attach %s: %v", agentName, err)
	}
}

func participantsByName(t *testing.T, repo *ConversationsRepo, conversationID string, want int) map[string]map[string]any {
	t.Helper()
	items, err := repo.ListParticipants(context.Background(), "1", conversationID)
	if err != nil {
		t.Fatalf("list participants: %v", err)
	}
	if len(items) != want {
		t.Fatalf("participants = %d, want %d: %v", len(items), want, items)
	}
	byName := map[string]map[string]any{}
	for _, item := range items {
		name, _ := item.Meta["name"].(string)
		byName[name] = item.Meta
	}
	return byName
}

func participantTools(t *testing.T, meta map[string]any) []map[string]any {
	t.Helper()
	raw, present := meta["tools"]
	if !present {
		t.Fatalf("the participant carries no resolved tool list: %v", meta)
	}
	tools, ok := raw.([]map[string]any)
	if !ok {
		t.Fatalf("meta.tools is %T, want a list of tools", raw)
	}
	return tools
}

func TestAnAgentParticipantCarriesItsResolvedToolList(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)
	conversationID := seedConversation(t, repo)
	seedToolkitTables(t, repo)

	const versionID = 4100
	attachToolkits(t, repo, versionID, map[string]string{
		"jira board":  "jira",
		"the sources": "github",
	})
	attachAgentParticipant(t, repo, conversationID, "tooled agent", 42,
		map[string]any{"version_id": versionID})

	meta := participantsByName(t, repo, conversationID, 1)["tooled agent"]
	// The display name still comes back. The tool list is an ENRICHMENT: a read
	// that answered tools instead of the name would break the rail it feeds.
	if meta["name"] != "tooled agent" {
		t.Fatalf("meta.name = %v, want the agent name", meta["name"])
	}

	tools := participantTools(t, meta)
	if len(tools) != 2 {
		t.Fatalf("meta.tools = %v, want both toolkits", tools)
	}
	byName := map[string]map[string]any{}
	for _, tool := range tools {
		name, _ := tool["name"].(string)
		byName[name] = tool
	}
	jira, found := byName["jira board"]
	if !found {
		t.Fatalf("meta.tools = %v, want the jira toolkit", tools)
	}
	if jira["type"] != "jira" {
		t.Errorf("the jira toolkit's type = %v, want jira", jira["type"])
	}
	if jira["toolkit_name"] != "jira board" {
		t.Errorf("toolkit_name = %v, want the toolkit's own name", jira["toolkit_name"])
	}
	if _, found := byName["the sources"]; !found {
		t.Errorf("meta.tools = %v, want the github toolkit too", tools)
	}
}

// TestAnAgentParticipantIsEnrichedFromItsOwnVersion: two agents in one
// conversation get their OWN toolkits. Without this, a read that resolved one
// list and gave it to everybody would pass the case above.
func TestAnAgentParticipantIsEnrichedFromItsOwnVersion(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)
	conversationID := seedConversation(t, repo)
	seedToolkitTables(t, repo)

	attachToolkits(t, repo, 4100, map[string]string{"jira board": "jira"})
	attachToolkits(t, repo, 4200, map[string]string{"the sources": "github"})
	attachAgentParticipant(t, repo, conversationID, "first agent", 42, map[string]any{"version_id": 4100})
	attachAgentParticipant(t, repo, conversationID, "second agent", 43, map[string]any{"version_id": "4200"})

	byName := participantsByName(t, repo, conversationID, 2)
	first := participantTools(t, byName["first agent"])
	if len(first) != 1 || first[0]["name"] != "jira board" {
		t.Errorf("the first agent's tools = %v, want its own toolkit", first)
	}
	// The second agent's version arrives as a STRING, which is how a client
	// that read it out of a URL writes it.
	second := participantTools(t, byName["second agent"])
	if len(second) != 1 || second[0]["name"] != "the sources" {
		t.Errorf("the second agent's tools = %v, want its own toolkit", second)
	}
}

// TestAParticipantWithoutAVersionStillLoads is the resilience case: the
// enrichment may never take the conversation down with it.
func TestAParticipantWithoutAVersionStillLoads(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)
	conversationID := seedConversation(t, repo)
	seedToolkitTables(t, repo)

	attachToolkits(t, repo, 4100, map[string]string{"a toolkit": "github"})
	attachAgentParticipant(t, repo, conversationID, "healthy agent", 44,
		map[string]any{"version_id": 4100})
	// Settings with no version at all…
	attachAgentParticipant(t, repo, conversationID, "settings-less agent", 45,
		map[string]any{"chat_settings": map[string]any{"temperature": 0.1}})
	// …and settings naming a version that has nothing attached, which is also
	// what a version that has been deleted looks like from here.
	attachAgentParticipant(t, repo, conversationID, "empty agent", 46,
		map[string]any{"version_id": 99_000})

	byName := participantsByName(t, repo, conversationID, 3)
	if tools := participantTools(t, byName["healthy agent"]); len(tools) != 1 {
		t.Errorf("the healthy agent's tools = %v, want one toolkit", tools)
	}
	// A participant with no version is not enriched at all — there is nothing
	// to resolve — and it still comes back, with its display name intact.
	if _, resolved := byName["settings-less agent"]["tools"]; resolved {
		t.Errorf("a participant with no version was given a tool list: %v", byName["settings-less agent"])
	}
	if byName["settings-less agent"]["name"] != "settings-less agent" {
		t.Errorf("the degraded participant lost its name: %v", byName["settings-less agent"])
	}
	// A version with nothing attached answers an EMPTY list rather than an
	// error or a missing key.
	if tools := participantTools(t, byName["empty agent"]); len(tools) != 0 {
		t.Errorf("a version with no toolkits resolved %v", tools)
	}
}

// TestParticipantsLoadWhereTheToolkitTablesDoNotExist: `elitea_tools` and
// `entity_tool_mapping` belong to pylon in the tenant history, so a deployment
// can have chat tables and no toolkit tables. The read must answer the
// participants it has.
func TestParticipantsLoadWhereTheToolkitTablesDoNotExist(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)
	conversationID := seedConversation(t, repo)

	attachAgentParticipant(t, repo, conversationID, "unresolvable agent", 47,
		map[string]any{"version_id": 4100})

	meta := participantsByName(t, repo, conversationID, 1)["unresolvable agent"]
	if meta["name"] != "unresolvable agent" {
		t.Fatalf("meta = %v, want the display name", meta)
	}
	if _, resolved := meta["tools"]; resolved {
		t.Errorf("a tenant with no toolkit tables resolved a tool list: %v", meta)
	}
}
