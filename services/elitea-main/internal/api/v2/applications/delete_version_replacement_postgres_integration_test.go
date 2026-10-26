package applications_test

// "Replace & Delete" — deleting a version that ANOTHER agent references as a
// sub-agent (#894).
//
// `replacement_version_id` has been a documented query parameter of the delete
// route since the spec was written, and the handler never read it: the delete
// went through regardless, leaving every parent that referenced the version
// pointing at a row that no longer exists. The legacy runtime refuses that
// delete and repoints the references when a replacement is named
// (`legacy/plugins/elitea_core/rpc/application.py:1869-1961`).
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
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
INSERT INTO p_1.elitea_tools (name, type, description, settings, meta, owner_id, author_id, created_at)
VALUES ('child', 'application', '', $1::jsonb, '{}'::jsonb, 1, 1, now() AT TIME ZONE 'UTC') RETURNING id`, settings).Scan(&toolID); err != nil {
		t.Fatalf("seed the sub-agent tool row: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO p_1.entity_tool_mapping (entity_version_id, entity_type, tool_id, created_at)
VALUES ($1, 'agent', $2, now() AT TIME ZONE 'UTC')`, parentVersionID, toolID); err != nil {
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

// FU-244, PLAN §8.8: re-pointing is a version switch for every parent. A
// replacement whose own attachments reach the parent would close a cycle, so
// the delete is refused with the composition refusal and nothing moves.
func TestHandlerPostgres_DeleteVersionRefusesAReplacementThatClosesACycle(t *testing.T) {
	pool := newHandlerTestPool(t)
	seedHandlerUser(t, pool, 1, "one@elitea.ai")
	router := newHandlerTestServer(t, pool, auth.User{ID: "1", UserID: "1", Email: "one@elitea.ai"})

	_, child := do(t, router, http.MethodPost, "/applications/prompt_lib/1", j14CreateBody("cycle-child"))
	childAppID := child["id"].(string)
	childBaseID := child["version_details"].(map[string]any)["id"].(string)
	_, childSecond := do(t, router, http.MethodPost, "/versions/prompt_lib/1/"+childAppID, map[string]any{"name": "v2"})
	childSecondID := childSecond["id"].(string)

	_, parent := do(t, router, http.MethodPost, "/applications/prompt_lib/1", j14CreateBody("cycle-parent"))
	parentAppID := parent["id"].(string)
	parentVersionID := parent["version_details"].(map[string]any)["id"].(string)
	toolID := attachSubAgentReference(t, pool, parentAppID, parentVersionID, childAppID, childSecondID)
	// The base version of the child uses the parent.
	attachSubAgentReference(t, pool, childAppID, childBaseID, parentAppID, parentVersionID)

	recorder, body := do(t, router, http.MethodDelete,
		"/version/prompt_lib/1/"+childAppID+"/"+childSecondID+"?replacement_version_id="+childBaseID, nil)
	message, _ := body["error"].(string)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(message, `"cycle-parent" → "cycle-child" → "cycle-parent"`) {
		t.Fatalf("a re-point closing a cycle answered %d %s", recorder.Code, recorder.Body.String())
	}
	if got := referencedVersionID(t, pool, toolID); got != childSecondID {
		t.Errorf("the refused re-point moved the reference to %q", got)
	}
	_, fetched := do(t, router, http.MethodGet, "/application/prompt_lib/1/"+childAppID, nil)
	if !contains(versionIDs(t, fetched), childSecondID) {
		t.Errorf("the refused delete removed the version anyway")
	}
}

// Review item 8: the replacement is walked for the whole composition, not only
// in the parent being checked. P1 attaches P2 and the version being deleted;
// P2 attaches it too. The replacement is a 2-level agent, so through P2 it puts
// a fourth level under P1, which neither parent shows when only its own
// reference is swapped.
func TestHandlerPostgres_DeleteVersionWalksTheReplacementThroughEveryParent(t *testing.T) {
	pool := newHandlerTestPool(t)
	seedHandlerUser(t, pool, 1, "one@elitea.ai")
	router := newHandlerTestServer(t, pool, auth.User{ID: "1", UserID: "1", Email: "one@elitea.ai"})
	create := func(name string) (string, string) {
		_, created := do(t, router, http.MethodPost, "/applications/prompt_lib/1", j14CreateBody(name))
		return created["id"].(string), created["version_details"].(map[string]any)["id"].(string)
	}

	leafApp, leafVersion := create("depth-leaf")
	targetApp, targetBase := create("depth-target")
	_, second := do(t, router, http.MethodPost, "/versions/prompt_lib/1/"+targetApp, map[string]any{"name": "v2"})
	targetSecond := second["id"].(string)
	// The replacement (base) is two levels: it attaches the leaf.
	attachSubAgentReference(t, pool, targetApp, targetBase, leafApp, leafVersion)
	p2App, p2Version := create("depth-p2")
	p2Tool := attachSubAgentReference(t, pool, p2App, p2Version, targetApp, targetSecond)
	p1App, p1Version := create("depth-p1")
	attachSubAgentReference(t, pool, p1App, p1Version, p2App, p2Version)
	attachSubAgentReference(t, pool, p1App, p1Version, targetApp, targetSecond)

	recorder, body := do(t, router, http.MethodDelete,
		"/version/prompt_lib/1/"+targetApp+"/"+targetSecond+"?replacement_version_id="+targetBase, nil)
	message, _ := body["error"].(string)
	if recorder.Code != http.StatusBadRequest ||
		!strings.Contains(message, `"depth-p1" → "depth-p2" → "depth-target" → "depth-leaf"`) {
		t.Fatalf("a re-point nesting a fourth level answered %d %s", recorder.Code, recorder.Body.String())
	}
	if got := referencedVersionID(t, pool, p2Tool); got != targetSecond {
		t.Errorf("the refused re-point moved the reference to %q", got)
	}
}

// Review round 2 (B): an attach of the version being replaced that is in flight
// when the re-point starts is not moved unwalked. The attach holds the child
// version row FOR SHARE (as the relation route does); the re-point locks it FOR
// UPDATE first, so it waits for the attach, then reads and walks the new parent.
// Here the replacement attaches that new parent, so the move would close a
// cycle and is refused.
func TestHandlerPostgres_DeleteVersionWaitsForAConcurrentAttachAndWalksIt(t *testing.T) {
	pool := newHandlerTestPool(t)
	seedHandlerUser(t, pool, 1, "one@elitea.ai")
	router := newHandlerTestServer(t, pool, auth.User{ID: "1", UserID: "1", Email: "one@elitea.ai"})
	create := func(name string) (string, string) {
		_, created := do(t, router, http.MethodPost, "/applications/prompt_lib/1", j14CreateBody(name))
		return created["id"].(string), created["version_details"].(map[string]any)["id"].(string)
	}
	newParentApp, newParentVersion := create("race-parent")
	targetApp, targetBase := create("race-target")
	_, second := do(t, router, http.MethodPost, "/versions/prompt_lib/1/"+targetApp, map[string]any{"name": "v2"})
	targetSecond := second["id"].(string)
	attachSubAgentReference(t, pool, targetApp, targetBase, newParentApp, newParentVersion)
	// A parent that already uses the version, so the delete re-points.
	existingApp, existingVersion := create("race-existing")
	existingTool := attachSubAgentReference(t, pool, existingApp, existingVersion, targetApp, targetSecond)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	attach, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = attach.Rollback(context.Background()) }()
	if _, err := attach.Exec(ctx, `SELECT 1 FROM p_1.application_versions WHERE id = $1 FOR SHARE`, targetSecond); err != nil {
		t.Fatal(err)
	}
	var toolID int64
	if err := attach.QueryRow(ctx, `
INSERT INTO p_1.elitea_tools (name, type, description, settings, meta, owner_id, author_id, created_at)
VALUES ('race-target', 'application', '', jsonb_build_object('application_id', $1::int, 'application_version_id', $2::int), '{}'::jsonb, 1, 1, now())
RETURNING id`, targetApp, targetSecond).Scan(&toolID); err != nil {
		t.Fatal(err)
	}
	if _, err := attach.Exec(ctx, `INSERT INTO p_1.entity_tool_mapping (entity_version_id, entity_type, tool_id, created_at)
		VALUES ($1, 'agent', $2, now())`, newParentVersion, toolID); err != nil {
		t.Fatal(err)
	}

	type answer struct {
		code int
		body string
	}
	answered := make(chan answer, 1)
	go func() {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodDelete,
			"/version/prompt_lib/1/"+targetApp+"/"+targetSecond+"?replacement_version_id="+targetBase, nil)
		router.ServeHTTP(recorder, request.WithContext(auth.ContextWithUser(request.Context(),
			auth.User{ID: "1", UserID: "1", Email: "one@elitea.ai"})))
		answered <- answer{recorder.Code, recorder.Body.String()}
	}()
	// The re-point must be waiting on the version lock while the attach is open.
	waiting := false
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline) && !waiting; {
		var query string
		if err := pool.QueryRow(ctx, `SELECT coalesce(max(query), '') FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&query); err != nil {
			t.Fatal(err)
		}
		waiting = strings.Contains(query, "FOR UPDATE")

		select {
		case early := <-answered:
			t.Fatalf("the re-point did not wait for the attach: %d %s", early.code, early.body)
		case <-time.After(20 * time.Millisecond):
		}
	}
	if !waiting {
		t.Fatal("the re-point never waited on the version lock")
	}
	if err := attach.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	got := <-answered
	if got.code != http.StatusBadRequest || !strings.Contains(got.body, "circular") {
		t.Fatalf("the re-point answered %d %s", got.code, got.body)
	}
	if moved := referencedVersionID(t, pool, toolID); moved != targetSecond {
		t.Fatalf("the concurrent attach's reference was moved to %q unwalked", moved)
	}
	if moved := referencedVersionID(t, pool, existingTool); moved != targetSecond {
		t.Fatalf("a refused re-point moved the existing reference to %q", moved)
	}
}
