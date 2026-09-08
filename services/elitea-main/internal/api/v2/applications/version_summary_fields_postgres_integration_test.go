package applications_test

// Issue 844: the version SUMMARY dropped the two fields a client reads a
// version's content from.
//
// `GET /application/prompt_lib/{project}/{id}` answers a `versions` array of
// summaries. Pylon's `ApplicationVersionListModel`
// (legacy/plugins/elitea_core/models/pd/version.py) carried `instructions` and
// `meta` on every entry; Go's `getVersions` projection selected neither, so a
// caller that read the prompt text off the LIST — which is what the legacy
// public API suite's export/import round trip does, and what any client that
// wants a version's body without a second request per version does — read
// `null` from a 200 that looked complete in every other field.
//
// WHAT THE RED RUN SHOWS. Against the unchanged projection both assertions on
// `instructions` and both on `meta` fail with the key absent. The detail half
// of the same response (`version_details`) already carried them, which is why
// nothing caught this: a test that read the response's own instructions found
// them, one field away from the array that had lost them.
//
// The two fields the summary carries that pylon did NOT (`agent_type` and
// `is_default`) are asserted here too, in the same read: both are live reads in
// apps/elitea-web (editApplicationMappers.ts / editPipelineMappers.ts, and
// useVersionBarCommands.ts), so restoring parity by deleting them would blank a
// control that works today. This test is what says so.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"net/http"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

// TestHandlerPostgres_VersionSummaryCarriesInstructionsAndMeta is the
// acceptance test for issue 844.
func TestHandlerPostgres_VersionSummaryCarriesInstructionsAndMeta(t *testing.T) {
	pool := newHandlerTestPool(t)
	seedHandlerUser(t, pool, 1, "one@elitea.ai")
	router := newHandlerTestServer(t, pool, auth.User{ID: "1", UserID: "1", Email: "one@elitea.ai"})

	_, created := do(t, router, http.MethodPost, "/applications/prompt_lib/1", j14CreateBody("summary-fields"))
	applicationID, _ := created["id"].(string)
	if applicationID == "" {
		t.Fatalf("create answered no id: %v", created)
	}
	baseVersionID, _ := created["version_details"].(map[string]any)["id"].(string)

	_, fetched := do(t, router, http.MethodGet, "/application/prompt_lib/1/"+applicationID, nil)
	summary := versionByID(t, fetched, baseVersionID)

	// The field the issue is about. `j14CreateBody` seeds this exact text, and
	// the detail beside it reports the same string — so a mismatch here is the
	// projection and not the write.
	if got := summary["instructions"]; got != "Follow the brief." {
		t.Errorf("versions[].instructions = %v, want %q (issue 844)", got, "Follow the brief.")
	}
	detail, _ := fetched["version_details"].(map[string]any)
	if detail["instructions"] != summary["instructions"] {
		t.Errorf("the summary and the detail of the SAME version disagree on instructions: %v vs %v",
			summary["instructions"], detail["instructions"])
	}

	// `meta` is an object, not a string, and the assertion reads a key out of
	// it: a projection that emitted `meta` as the raw jsonb TEXT would satisfy
	// "the key is present" and fail this.
	meta, ok := summary["meta"].(map[string]any)
	if !ok {
		t.Fatalf("versions[].meta is not an object: %#v (issue 844)", summary["meta"])
	}
	if got := meta["step_limit"]; got != float64(25) {
		t.Errorf("versions[].meta.step_limit = %v, want 25 — the seeded meta did not survive the projection", got)
	}

	// The two fields kept deliberately, each with a live reader in elitea-web.
	if got := summary["agent_type"]; got != "openai" {
		t.Errorf("versions[].agent_type = %v, want \"openai\" — editApplicationMappers.ts reads it", got)
	}
	if _, present := summary["is_default"]; !present {
		t.Errorf("versions[].is_default is absent — useVersionBarCommands.ts reads it: %v", summary)
	}
}

// A version whose columns are NULL must answer the EMPTY values, not `null` and
// not a missing key. `""` and `{}` are what every other read of these two
// columns degrades to (fetchVersionDetails COALESCEs both), and a client that
// switched on `instructions === undefined` would otherwise treat "no prompt
// yet" as "this response cannot tell you".
func TestHandlerPostgres_VersionSummaryEmptyInstructionsAndMetaAreNotNull(t *testing.T) {
	pool := newHandlerTestPool(t)
	seedHandlerUser(t, pool, 1, "one@elitea.ai")
	router := newHandlerTestServer(t, pool, auth.User{ID: "1", UserID: "1", Email: "one@elitea.ai"})

	_, created := do(t, router, http.MethodPost, "/applications/prompt_lib/1", j14CreateBody("summary-empty"))
	applicationID, _ := created["id"].(string)

	// `POST /versions/...` with a bare name leaves instructions and meta at
	// their column defaults — the shape this case is about.
	recorder, second := do(t, router, http.MethodPost, "/versions/prompt_lib/1/"+applicationID,
		map[string]any{"name": "bare"})
	if recorder.Code != http.StatusOK && recorder.Code != http.StatusCreated {
		t.Fatalf("create second version: %d %s", recorder.Code, recorder.Body.String())
	}
	bareVersionID, _ := second["id"].(string)

	_, fetched := do(t, router, http.MethodGet, "/application/prompt_lib/1/"+applicationID, nil)
	summary := versionByID(t, fetched, bareVersionID)

	if got := summary["instructions"]; got != "" {
		t.Errorf("an unset instructions column answers %#v, want the empty string", got)
	}
	// An OBJECT, whatever `CreateVersion` chose to seed into it (it writes a
	// default `step_limit`). The claim is the shape: never `null`, never a
	// missing key, and never the raw jsonb text.
	if _, ok := summary["meta"].(map[string]any); !ok {
		t.Errorf("a version created with a bare name answers meta = %#v, want an object", summary["meta"])
	}
}
