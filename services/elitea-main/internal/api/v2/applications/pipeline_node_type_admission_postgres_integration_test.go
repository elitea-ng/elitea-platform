package applications_test

// Save-time node-type admission (internal/domain/pipelinelimits.Check): the
// create path stored a graph with a node type the Worker refuses, and only
// reopening the editor showed "The runtime would refuse this graph". Every write of pipeline `instructions` now refuses a node
// type the deployment's runtime does not run, with a 400 that names the node.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// unsupportedTypePipeline holds a node type no runtime build runs.
const unsupportedTypePipeline = "entry_point: split\nstate:\n  records: list\n  expanded: list\nnodes:\n" +
	"  - id: split\n    type: custom\n    source: records\n    split: {mode: list}\n" +
	"    destination: item\n    output: [expanded]\n    transition: END\n"

// splitOutPipeline holds the shaping node every deployment admits.
const splitOutPipeline = "entry_point: split\nstate:\n  records: list\n  expanded: list\nnodes:\n" +
	"  - id: split\n    type: split_out\n    source: records\n    split: {mode: list}\n" +
	"    destination: item\n    output: [expanded]\n    transition: END\n"

func requireNodeTypeRefusal(t *testing.T, what string, code int, body string) {
	t.Helper()
	if code != http.StatusBadRequest {
		t.Fatalf("%s: status = %d, want 400; body=%s", what, code, truncateForLog(body))
	}
	for _, want := range []string{`Node \"split\"`, `\"custom\" node type`, "does not support", "before saving"} {
		if !strings.Contains(body, want) {
			t.Errorf("%s: body %s does not contain %s", what, truncateForLog(body), want)
		}
	}
}

func TestHandlerPostgres_NodeTypeAdmissionOnCreate(t *testing.T) {
	f := newLimitsFixture(t)
	recorder, _ := do(t, f.router, http.MethodPost, "/applications/prompt_lib/1",
		pipelineCreateBody("shaping", "pipeline", unsupportedTypePipeline))
	requireNodeTypeRefusal(t, "create", recorder.Code, recorder.Body.String())
	if n := f.count(t, `SELECT count(*) FROM p_1.applications WHERE name = 'shaping'`); n != 0 {
		t.Errorf("a refused create left %d application rows", n)
	}
	if n := f.count(t, `SELECT count(*) FROM p_1.application_versions WHERE instructions LIKE '%custom%'`); n != 0 {
		t.Errorf("a refused create left %d version rows", n)
	}
	// The shaping nodes are admitted on every deployment.
	if recorder, _ = do(t, f.router, http.MethodPost, "/applications/prompt_lib/1",
		pipelineCreateBody("shaping-admitted", "pipeline", splitOutPipeline)); recorder.Code != http.StatusCreated {
		t.Errorf("split_out create: %d %s", recorder.Code, truncateForLog(recorder.Body.String()))
	}
	// The same text is prose to an agent and is stored as written.
	if recorder, _ = do(t, f.router, http.MethodPost, "/applications/prompt_lib/1",
		pipelineCreateBody("agent-prose", "openai", unsupportedTypePipeline)); recorder.Code != http.StatusCreated {
		t.Errorf("agent create: %d %s", recorder.Code, truncateForLog(recorder.Body.String()))
	}
}

func TestHandlerPostgres_NodeTypeAdmissionOnCreateVersionAndUpdate(t *testing.T) {
	f := newLimitsFixture(t)
	appID, versionID := f.createPipeline(t, "nt", "pipeline")

	recorder, _ := do(t, f.router, http.MethodPost, "/versions/prompt_lib/1/"+appID,
		map[string]any{"name": "v-shaping", "agent_type": "pipeline", "instructions": unsupportedTypePipeline})
	requireNodeTypeRefusal(t, "create version", recorder.Code, recorder.Body.String())

	path := "/version/prompt_lib/1/" + appID + "/" + versionID
	for name, body := range map[string]map[string]any{
		"stored type":                {"instructions": unsupportedTypePipeline},
		"body type":                  {"agent_type": "pipeline", "instructions": unsupportedTypePipeline},
		"with a rename":              {"name": "renamed", "instructions": unsupportedTypePipeline},
		"unsupported after admitted": {"instructions": strings.Replace(unsupportedTypePipeline, "transition: END", "transition: END\n  - id: tail\n    type: function", 1)},
	} {
		recorder, _ = do(t, f.router, http.MethodPut, path, body)
		requireNodeTypeRefusal(t, "update "+name, recorder.Code, recorder.Body.String())
	}
	if n := f.count(t, `SELECT count(*) FROM p_1.application_versions WHERE application_id = $1 AND (name = 'renamed' OR name = 'v-shaping' OR instructions LIKE '%custom%')`, mustAtoi(t, appID)); n != 0 {
		t.Errorf("a refused save still wrote %d rows/fields", n)
	}

	recorder, _ = do(t, f.router, http.MethodPut, "/application/prompt_lib/1/"+appID, map[string]any{
		"name":    "nt-renamed",
		"version": map[string]any{"application_id": appID, "id": versionID, "instructions": unsupportedTypePipeline},
	})
	requireNodeTypeRefusal(t, "application update", recorder.Code, recorder.Body.String())
	if n := f.count(t, `SELECT count(*) FROM p_1.applications WHERE name = 'nt-renamed'`); n != 0 {
		t.Error("a refused application update still renamed the application")
	}
}

// Turning an agent whose stored text declares an unsupported node into a pipeline
// is refused the same way, so a type change cannot store such a graph.
func TestHandlerPostgres_NodeTypeAdmissionOnAgentTypeChange(t *testing.T) {
	f := newLimitsFixture(t)
	recorder, created := do(t, f.router, http.MethodPost, "/applications/prompt_lib/1",
		pipelineCreateBody("flip-shaping", "openai", unsupportedTypePipeline))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("seed agent: %d %s", recorder.Code, truncateForLog(recorder.Body.String()))
	}
	path := "/version/prompt_lib/1/" + created["id"].(string) + "/" + created["version_details"].(map[string]any)["id"].(string)
	recorder, _ = do(t, f.router, http.MethodPut, path, map[string]any{"agent_type": "pipeline"})
	requireNodeTypeRefusal(t, "type change", recorder.Code, recorder.Body.String())
	if n := f.count(t, `SELECT count(*) FROM p_1.application_versions WHERE agent_type = 'pipeline' AND instructions LIKE '%custom%'`); n != 0 {
		t.Errorf("a refused type change stored %d unsupported pipelines", n)
	}
}

// A version stored before this check (local pipeline 161) keeps loading and
// stays editable in its other fields; only writing such a graph is refused, and
// replacing the node is accepted.
func TestHandlerPostgres_ExistingInadmissibleVersionStaysReadableAndFixable(t *testing.T) {
	f := newLimitsFixture(t)
	appID, versionID := f.createPipeline(t, "legacy-shaping", "pipeline")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := f.pool.Exec(ctx, `UPDATE p_1.application_versions SET instructions = $1 WHERE id = $2`, unsupportedTypePipeline, mustAtoi(t, versionID)); err != nil {
		t.Fatalf("seed stored unsupported version: %v", err)
	}
	path := "/version/prompt_lib/1/" + appID + "/" + versionID
	if recorder, _ := do(t, f.router, http.MethodGet, path, nil); recorder.Code != http.StatusOK {
		t.Errorf("get: %d %s", recorder.Code, truncateForLog(recorder.Body.String()))
	}
	if recorder, _ := do(t, f.router, http.MethodPut, path, map[string]any{"name": "renamed"}); recorder.Code != http.StatusCreated {
		t.Errorf("a save without instructions was refused: %d %s", recorder.Code, truncateForLog(recorder.Body.String()))
	}
	fixed := strings.Replace(unsupportedTypePipeline, "type: custom", "type: state_modifier", 1)
	if recorder, _ := do(t, f.router, http.MethodPut, path, map[string]any{"instructions": fixed}); recorder.Code != http.StatusCreated {
		t.Errorf("replacing the node was refused: %d %s", recorder.Code, truncateForLog(recorder.Body.String()))
	}
}
