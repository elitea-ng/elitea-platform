package applications_test

// The two things this service does with a SUB-AGENT reference, both of which
// used to be decided by a table that exists in no schema it builds.
//
// A sub-agent reference is one `elitea_tools` row of type `application`, whose
// `settings` carry the child pair, plus one `entity_tool_mapping` row binding
// it to the parent version. That is what
// internal/api/v2/eliteacore/application_relation.go writes, what the chat
// resolver joins, and what the freeze parses. The pylon-era `application_tools`
// table holds the same reference in a pylon-MIGRATED tenant and nowhere else:
// no migration in this repository creates it.
//
// Both defects below are the same shape. A statement named the pylon table
// only, so on every schema this service creates it either failed with 42P01
// into a discarded error or matched no row — and the feature it implemented
// looked present in the source and was absent in every deployment.
//
//  1. The expanded version read stamped `project_id` on the pylon branch alone,
//     so the SDK received sub-agent tools with no project. The child lives in a
//     tenant schema and the application id alone does not say which one, so a
//     runtime that had to resolve the child could not.
//  2. Deleting an agent cleaned up references to it in the pylon table alone, so
//     every parent went on offering a tool whose agent had been deleted.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// attachSubAgent writes one sub-agent reference in the canonical pair of
// tables — the shape UpdateApplicationRelation writes — and answers the
// `elitea_tools.id` it minted.
func attachSubAgent(
	t *testing.T,
	pool *pgxpool.Pool,
	parentApplicationID, parentVersionID, childApplicationID, childVersionID int64,
	name string,
) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	settings := `{"application_id":` + strconv.FormatInt(childApplicationID, 10) +
		`,"application_version_id":` + strconv.FormatInt(childVersionID, 10) + `}`
	var toolID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO p_1.elitea_tools (name, type, description, settings, meta, owner_id, author_id)
		VALUES ($1, 'application', '', $2::jsonb, '{}'::jsonb, 1, 7)
		RETURNING id`, name, settings).Scan(&toolID); err != nil {
		t.Fatalf("seed the sub-agent tool row: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO p_1.entity_tool_mapping (entity_version_id, entity_type, tool_id, entity_id)
		VALUES ($1, 'agent', $2, $3)`, parentVersionID, toolID, parentApplicationID); err != nil {
		t.Fatalf("seed the sub-agent mapping: %v", err)
	}
	return toolID
}

// seedChildAgent creates a second agent in p_1 to stand as the sub-agent.
func seedChildAgent(t *testing.T, pool *pgxpool.Pool, name string) (int64, int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	var applicationID, versionID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO p_1.applications (name, description, owner_id)
		VALUES ($1, 'the sub-agent', 1) RETURNING id`, name).Scan(&applicationID); err != nil {
		t.Fatalf("seed the child application: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO p_1.application_versions
			(application_id, name, status, author_id, agent_type, instructions)
		VALUES ($1, 'base', 'draft', 7, 'openai', 'Answer the caller.')
		RETURNING id`, applicationID).Scan(&versionID); err != nil {
		t.Fatalf("seed the child version: %v", err)
	}
	return applicationID, versionID
}

// ── 1. the expanded read names the project of every tool ─────────────────────

// The SDK reads `project_id` off each tool. Pylon types it on the tool model
// itself, so every entry carries it; this handler served it on the pylon branch
// alone, which no schema it builds has.
func TestVersionPatchStampsTheProjectOnEverySubAgentTool(t *testing.T) {
	fixture := newExpandedFixture(t, "openai")
	childApplicationID, childVersionID := seedChildAgent(t, fixture.pool, "the-child")
	attachSubAgent(t, fixture.pool,
		fixture.applicationID, fixture.versionID, childApplicationID, childVersionID, "the-child")

	recorder := fixture.patchVersion(t, expandedHeaderValue)
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	body := decodeBody(t, recorder)

	tools, ok := body["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("expected the one attached sub-agent, got %v (%s)", body["tools"], recorder.Body.String())
	}
	tool, _ := tools[0].(map[string]any)
	if tool["type"] != "application" {
		t.Fatalf("expected an application-type tool, got %v", tool["type"])
	}
	// The project the child lives in. Compared as a number, because that is what
	// the schema declares and what the SDK indexes.
	projectID, ok := tool["project_id"].(float64)
	if !ok {
		t.Fatalf("the sub-agent tool carries no project_id: %v", tool)
	}
	if int(projectID) != 1 {
		t.Errorf("expected project_id 1, got %v", tool["project_id"])
	}
	// …and it still names the child pair, which is the other half of what the
	// runtime needs to reach the agent.
	settings, _ := tool["settings"].(map[string]any)
	if settings["application_id"] != float64(childApplicationID) {
		t.Errorf("expected settings.application_id %d, got %v", childApplicationID, settings["application_id"])
	}
	if settings["application_version_id"] != float64(childVersionID) {
		t.Errorf("expected settings.application_version_id %d, got %v",
			childVersionID, settings["application_version_id"])
	}
}

// ── 2. deleting a sub-agent detaches it from every parent ────────────────────

// The acceptance test for the dead cleanup: delete the child through the route
// and read the parent's mapping rows back out of the database. The delete's own
// 204 is exactly what the defect produced, so it proves nothing on its own.
func TestDeletingAnAgentDetachesItFromEveryParentThatUsedIt(t *testing.T) {
	fixture := newExpandedFixture(t, "openai")
	childApplicationID, childVersionID := seedChildAgent(t, fixture.pool, "the-child")
	toolID := attachSubAgent(t, fixture.pool,
		fixture.applicationID, fixture.versionID, childApplicationID, childVersionID, "the-child")

	// A second toolkit on the same parent, which this delete must NOT touch: a
	// cleanup that removed every mapping of the version would pass an assertion
	// that only counted the sub-agent away.
	var otherToolID int64
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := fixture.pool.QueryRow(ctx, `
		INSERT INTO p_1.elitea_tools (name, type, description, settings, meta, owner_id, author_id)
		VALUES ('a github toolkit', 'github', '', '{}'::jsonb, '{}'::jsonb, 1, 7)
		RETURNING id`).Scan(&otherToolID); err != nil {
		t.Fatalf("seed the unrelated toolkit: %v", err)
	}
	if _, err := fixture.pool.Exec(ctx, `
		INSERT INTO p_1.entity_tool_mapping (entity_version_id, entity_type, tool_id, entity_id)
		VALUES ($1, 'agent', $2, $3)`,
		fixture.versionID, otherToolID, fixture.applicationID); err != nil {
		t.Fatalf("seed the unrelated attachment: %v", err)
	}

	request := httptest.NewRequest(http.MethodDelete,
		"/application/prompt_lib/1/"+strconv.FormatInt(childApplicationID, 10), nil)
	recorder := httptest.NewRecorder()
	fixture.router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("delete the child agent: got %d: %s", recorder.Code, recorder.Body.String())
	}

	var mappings int
	if err := fixture.pool.QueryRow(ctx, `
		SELECT count(*) FROM p_1.entity_tool_mapping
		WHERE entity_version_id = $1 AND tool_id = $2`, fixture.versionID, toolID).Scan(&mappings); err != nil {
		t.Fatalf("read the parent's mappings: %v", err)
	}
	if mappings != 0 {
		t.Errorf("the parent still references the deleted sub-agent: %d mapping(s)", mappings)
	}

	// The orphaned tool row goes with it. Left behind, it would keep answering
	// the union read the resolver makes.
	var toolRows int
	if err := fixture.pool.QueryRow(ctx,
		`SELECT count(*) FROM p_1.elitea_tools WHERE id = $1`, toolID).Scan(&toolRows); err != nil {
		t.Fatalf("read the sub-agent tool row: %v", err)
	}
	if toolRows != 0 {
		t.Errorf("the orphaned sub-agent tool row survived the delete")
	}

	// The unrelated toolkit is untouched.
	var otherMappings int
	if err := fixture.pool.QueryRow(ctx, `
		SELECT count(*) FROM p_1.entity_tool_mapping
		WHERE entity_version_id = $1 AND tool_id = $2`,
		fixture.versionID, otherToolID).Scan(&otherMappings); err != nil {
		t.Fatalf("read the unrelated attachment: %v", err)
	}
	if otherMappings != 1 {
		t.Errorf("the delete removed an attachment it was not asked to touch")
	}
}
