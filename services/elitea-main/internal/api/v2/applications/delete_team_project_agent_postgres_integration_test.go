package applications_test

// "Unable to delete agents" in a team project.
//
// An agent hired from the Marketplace, or published, brings its sub-agents
// along as EMBEDDED copies: one application and one version with
// `status = 'embedded'` per sub-agent, linked to the parent version as a
// sub-agent reference (marketplace_hire.go installNested, handler.go
// embedSubAgentsRecursive). The Workshop lists those copies like any other
// agent, as drafts.
//
// The delete route refused every application with an `embedded` version, with
// "Unpublish first", and nothing in the Workshop can unpublish a draft. The
// delete of the PARENT did not take the copies with it either. So after a
// hired agent was deleted, its sub-agent copies stayed in the list for good
// and every Delete on them failed.
//
// The cases run in a team project (p_1, "Default Project", not a personal
// `project_user_<id>` project) against the production migration chain, and
// the parent carries what a working agent carries: two versions, a folder,
// a saved team, a toolkit attachment.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// seedEmbeddedCopy writes one sub-agent copy the way a hire or a publish does:
// an application with one `embedded` version.
func seedEmbeddedCopy(t *testing.T, pool *pgxpool.Pool, name string) (int64, int64) {
	t.Helper()
	applicationID, versionID := seedChildAgent(t, pool, name)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `UPDATE p_1.application_versions SET status = 'embedded' WHERE id = $1`, versionID); err != nil {
		t.Fatalf("mark the copy embedded: %v", err)
	}
	return applicationID, versionID
}

func deleteAgent(t *testing.T, router http.Handler, applicationID int64) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodDelete,
		"/application/prompt_lib/1/"+strconv.FormatInt(applicationID, 10), nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func applicationExists(t *testing.T, pool *pgxpool.Pool, applicationID int64) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM p_1.applications WHERE id = $1`, applicationID).Scan(&n); err != nil {
		t.Fatalf("read the application: %v", err)
	}
	return n == 1
}

// seedWorkingAgentContext gives the fixture's agent a second version, a
// folder, a saved team and a toolkit: none of them may block its delete.
func seedWorkingAgentContext(t *testing.T, pool *pgxpool.Pool, applicationID, versionID int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var projectName string
	if err := pool.QueryRow(ctx, `SELECT name FROM centry.project WHERE id = 1`).Scan(&projectName); err != nil {
		t.Fatalf("read the project: %v", err)
	}
	if strings.HasPrefix(projectName, "project_user_") {
		t.Fatalf("project 1 is a personal project (%q); this test is about a team project", projectName)
	}
	statements := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO p_1.application_versions (application_id, name, status, author_id, agent_type, instructions, created_at)
		  VALUES ($1, 'second', 'draft', 7, 'openai', 'Second version.', now() AT TIME ZONE 'UTC')`, []any{applicationID}},
		{`INSERT INTO p_1.project_folders (id, entity_kind, name) VALUES (901, 'agent', 'Dogfooding')`, nil},
		{`INSERT INTO p_1.project_folder_items (entity_kind, entity_id, folder_id) VALUES ('agent', $1, 901)`, []any{applicationID}},
		{`INSERT INTO elitea_chat.agent_teams (id, project_id, name, created_by) VALUES (901, 1, 'Dev team', 7)`, nil},
		{`INSERT INTO elitea_chat.agent_team_members (team_id, position, application_id, agent_project_id, version_id, name)
		  VALUES (901, 1, $1, 1, $2, 'expanded-fixture')`, []any{applicationID, versionID}},
		{`INSERT INTO p_1.elitea_tools (id, name, type, description, settings, meta, owner_id, author_id, created_at)
		  VALUES (901, 'a github toolkit', 'github', '', '{}'::jsonb, '{}'::jsonb, 1, 7, now() AT TIME ZONE 'UTC')`, nil},
		{`INSERT INTO p_1.entity_tool_mapping (entity_version_id, entity_type, tool_id, entity_id, created_at)
		  VALUES ($1, 'agent', 901, $2, now() AT TIME ZONE 'UTC')`, []any{versionID, applicationID}},
	}
	for _, statement := range statements {
		if _, err := pool.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatalf("seed %q: %v", statement.sql, err)
		}
	}
}

// The agent itself, with no sub-agent: the control. It must delete.
func TestDeleteAgentInATeamProjectWithVersionsFolderAndTeam(t *testing.T) {
	fixture := newExpandedFixture(t, "openai")
	seedWorkingAgentContext(t, fixture.pool, fixture.applicationID, fixture.versionID)

	if recorder := deleteAgent(t, fixture.router, fixture.applicationID); recorder.Code != http.StatusNoContent {
		t.Fatalf("delete the agent: got %d: %s", recorder.Code, recorder.Body.String())
	}
	if applicationExists(t, fixture.pool, fixture.applicationID) {
		t.Fatal("the agent survived its delete")
	}
}

// A hired agent with one nested agent: deleting the parent takes the copy
// with it, so nothing undeletable is left in the list.
func TestDeletingAnAgentTakesItsEmbeddedSubAgentCopies(t *testing.T) {
	fixture := newExpandedFixture(t, "openai")
	seedWorkingAgentContext(t, fixture.pool, fixture.applicationID, fixture.versionID)
	copyID, copyVersionID := seedEmbeddedCopy(t, fixture.pool, "nested-copy")
	attachSubAgent(t, fixture.pool, fixture.applicationID, fixture.versionID, copyID, copyVersionID, "nested-copy")
	// A copy of the copy: a nested agent of the nested agent.
	grandchildID, grandchildVersionID := seedEmbeddedCopy(t, fixture.pool, "nested-copy-of-copy")
	attachSubAgent(t, fixture.pool, copyID, copyVersionID, grandchildID, grandchildVersionID, "nested-copy-of-copy")

	// While its parent holds it, the copy is refused, and the refusal says why.
	recorder := deleteAgent(t, fixture.router, copyID)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("delete a copy in use: got %d: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "sub-agent") {
		t.Errorf("the refusal does not say the agent is a sub-agent copy: %s", recorder.Body.String())
	}

	if recorder := deleteAgent(t, fixture.router, fixture.applicationID); recorder.Code != http.StatusNoContent {
		t.Fatalf("delete the parent: got %d: %s", recorder.Code, recorder.Body.String())
	}
	for label, id := range map[string]int64{"parent": fixture.applicationID, "copy": copyID, "copy of the copy": grandchildID} {
		if applicationExists(t, fixture.pool, id) {
			t.Errorf("the %s survived the parent's delete", label)
		}
	}
}

// A copy whose parent is already gone (left behind before this fix) is an
// ordinary agent to delete.
func TestAnOrphanedEmbeddedCopyCanBeDeleted(t *testing.T) {
	fixture := newExpandedFixture(t, "openai")
	orphanID, _ := seedEmbeddedCopy(t, fixture.pool, "orphaned-copy")

	if recorder := deleteAgent(t, fixture.router, orphanID); recorder.Code != http.StatusNoContent {
		t.Fatalf("delete the orphaned copy: got %d: %s", recorder.Code, recorder.Body.String())
	}
	if applicationExists(t, fixture.pool, orphanID) {
		t.Fatal("the orphaned copy survived its delete")
	}
}

// A published agent stays protected, and the refusal is a sentence the
// Workshop can show.
func TestAPublishedAgentIsRefusedWithAReason(t *testing.T) {
	fixture := newExpandedFixture(t, "openai")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := fixture.pool.Exec(ctx, `UPDATE p_1.application_versions SET status = 'published' WHERE id = $1`, fixture.versionID); err != nil {
		t.Fatalf("publish the fixture: %v", err)
	}
	recorder := deleteAgent(t, fixture.router, fixture.applicationID)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "Unpublish") {
		t.Fatalf("delete a published agent: got %d: %s", recorder.Code, recorder.Body.String())
	}
	if !applicationExists(t, fixture.pool, fixture.applicationID) {
		t.Fatal("a published agent was deleted")
	}
}
