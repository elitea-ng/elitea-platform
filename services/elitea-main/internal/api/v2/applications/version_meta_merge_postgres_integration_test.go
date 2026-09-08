package applications_test

// An ordinary agent save must not destroy the `meta` keys it does not carry.
//
// This is the HTTP half of the pin (the repository half is
// internal/infra/db/repos/applications_version_meta_merge_postgres_integration_test.go),
// and it is the half that shows how the defect was REACHED: nothing in the
// request below is unusual. It is the body the agent editor sends when a user
// edits a variable — `variables` and no `meta` at all. The handler folds that
// into a one-key `meta`, and while the repository replaced the column, that one
// key was the whole of the version's meta afterwards.
//
// What was lost each time:
//
//	step_limit         one of the four gates the Rust runtime admits an agent
//	                   on, so the agent became unrunnable
//	icon_meta          the agent's icon
//	parent_*           the fork provenance a forked agent is traced by
//
// All of it behind a 201, and invisible from the response the client read,
// because the echo reported the same destroyed object the row now held.
//
// The read-back is through `GET /version/...` — `fetchVersionDetails`, the
// editor's own reload — and not through the write's echo. A merge computed in
// Go and never written would satisfy the echo.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"net/http"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

// createBodyWithFullMeta is the create request for an agent whose meta holds
// every key a real one accumulates, not only the two an editor draft models.
func createBodyWithFullMeta(name string) map[string]any {
	return map[string]any{
		"name":        name,
		"description": "created by the version-contract journey",
		"type":        "agent",
		"versions": []any{map[string]any{
			"name":         "base",
			"agent_type":   "openai",
			"instructions": "Follow the brief.",
			"variables":    []any{map[string]any{"name": "region", "value": "emea"}},
			"meta": map[string]any{
				"step_limit":        float64(7),
				"icon_meta":         map[string]any{"icon": "bolt"},
				"internal_tools":    []any{"canvas"},
				"parent_entity_id":  float64(41),
				"parent_project_id": float64(2),
				"parent_author_id":  float64(3),
			},
		}},
	}
}

func TestHandlerPostgres_SavingVariablesKeepsTheRestOfTheVersionMeta(t *testing.T) {
	pool := newHandlerTestPool(t)
	seedHandlerUser(t, pool, 1, "one@elitea.ai")
	router := newHandlerTestServer(t, pool, auth.User{ID: "1", UserID: "1", Email: "one@elitea.ai"})

	recorder, created := do(t, router, http.MethodPost, "/applications/prompt_lib/1",
		createBodyWithFullMeta("autotest_meta_merge"))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create: status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	applicationID, _ := created["id"].(string)
	details, _ := created["version_details"].(map[string]any)
	versionID, _ := details["id"].(string)
	if applicationID == "" || versionID == "" {
		t.Fatalf("create answered no ids: %v", created)
	}

	// The save. `variables` and NO `meta` — the shape a variables edit takes.
	path := "/version/prompt_lib/1/" + applicationID + "/" + versionID
	saved, echo := do(t, router, http.MethodPut, path, map[string]any{
		"name":      "base",
		"variables": []any{map[string]any{"name": "region", "value": "apac"}},
	})
	if saved.Code != http.StatusCreated {
		t.Fatalf("save: status = %d, body = %s", saved.Code, saved.Body.String())
	}
	echoMeta, _ := echo["meta"].(map[string]any)
	if got := echoMeta["step_limit"]; got != float64(7) {
		t.Errorf("the write echo reports meta.step_limit = %v, want 7", got)
	}

	// …and the row says the same thing on the editor's own reload.
	read, stored := do(t, router, http.MethodGet, path, nil)
	if read.Code != http.StatusOK {
		t.Fatalf("read: status = %d, body = %s", read.Code, read.Body.String())
	}
	meta, ok := stored["meta"].(map[string]any)
	if !ok {
		t.Fatalf("the version read answered no meta object: %#v", stored["meta"])
	}
	if got := meta["step_limit"]; got != float64(7) {
		t.Errorf("meta.step_limit = %v, want 7 — a variables edit destroyed the runtime's admission gate", got)
	}
	if _, present := meta["icon_meta"]; !present {
		t.Errorf("meta.icon_meta is gone after an ordinary save: %v", meta)
	}
	if _, present := meta["internal_tools"]; !present {
		t.Errorf("meta.internal_tools is gone after an ordinary save: %v", meta)
	}
	for _, key := range []string{"parent_entity_id", "parent_project_id", "parent_author_id"} {
		if _, present := meta[key]; !present {
			t.Errorf("meta.%s is gone — a forked agent lost its provenance to a variables edit: %v", key, meta)
		}
	}

	// The edit itself still landed, in BOTH stores: `meta.variables` (what
	// the write folds) and application_variables (what the read projects).
	edited, ok := meta["variables"].([]any)
	if !ok || len(edited) != 1 {
		t.Fatalf("meta.variables = %v, want the one edited variable", meta["variables"])
	}
	if first, _ := edited[0].(map[string]any); first["value"] != "apac" {
		t.Errorf("meta.variables[0] = %v, want the edited value", edited[0])
	}
	rows, ok := stored["variables"].([]any)
	if !ok || len(rows) != 1 {
		t.Fatalf("variables = %v, want the one edited variable", stored["variables"])
	}
	if row, _ := rows[0].(map[string]any); row["value"] != "apac" {
		t.Errorf("variables[0] = %v, want the edited value", rows[0])
	}
}

// The editor's own save shape: a `meta` that models two keys out of six. It
// must write those two and leave the other four alone.
func TestHandlerPostgres_AVersionSaveKeepsTheMetaKeysTheClientDoesNotModel(t *testing.T) {
	pool := newHandlerTestPool(t)
	seedHandlerUser(t, pool, 1, "one@elitea.ai")
	router := newHandlerTestServer(t, pool, auth.User{ID: "1", UserID: "1", Email: "one@elitea.ai"})

	_, created := do(t, router, http.MethodPost, "/applications/prompt_lib/1",
		createBodyWithFullMeta("autotest_meta_partial"))
	applicationID, _ := created["id"].(string)
	details, _ := created["version_details"].(map[string]any)
	versionID, _ := details["id"].(string)
	if applicationID == "" || versionID == "" {
		t.Fatalf("create answered no ids: %v", created)
	}

	path := "/version/prompt_lib/1/" + applicationID + "/" + versionID
	saved, _ := do(t, router, http.MethodPut, path, map[string]any{
		"name": "base",
		// entities/application-form/model/mutations.ts, toVersionWriteRequest.
		"meta":      map[string]any{"step_limit": float64(40), "internal_tools": []any{}},
		"variables": []any{},
	})
	if saved.Code != http.StatusCreated {
		t.Fatalf("save: status = %d, body = %s", saved.Code, saved.Body.String())
	}

	_, stored := do(t, router, http.MethodGet, path, nil)
	meta, ok := stored["meta"].(map[string]any)
	if !ok {
		t.Fatalf("the version read answered no meta object: %#v", stored["meta"])
	}
	// The two keys the client modelled won.
	if got := meta["step_limit"]; got != float64(40) {
		t.Errorf("meta.step_limit = %v, want 40 — the client's own key must win", got)
	}
	tools, ok := meta["internal_tools"].([]any)
	if !ok || len(tools) != 0 {
		t.Errorf("meta.internal_tools = %v, want an empty list — removing the last one is a real action",
			meta["internal_tools"])
	}
	// The four it did not survive.
	if _, present := meta["icon_meta"]; !present {
		t.Errorf("meta.icon_meta is gone: %v", meta)
	}
	for _, key := range []string{"parent_entity_id", "parent_project_id", "parent_author_id"} {
		if _, present := meta[key]; !present {
			t.Errorf("meta.%s is gone: %v", key, meta)
		}
	}
}
