package applications_test

// Nothing on the read side reported WHICH version is the default.
//
// `SetDefaultVersion` writes `applications.meta.default_version_id`
// (repos/applications.go:650-682) and the write worked; the read did not exist.
// `Get` built its response map from seven hand-picked keys and `meta` was not
// one of them, and `getVersions` selected `id, name, status, agent_type,
// created_at` with no `is_default`. So after a client set a default, nothing it
// could fetch would tell it — and on a page reload the affordance was back to
// guessing (`apps/elitea-web/.../AgentVersionControls.tsx` kept the id it had
// just set in component state, which a reload throws away).
//
// WHAT THE RED RUN SHOWS. Against the unchanged handler, every assertion below
// on `meta.default_version_id` or on a version's `is_default` fails: the keys
// are absent from a 200 that is otherwise correct. The SET half already passed,
// which is why nothing caught this.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"net/http"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

// versionByID finds one entry of the `versions` array by its id.
func versionByID(t *testing.T, body map[string]any, id string) map[string]any {
	t.Helper()
	versions, _ := body["versions"].([]any)
	for _, raw := range versions {
		version, _ := raw.(map[string]any)
		if version["id"] == id {
			return version
		}
	}
	t.Fatalf("version %q is not in %v", id, body["versions"])
	return nil
}

// defaultVersionID reads meta.default_version_id, reporting the shape it found
// rather than panicking on a missing key.
func defaultVersionID(t *testing.T, body map[string]any) string {
	t.Helper()
	meta, ok := body["meta"].(map[string]any)
	if !ok {
		t.Fatalf("the response carries no meta object: %v", body)
	}
	id, _ := meta["default_version_id"].(string)
	return id
}

// The whole round trip the UI needs: set a default, read the application back,
// and find out which version it is — from the application's meta and from the
// version row itself.
func TestHandlerPostgres_GetReportsWhichVersionIsDefault(t *testing.T) {
	pool := newHandlerTestPool(t)
	seedHandlerUser(t, pool, 1, "one@elitea.ai")
	router := newHandlerTestServer(t, pool, auth.User{ID: "1", UserID: "1", Email: "one@elitea.ai"})

	_, created := do(t, router, http.MethodPost, "/applications/prompt_lib/1", j14CreateBody("default-readback"))
	applicationID := created["id"].(string)
	baseVersionID := created["version_details"].(map[string]any)["id"].(string)

	// A second version, so "is_default" has something to discriminate against.
	recorder, second := do(t, router, http.MethodPost, "/versions/prompt_lib/1/"+applicationID,
		map[string]any{"name": "v2"})
	if recorder.Code != http.StatusOK && recorder.Code != http.StatusCreated {
		t.Fatalf("create second version: %d %s", recorder.Code, recorder.Body.String())
	}
	secondVersionID := second["id"].(string)

	// Before any default is set the application reports none — an empty string,
	// not a missing `meta` object. The two are different answers: "no default
	// recorded" versus "this response cannot tell you".
	_, fetched := do(t, router, http.MethodGet, "/application/prompt_lib/1/"+applicationID, nil)
	if got := defaultVersionID(t, fetched); got != "" {
		t.Errorf("a fresh application reports default_version_id = %q, want the empty string", got)
	}
	for _, id := range []string{baseVersionID, secondVersionID} {
		if versionByID(t, fetched, id)["is_default"] != false {
			t.Errorf("version %s claims to be the default before one was set: %v", id, versionByID(t, fetched, id))
		}
	}

	recorder, _ = do(t, router, http.MethodPatch,
		"/default_version/prompt_lib/1/"+applicationID+"/"+secondVersionID, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("set default version: %d %s", recorder.Code, recorder.Body.String())
	}

	_, afterSet := do(t, router, http.MethodGet, "/application/prompt_lib/1/"+applicationID, nil)
	if got := defaultVersionID(t, afterSet); got != secondVersionID {
		t.Errorf("meta.default_version_id = %q, want %q", got, secondVersionID)
	}
	if versionByID(t, afterSet, secondVersionID)["is_default"] != true {
		t.Errorf("the version that was just made default does not say so: %v", versionByID(t, afterSet, secondVersionID))
	}
	if versionByID(t, afterSet, baseVersionID)["is_default"] != false {
		t.Errorf("a second version claims to be the default too: %v", versionByID(t, afterSet, baseVersionID))
	}
}

// Moving the default moves the flag. Exactly one version may carry it, so a
// handler that ORed the new default onto the old one would pass the test above
// and fail here.
func TestHandlerPostgres_ExactlyOneVersionIsDefault(t *testing.T) {
	pool := newHandlerTestPool(t)
	seedHandlerUser(t, pool, 1, "one@elitea.ai")
	router := newHandlerTestServer(t, pool, auth.User{ID: "1", UserID: "1", Email: "one@elitea.ai"})

	_, created := do(t, router, http.MethodPost, "/applications/prompt_lib/1", j14CreateBody("default-moves"))
	applicationID := created["id"].(string)
	baseVersionID := created["version_details"].(map[string]any)["id"].(string)
	_, second := do(t, router, http.MethodPost, "/versions/prompt_lib/1/"+applicationID, map[string]any{"name": "v2"})
	secondVersionID := second["id"].(string)

	for _, want := range []string{secondVersionID, baseVersionID} {
		recorder, _ := do(t, router, http.MethodPatch,
			"/default_version/prompt_lib/1/"+applicationID+"/"+want, nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("set default to %s: %d %s", want, recorder.Code, recorder.Body.String())
		}

		_, fetched := do(t, router, http.MethodGet, "/application/prompt_lib/1/"+applicationID, nil)
		if got := defaultVersionID(t, fetched); got != want {
			t.Errorf("meta.default_version_id = %q, want %q", got, want)
		}

		var defaults []string
		versions, _ := fetched["versions"].([]any)
		for _, raw := range versions {
			version, _ := raw.(map[string]any)
			if version["is_default"] == true {
				defaults = append(defaults, version["id"].(string))
			}
		}
		if len(defaults) != 1 || defaults[0] != want {
			t.Errorf("versions flagged default = %v, want exactly [%s]", defaults, want)
		}
	}
}
