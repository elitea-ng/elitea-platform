package applications_test

// SEC-11: the save-time bound includes the YAML expansion budget the Worker
// parses a stored pipeline under. A definition inside the byte and node bounds
// whose anchors and aliases expand past it is refused with a typed 400 on
// create and on update, and nothing is stored; one exactly at the budget saves.
// The documents are the shared boundary cases
// (testdata/pipeline-yaml-budget/cases.json).
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func budgetCaseYAML(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "..", "..", "testdata", "pipeline-yaml-budget", "cases.json"))
	if err != nil {
		t.Fatalf("read shared cases: %v", err)
	}
	var fixture struct {
		Cases []struct{ Name, YAML string } `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatalf("parse shared cases: %v", err)
	}
	for _, c := range fixture.Cases {
		if c.Name == name {
			return c.YAML
		}
	}
	t.Fatalf("no shared case %q", name)
	return ""
}

const expansionRefusalText = "once anchors and aliases are expanded"

func TestHandlerPostgres_PipelineExpansionBudgetOnCreateAndUpdate(t *testing.T) {
	f := newLimitsFixture(t)
	atNodes := budgetCaseYAML(t, "nodes at the limit")
	refused := map[string]string{
		"nodes+1":        budgetCaseYAML(t, "nodes one past the limit"),
		"scalar bytes+1": budgetCaseYAML(t, "scalar bytes one past the limit"),
		"depth+1":        budgetCaseYAML(t, "depth one past the limit"),
		"alias bomb":     budgetCaseYAML(t, "alias bomb"),
	}

	// Create: at the budget saves, past it refuses and stores nothing.
	recorder, _ := do(t, f.router, http.MethodPost, "/applications/prompt_lib/1", pipelineCreateBody("at-expansion", "pipeline", atNodes))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("at the budget: %d %s", recorder.Code, truncateForLog(recorder.Body.String()))
	}
	for name, doc := range refused {
		recorder, _ := do(t, f.router, http.MethodPost, "/applications/prompt_lib/1", pipelineCreateBody("over-"+name, "pipeline", doc))
		requireLimitRefusal(t, "create "+name, recorder.Code, recorder.Body.String(), expansionRefusalText)
		if n := f.count(t, `SELECT count(*) FROM p_1.applications WHERE name = $1`, "over-"+name); n != 0 {
			t.Errorf("refused create %q left %d application rows", name, n)
		}
	}

	// Create version and update version, with the stored type and with the
	// type in the body.
	appID, versionID := f.createPipeline(t, "expansion-updates", "pipeline")
	before := f.count(t, `SELECT count(*) FROM p_1.application_versions WHERE application_id = $1`, mustAtoi(t, appID))
	for name, doc := range refused {
		recorder, _ := do(t, f.router, http.MethodPost, "/versions/prompt_lib/1/"+appID,
			map[string]any{"name": "v-over", "agent_type": "pipeline", "instructions": doc})
		requireLimitRefusal(t, "create version "+name, recorder.Code, recorder.Body.String(), expansionRefusalText)
		for _, body := range []map[string]any{
			{"instructions": doc},
			{"agent_type": "pipeline", "instructions": doc, "name": "renamed"},
		} {
			recorder, _ := do(t, f.router, http.MethodPut, "/version/prompt_lib/1/"+appID+"/"+versionID, body)
			requireLimitRefusal(t, "update version "+name, recorder.Code, recorder.Body.String(), expansionRefusalText)
		}
	}
	if n := f.count(t, `SELECT count(*) FROM p_1.application_versions WHERE application_id = $1`, mustAtoi(t, appID)); n != before {
		t.Errorf("versions = %d after refused writes, want the unchanged %d", n, before)
	}
	if n := f.count(t, `SELECT count(*) FROM p_1.application_versions WHERE id = $1 AND name = 'renamed'`, mustAtoi(t, versionID)); n != 0 {
		t.Error("a refused update still applied its other fields")
	}
	recorder, _ = do(t, f.router, http.MethodPut, "/version/prompt_lib/1/"+appID+"/"+versionID, map[string]any{"instructions": atNodes})
	if recorder.Code != http.StatusOK && recorder.Code != http.StatusCreated {
		t.Fatalf("update at the budget: %d %s", recorder.Code, truncateForLog(recorder.Body.String()))
	}
}
