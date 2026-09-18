package applications_test

// "Replace & Delete" — deleting a version that ANOTHER agent references as a
// sub-agent (#894).
//
// `replacement_version_id` has been a documented query parameter of the delete
// route since the spec was written, and the handler never read it: the delete
// went through regardless, leaving every parent that referenced the version
// pointing at a row that no longer exists. Pylon refuses that delete and
// repoints the references when a replacement is named
// (`legacy/plugins/elitea_core/rpc/application.py:1869-1961`).
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

// attachSubAgentReference writes the pair every reader joins: a tool row of
// type 'application' naming the child version, and the mapping that binds it
// to the PARENT version. The same shape `eliteacore`'s relation route writes.
func attachSubAgentReference(t *testing.T, pool *pgxpool.Pool, parentAppID, parentVersionID, childAppID, childVersionID string) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	settings := fmt.Sprintf(`{"application_id":%s,"application_version_id":%s}`, childAppID, childVersionID)
	var toolID int64
	if err := pool.QueryRow(ctx, `
INSERT INTO p_1.elitea_tools (name, type, description, settings, meta, owner_id, author_id)
VALUES ('child', 'application', '', $1::jsonb, '{}'::jsonb, 1, 1) RETURNING id`, settings).Scan(&toolID); err != nil {
		t.Fatalf("seed the sub-agent tool row: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO p_1.entity_tool_mapping (entity_version_id, entity_type, tool_id)
VALUES ($1, 'agent', $2)`, parentVersionID, toolID); err != nil {
		t.Fatalf("seed the sub-agent mapping: %v", err)
	}
	_ = parentAppID
	return toolID
}

func referencedVersionID(t *testing.T, pool *pgxpool.Pool, toolID int64) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var versionID string
	if err := pool.QueryRow(ctx, `
SELECT COALESCE(settings->>'application_version_id', settings->>'version_id', '')
FROM p_1.elitea_tools WHERE id = $1`, toolID).Scan(&versionID); err != nil {
		t.Fatalf("read the tool settings: %v", err)
	}
	return versionID
}

// A version another agent references cannot be deleted silently.
func TestHandlerPostgres_DeleteVersionRefusesAnInUseVersionWithoutAReplacement(t *testing.T) {
	pool := newHandlerTestPool(t)
	seedHandlerUser(t, pool, 1, "one@elitea.ai")
	router := newHandlerTestServer(t, pool, auth.User{ID: "1", UserID: "1", Email: "one@elitea.ai"})

	_, child := do(t, router, http.MethodPost, "/applications/prompt_lib/1", j14CreateBody("inuse-child"))
	childAppID := child["id"].(string)
	childBaseID := child["version_details"].(map[string]any)["id"].(string)
	_, childSecond := do(t, router, http.MethodPost, "/versions/prompt_lib/1/"+childAppID, map[string]any{"name": "v2"})
	childSecondID := childSecond["id"].(string)

	_, parent := do(t, router, http.MethodPost, "/applications/prompt_lib/1", j14CreateBody("inuse-parent"))
	parentAppID := parent["id"].(string)
	parentVersionID := parent["version_details"].(map[string]any)["id"].(string)
	attachSubAgentReference(t, pool, parentAppID, parentVersionID, childAppID, childSecondID)

	recorder, _ := do(t, router, http.MethodDelete,
		"/version/prompt_lib/1/"+childAppID+"/"+childSecondID, nil)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("deleting an in-use version answered %d %s, want 400", recorder.Code, recorder.Body.String())
	}

	// And it really is still there — a refusal that had deleted the row anyway
	// would pass an assertion on the status code alone.
	_, fetched := do(t, router, http.MethodGet, "/application/prompt_lib/1/"+childAppID, nil)
	if !contains(versionIDs(t, fetched), childSecondID) {
		t.Errorf("the refused delete removed the version anyway")
	}
	_ = childBaseID
}

// The replacement moves the reference and the delete goes through.
func TestHandlerPostgres_DeleteVersionWithAReplacementRepointsTheReference(t *testing.T) {
	pool := newHandlerTestPool(t)
	seedHandlerUser(t, pool, 1, "one@elitea.ai")
	router := newHandlerTestServer(t, pool, auth.User{ID: "1", UserID: "1", Email: "one@elitea.ai"})

	_, child := do(t, router, http.MethodPost, "/applications/prompt_lib/1", j14CreateBody("replace-child"))
	childAppID := child["id"].(string)
	childBaseID := child["version_details"].(map[string]any)["id"].(string)
	_, childSecond := do(t, router, http.MethodPost, "/versions/prompt_lib/1/"+childAppID, map[string]any{"name": "v2"})
	childSecondID := childSecond["id"].(string)

	_, parent := do(t, router, http.MethodPost, "/applications/prompt_lib/1", j14CreateBody("replace-parent"))
	parentAppID := parent["id"].(string)
	parentVersionID := parent["version_details"].(map[string]any)["id"].(string)
	toolID := attachSubAgentReference(t, pool, parentAppID, parentVersionID, childAppID, childSecondID)

	recorder, _ := do(t, router, http.MethodDelete,
		"/version/prompt_lib/1/"+childAppID+"/"+childSecondID+"?replacement_version_id="+childBaseID, nil)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("replace & delete answered %d %s, want 204", recorder.Code, recorder.Body.String())
	}

	_, fetched := do(t, router, http.MethodGet, "/application/prompt_lib/1/"+childAppID, nil)
	if contains(versionIDs(t, fetched), childSecondID) {
		t.Errorf("the deleted version is still listed")
	}
	if got := referencedVersionID(t, pool, toolID); got != childBaseID {
		t.Errorf("the parent still references version %q, want the replacement %q", got, childBaseID)
	}
}

// A replacement belonging to a DIFFERENT agent is refused, and nothing moves.
func TestHandlerPostgres_DeleteVersionRefusesAReplacementFromAnotherApplication(t *testing.T) {
	pool := newHandlerTestPool(t)
	seedHandlerUser(t, pool, 1, "one@elitea.ai")
	router := newHandlerTestServer(t, pool, auth.User{ID: "1", UserID: "1", Email: "one@elitea.ai"})

	_, child := do(t, router, http.MethodPost, "/applications/prompt_lib/1", j14CreateBody("foreign-child"))
	childAppID := child["id"].(string)
	_, childSecond := do(t, router, http.MethodPost, "/versions/prompt_lib/1/"+childAppID, map[string]any{"name": "v2"})
	childSecondID := childSecond["id"].(string)

	_, other := do(t, router, http.MethodPost, "/applications/prompt_lib/1", j14CreateBody("foreign-other"))
	otherVersionID := other["version_details"].(map[string]any)["id"].(string)

	_, parent := do(t, router, http.MethodPost, "/applications/prompt_lib/1", j14CreateBody("foreign-parent"))
	parentAppID := parent["id"].(string)
	parentVersionID := parent["version_details"].(map[string]any)["id"].(string)
	toolID := attachSubAgentReference(t, pool, parentAppID, parentVersionID, childAppID, childSecondID)

	recorder, _ := do(t, router, http.MethodDelete,
		"/version/prompt_lib/1/"+childAppID+"/"+childSecondID+"?replacement_version_id="+otherVersionID, nil)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("a foreign replacement answered %d %s, want 400", recorder.Code, recorder.Body.String())
	}
	if got := referencedVersionID(t, pool, toolID); got != childSecondID {
		t.Errorf("the refused replacement moved the reference to %q anyway", got)
	}
}

// A version nothing references still deletes with no replacement — the
// ordinary case must not have acquired a new requirement.
func TestHandlerPostgres_DeleteVersionStillNeedsNoReplacementWhenNothingReferencesIt(t *testing.T) {
	pool := newHandlerTestPool(t)
	seedHandlerUser(t, pool, 1, "one@elitea.ai")
	router := newHandlerTestServer(t, pool, auth.User{ID: "1", UserID: "1", Email: "one@elitea.ai"})

	_, created := do(t, router, http.MethodPost, "/applications/prompt_lib/1", j14CreateBody("free-delete"))
	applicationID := created["id"].(string)
	_, second := do(t, router, http.MethodPost, "/versions/prompt_lib/1/"+applicationID, map[string]any{"name": "v2"})
	secondVersionID := second["id"].(string)

	// The version uses a tool of its OWN — the relation `check_version_in_use`
	// used to answer, and the one that must NOT count as "in use".
	attachSubAgentReference(t, pool, applicationID, secondVersionID, applicationID, secondVersionID)

	recorder, _ := do(t, router, http.MethodDelete,
		"/version/prompt_lib/1/"+applicationID+"/"+secondVersionID, nil)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("deleting a version that only uses its own tool answered %d %s, want 204",
			recorder.Code, recorder.Body.String())
	}
}
