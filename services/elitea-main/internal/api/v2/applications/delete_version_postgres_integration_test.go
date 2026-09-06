package applications_test

// The DELETE-a-version route had no test at all.
//
// `DeleteVersion` (handler.go:1213) answers two different things, and the web
// client now shows both to the user (#147):
//
//   - 204 No Content, and the version is gone from the list the editor reads.
//   - 400 with `{"error": "Unpublish first. Cannot delete a published
//     version."}` when the version is published or embedded. This message is
//     the only explanation a user gets, so its exact text is a contract.
//
// The 400 branch is guarded by a live SQL read of `application_versions.status`
// and it is unreachable when `h.pool` is nil. The unit-test wiring uses a mock
// repository and no pool, so the mock-based tests in handler_test.go cannot
// reach this branch at all. Only a real database can.
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

// setVersionStatus writes the publish lifecycle column directly.
//
// There is no route that publishes a version in this router table, and the
// publish flow belongs to a different handler. The guard under test reads this
// one column, so the test sets that column and nothing else.
func setVersionStatus(t *testing.T, pool *pgxpool.Pool, projectID, versionID, status string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	tag, err := pool.Exec(ctx, fmt.Sprintf(
		`UPDATE p_%s.application_versions SET status = $1 WHERE id = $2`, projectID), status, versionID)
	if err != nil {
		t.Fatalf("set version status: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("set version status: %d rows changed, want 1", tag.RowsAffected())
	}
}

// versionIDs lists the ids the application detail reports, which is the list
// the version menu renders.
func versionIDs(t *testing.T, body map[string]any) []string {
	t.Helper()
	versions, _ := body["versions"].([]any)
	ids := make([]string, 0, len(versions))
	for _, raw := range versions {
		version, _ := raw.(map[string]any)
		id, _ := version["id"].(string)
		ids = append(ids, id)
	}
	return ids
}

func contains(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// The happy path, read back through the route the editor calls.
//
// A 204 alone is not evidence: a handler that answered 204 and deleted nothing
// would pass an assertion on the status code. The version list is the
// assertion.
func TestHandlerPostgres_DeleteVersionRemovesItFromTheApplication(t *testing.T) {
	pool := newHandlerTestPool(t)
	seedHandlerUser(t, pool, 1, "one@elitea.ai")
	router := newHandlerTestServer(t, pool, auth.User{ID: "1", UserID: "1", Email: "one@elitea.ai"})

	_, created := do(t, router, http.MethodPost, "/applications/prompt_lib/1", j14CreateBody("delete-version"))
	applicationID := created["id"].(string)
	baseVersionID := created["version_details"].(map[string]any)["id"].(string)

	_, second := do(t, router, http.MethodPost, "/versions/prompt_lib/1/"+applicationID, map[string]any{"name": "v2"})
	secondVersionID := second["id"].(string)

	recorder, _ := do(t, router, http.MethodDelete,
		"/version/prompt_lib/1/"+applicationID+"/"+secondVersionID, nil)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("delete version: %d %s", recorder.Code, recorder.Body.String())
	}

	_, fetched := do(t, router, http.MethodGet, "/application/prompt_lib/1/"+applicationID, nil)
	ids := versionIDs(t, fetched)
	if contains(ids, secondVersionID) {
		t.Errorf("the deleted version %s is still listed: %v", secondVersionID, ids)
	}
	if !contains(ids, baseVersionID) {
		t.Errorf("delete removed the wrong rows; %s is gone too: %v", baseVersionID, ids)
	}
}

// A second delete of the same version answers 404, not 204.
//
// The repository reports `RowsAffected() == 0` as a not-found
// (repos/applications.go:633). The web client shows the server's own message,
// so a 204 for a version that was already gone would tell the user a delete
// happened twice.
func TestHandlerPostgres_DeleteVersionAnswersNotFoundTwice(t *testing.T) {
	pool := newHandlerTestPool(t)
	seedHandlerUser(t, pool, 1, "one@elitea.ai")
	router := newHandlerTestServer(t, pool, auth.User{ID: "1", UserID: "1", Email: "one@elitea.ai"})

	_, created := do(t, router, http.MethodPost, "/applications/prompt_lib/1", j14CreateBody("delete-twice"))
	applicationID := created["id"].(string)
	_, second := do(t, router, http.MethodPost, "/versions/prompt_lib/1/"+applicationID, map[string]any{"name": "v2"})
	secondVersionID := second["id"].(string)

	recorder, _ := do(t, router, http.MethodDelete, "/version/prompt_lib/1/"+applicationID+"/"+secondVersionID, nil)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("first delete: %d %s", recorder.Code, recorder.Body.String())
	}

	recorder, _ = do(t, router, http.MethodDelete, "/version/prompt_lib/1/"+applicationID+"/"+secondVersionID, nil)
	if recorder.Code != http.StatusNotFound {
		t.Errorf("second delete: %d %s, want 404", recorder.Code, recorder.Body.String())
	}
}

// The refusal the version menu shows.
//
// Both refused states are exercised. `status` holds the publish lifecycle, and
// the guard names two values; a guard that tested only "published" would pass
// half of this test.
func TestHandlerPostgres_DeleteVersionRefusesAPublishedVersion(t *testing.T) {
	for _, status := range []string{"published", "embedded"} {
		t.Run(status, func(t *testing.T) {
			pool := newHandlerTestPool(t)
			seedHandlerUser(t, pool, 1, "one@elitea.ai")
			router := newHandlerTestServer(t, pool, auth.User{ID: "1", UserID: "1", Email: "one@elitea.ai"})

			_, created := do(t, router, http.MethodPost, "/applications/prompt_lib/1",
				j14CreateBody("delete-"+status))
			applicationID := created["id"].(string)
			_, second := do(t, router, http.MethodPost, "/versions/prompt_lib/1/"+applicationID,
				map[string]any{"name": "v2"})
			secondVersionID := second["id"].(string)

			setVersionStatus(t, pool, "1", secondVersionID, status)

			recorder, body := do(t, router, http.MethodDelete,
				"/version/prompt_lib/1/"+applicationID+"/"+secondVersionID, nil)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("delete a %s version: %d %s, want 400", status, recorder.Code, recorder.Body.String())
			}
			// The exact text. The web client puts this string in front of the
			// user with no rewording, so a change here changes the product.
			const want = "Unpublish first. Cannot delete a published version."
			if got, _ := body["error"].(string); got != want {
				t.Errorf("error = %q, want %q", got, want)
			}

			// The refusal must not be a partial delete. The version is still
			// there.
			_, fetched := do(t, router, http.MethodGet, "/application/prompt_lib/1/"+applicationID, nil)
			if !contains(versionIDs(t, fetched), secondVersionID) {
				t.Errorf("a refused delete removed the version anyway: %v", versionIDs(t, fetched))
			}
		})
	}
}

// A draft version stays deletable. This is the discriminating half of the pair
// above: a guard that refused every delete would pass the refusal test.
func TestHandlerPostgres_DeleteVersionAllowsADraftVersion(t *testing.T) {
	pool := newHandlerTestPool(t)
	seedHandlerUser(t, pool, 1, "one@elitea.ai")
	router := newHandlerTestServer(t, pool, auth.User{ID: "1", UserID: "1", Email: "one@elitea.ai"})

	_, created := do(t, router, http.MethodPost, "/applications/prompt_lib/1", j14CreateBody("delete-draft"))
	applicationID := created["id"].(string)
	_, second := do(t, router, http.MethodPost, "/versions/prompt_lib/1/"+applicationID, map[string]any{"name": "v2"})
	secondVersionID := second["id"].(string)

	setVersionStatus(t, pool, "1", secondVersionID, "draft")

	recorder, _ := do(t, router, http.MethodDelete,
		"/version/prompt_lib/1/"+applicationID+"/"+secondVersionID, nil)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("delete a draft version: %d %s, want 204", recorder.Code, recorder.Body.String())
	}
}
