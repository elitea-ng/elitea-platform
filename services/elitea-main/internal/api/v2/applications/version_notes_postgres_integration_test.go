package applications_test

// The agent editor's "Editor Notes" field, end to end (#898).
//
// The field was written, unit-tested and mounted by nobody, because
// `version_details.notes` "has no column on `application_versions`, no
// property on `VersionWriteRequest`, and no branch in `UpdateVersion`". It
// never wanted a column: pylon stores it inside the `meta` jsonb
// (elitea_issues #5410 chose that over a per-tenant schema migration), folds
// the top-level write field in, and lifts it back out on read. This service
// does the same, so both spellings — top-level `notes` and `meta.notes` —
// reach the one store, and both reads answer it.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"net/http"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

func notesOf(t *testing.T, body map[string]any) string {
	t.Helper()
	notes, _ := body["notes"].(string)
	return notes
}

func metaNotesOf(t *testing.T, body map[string]any) string {
	t.Helper()
	meta, _ := body["meta"].(map[string]any)
	notes, _ := meta["notes"].(string)
	return notes
}

// The write path a pylon-shaped client uses: `notes` at the top level.
func TestHandlerPostgres_VersionNotesRoundTripThroughTheTopLevelField(t *testing.T) {
	pool := newHandlerTestPool(t)
	seedHandlerUser(t, pool, 1, "one@elitea.ai")
	router := newHandlerTestServer(t, pool, auth.User{ID: "1", UserID: "1", Email: "one@elitea.ai"})

	_, created := do(t, router, http.MethodPost, "/applications/prompt_lib/1", j14CreateBody("notes-toplevel"))
	applicationID := created["id"].(string)
	versionID := created["version_details"].(map[string]any)["id"].(string)

	const note = "Ask the design team before changing the tone."
	recorder, updated := do(t, router, http.MethodPut,
		"/version/prompt_lib/1/"+applicationID+"/"+versionID,
		map[string]any{"name": "base", "notes": note})
	if recorder.Code != http.StatusCreated {
		t.Fatalf("save with notes: %d %s", recorder.Code, recorder.Body.String())
	}
	// The ECHO answers it, so the editor's own save response and its reload
	// agree about where the field is.
	if got := notesOf(t, updated); got != note {
		t.Errorf("the save echo answered notes %q, want %q", got, note)
	}

	_, reloaded := do(t, router, http.MethodGet, "/version/prompt_lib/1/"+applicationID+"/"+versionID, nil)
	if got := notesOf(t, reloaded); got != note {
		t.Errorf("the reload answered notes %q, want %q", got, note)
	}
	// And it is answered INSIDE meta as well. The agent editor round-trips
	// `meta` wholesale, so a read that hid the key there would make an
	// ordinary save of any other field erase the notes.
	if got := metaNotesOf(t, reloaded); got != note {
		t.Errorf("the reload answered meta.notes %q, want %q", got, note)
	}
}

// The write path the agent editor uses: the value inside the `meta` patch.
func TestHandlerPostgres_VersionNotesRoundTripThroughMeta(t *testing.T) {
	pool := newHandlerTestPool(t)
	seedHandlerUser(t, pool, 1, "one@elitea.ai")
	router := newHandlerTestServer(t, pool, auth.User{ID: "1", UserID: "1", Email: "one@elitea.ai"})

	_, created := do(t, router, http.MethodPost, "/applications/prompt_lib/1", j14CreateBody("notes-meta"))
	applicationID := created["id"].(string)
	versionID := created["version_details"].(map[string]any)["id"].(string)

	const note = "This agent is owned by the platform team."
	recorder, _ := do(t, router, http.MethodPut,
		"/version/prompt_lib/1/"+applicationID+"/"+versionID,
		map[string]any{"name": "base", "meta": map[string]any{"step_limit": float64(25), "notes": note}})
	if recorder.Code != http.StatusCreated {
		t.Fatalf("save with meta.notes: %d %s", recorder.Code, recorder.Body.String())
	}

	_, reloaded := do(t, router, http.MethodGet, "/version/prompt_lib/1/"+applicationID+"/"+versionID, nil)
	if got := notesOf(t, reloaded); got != note {
		t.Errorf("the reload answered notes %q, want %q", got, note)
	}

	// An emptied field CLEARS the stored value. The patch merge cannot delete
	// a key, and "" is what the editor's emptied box sends — a save that left
	// the old text in place would be the same silent-discard defect in
	// reverse.
	do(t, router, http.MethodPut,
		"/version/prompt_lib/1/"+applicationID+"/"+versionID,
		map[string]any{"name": "base", "notes": ""})
	_, cleared := do(t, router, http.MethodGet, "/version/prompt_lib/1/"+applicationID+"/"+versionID, nil)
	if got := notesOf(t, cleared); got != "" {
		t.Errorf("clearing the field left notes %q", got)
	}
}

// A save that names no `notes` key at all leaves the stored value alone.
func TestHandlerPostgres_VersionNotesSurviveASaveThatDoesNotNameThem(t *testing.T) {
	pool := newHandlerTestPool(t)
	seedHandlerUser(t, pool, 1, "one@elitea.ai")
	router := newHandlerTestServer(t, pool, auth.User{ID: "1", UserID: "1", Email: "one@elitea.ai"})

	_, created := do(t, router, http.MethodPost, "/applications/prompt_lib/1", j14CreateBody("notes-untouched"))
	applicationID := created["id"].(string)
	versionID := created["version_details"].(map[string]any)["id"].(string)

	const note = "Do not change the welcome message without asking support."
	if recorder, _ := do(t, router, http.MethodPut,
		"/version/prompt_lib/1/"+applicationID+"/"+versionID,
		map[string]any{"name": "base", "notes": note}); recorder.Code != http.StatusCreated {
		t.Fatalf("seed notes: %d", recorder.Code)
	}
	if recorder, _ := do(t, router, http.MethodPut,
		"/version/prompt_lib/1/"+applicationID+"/"+versionID,
		map[string]any{"name": "base", "instructions": "Follow the new brief."}); recorder.Code != http.StatusCreated {
		t.Fatalf("second save: %d", recorder.Code)
	}

	_, reloaded := do(t, router, http.MethodGet, "/version/prompt_lib/1/"+applicationID+"/"+versionID, nil)
	if got := notesOf(t, reloaded); got != note {
		t.Errorf("a save that named no notes key answered notes %q, want %q", got, note)
	}
}

// The ceiling pylon's model declares (max_length=1000), enforced here so an
// API caller cannot store what the editor can never show back.
func TestHandlerPostgres_VersionNotesRefuseMoreThanAThousandCharacters(t *testing.T) {
	pool := newHandlerTestPool(t)
	seedHandlerUser(t, pool, 1, "one@elitea.ai")
	router := newHandlerTestServer(t, pool, auth.User{ID: "1", UserID: "1", Email: "one@elitea.ai"})

	_, created := do(t, router, http.MethodPost, "/applications/prompt_lib/1", j14CreateBody("notes-ceiling"))
	applicationID := created["id"].(string)
	versionID := created["version_details"].(map[string]any)["id"].(string)

	recorder, _ := do(t, router, http.MethodPut,
		"/version/prompt_lib/1/"+applicationID+"/"+versionID,
		map[string]any{"name": "base", "notes": strings.Repeat("x", 1001)})
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("1001 characters of notes answered %d, want 400", recorder.Code)
	}

	// Exactly the ceiling is accepted — an off-by-one here would make the
	// editor's own maxLength unsaveable at its limit.
	recorder, _ = do(t, router, http.MethodPut,
		"/version/prompt_lib/1/"+applicationID+"/"+versionID,
		map[string]any{"name": "base", "notes": strings.Repeat("x", 1000)})
	if recorder.Code != http.StatusCreated {
		t.Fatalf("1000 characters of notes answered %d, want 201", recorder.Code)
	}
}
