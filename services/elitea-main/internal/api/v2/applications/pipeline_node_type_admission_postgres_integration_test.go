//go:build !graph_extensions_rehearsal

package applications_test

// Save-time node-type admission (internal/domain/pipelinelimits.Check): the
// create path stored a graph with a `type: split_out` node that a production
// Worker refuses, and only reopening the editor showed "The runtime would
// refuse this graph". Every write of pipeline `instructions` now refuses a node
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

const splitOutPipeline = "entry_point: split\nstate:\n  records: list\n  expanded: list\nnodes:\n" +
	"  - id: split\n    type: split_out\n    source: records\n    split: {mode: list}\n" +
	"    destination: item\n    output: [expanded]\n    transition: END\n"

func requireNodeTypeRefusal(t *testing.T, what string, code int, body string) {
	t.Helper()
	if code != http.StatusBadRequest {
		t.Fatalf("%s: status = %d, want 400; body=%s", what, code, truncateForLog(body))
	}
	for _, want := range []string{`Node \"split\"`, `\"split_out\" node type`, "not available on this deployment", "before saving"} {
		if !strings.Contains(body, want) {
			t.Errorf("%s: body %s does not contain %s", what, truncateForLog(body), want)
		}
	}
}

func TestHandlerPostgres_NodeTypeAdmissionOnCreate(t *testing.T) {
	f := newLimitsFixture(t)
	recorder, _ := do(t, f.router, http.MethodPost, "/applications/prompt_lib/1",
		pipelineCreateBody("shaping", "pipeline", splitOutPipeline))
	requireNodeTypeRefusal(t, "create", recorder.Code, recorder.Body.String())
	if n := f.count(t, `SELECT count(*) FROM p_1.applications WHERE name = 'shaping'`); n != 0 {
		t.Errorf("a refused create left %d application rows", n)
	}
	if n := f.count(t, `SELECT count(*) FROM p_1.application_versions WHERE instructions LIKE '%split_out%'`); n != 0 {
		t.Errorf("a refused create left %d version rows", n)
	}
	// The same text is prose to an agent and is stored as written.
	if recorder, _ = do(t, f.router, http.MethodPost, "/applications/prompt_lib/1",
		pipelineCreateBody("agent-prose", "openai", splitOutPipeline)); recorder.Code != http.StatusCreated {
		t.Errorf("agent create: %d %s", recorder.Code, truncateForLog(recorder.Body.String()))
	}
}

func TestHandlerPostgres_NodeTypeAdmissionOnCreateVersionAndUpdate(t *testing.T) {
	f := newLimitsFixture(t)
	appID, versionID := f.createPipeline(t, "nt", "pipeline")

	recorder, _ := do(t, f.router, http.MethodPost, "/versions/prompt_lib/1/"+appID,
		map[string]any{"name": "v-shaping", "agent_type": "pipeline", "instructions": splitOutPipeline})
	requireNodeTypeRefusal(t, "create version", recorder.Code, recorder.Body.String())

	path := "/version/prompt_lib/1/" + appID + "/" + versionID
	for name, body := range map[string]map[string]any{
		"stored type":           {"instructions": splitOutPipeline},
		"body type":             {"agent_type": "pipeline", "instructions": splitOutPipeline},
		"with a rename":         {"name": "renamed", "instructions": splitOutPipeline},
		"aggregate after split": {"instructions": strings.Replace(splitOutPipeline, "transition: END", "transition: END\n  - id: tail\n    type: aggregate", 1)},
	} {
		recorder, _ = do(t, f.router, http.MethodPut, path, body)
		requireNodeTypeRefusal(t, "update "+name, recorder.Code, recorder.Body.String())
	}
	if n := f.count(t, `SELECT count(*) FROM p_1.application_versions WHERE application_id = $1 AND (name = 'renamed' OR name = 'v-shaping' OR instructions LIKE '%split_out%')`, mustAtoi(t, appID)); n != 0 {
		t.Errorf("a refused save still wrote %d rows/fields", n)
	}

	recorder, _ = do(t, f.router, http.MethodPut, "/application/prompt_lib/1/"+appID, map[string]any{
		"name":    "nt-renamed",
		"version": map[string]any{"application_id": appID, "id": versionID, "instructions": splitOutPipeline},
	})
	requireNodeTypeRefusal(t, "application update", recorder.Code, recorder.Body.String())
	if n := f.count(t, `SELECT count(*) FROM p_1.applications WHERE name = 'nt-renamed'`); n != 0 {
		t.Error("a refused application update still renamed the application")
	}
}

// Turning an agent whose stored text declares a split_out node into a pipeline
// is refused the same way, so a type change cannot store such a graph.
func TestHandlerPostgres_NodeTypeAdmissionOnAgentTypeChange(t *testing.T) {
	f := newLimitsFixture(t)
	recorder, created := do(t, f.router, http.MethodPost, "/applications/prompt_lib/1",
		pipelineCreateBody("flip-shaping", "openai", splitOutPipeline))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("seed agent: %d %s", recorder.Code, truncateForLog(recorder.Body.String()))
	}
	path := "/version/prompt_lib/1/" + created["id"].(string) + "/" + created["version_details"].(map[string]any)["id"].(string)
	recorder, _ = do(t, f.router, http.MethodPut, path, map[string]any{"agent_type": "pipeline"})
	requireNodeTypeRefusal(t, "type change", recorder.Code, recorder.Body.String())
	if n := f.count(t, `SELECT count(*) FROM p_1.application_versions WHERE agent_type = 'pipeline' AND instructions LIKE '%split_out%'`); n != 0 {
		t.Errorf("a refused type change stored %d split_out pipelines", n)
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
	if _, err := f.pool.Exec(ctx, `UPDATE p_1.application_versions SET instructions = $1 WHERE id = $2`, splitOutPipeline, mustAtoi(t, versionID)); err != nil {
		t.Fatalf("seed stored split_out version: %v", err)
	}
	path := "/version/prompt_lib/1/" + appID + "/" + versionID
	if recorder, _ := do(t, f.router, http.MethodGet, path, nil); recorder.Code != http.StatusOK {
		t.Errorf("get: %d %s", recorder.Code, truncateForLog(recorder.Body.String()))
	}
	if recorder, _ := do(t, f.router, http.MethodPut, path, map[string]any{"name": "renamed"}); recorder.Code != http.StatusCreated {
		t.Errorf("a save without instructions was refused: %d %s", recorder.Code, truncateForLog(recorder.Body.String()))
	}
	fixed := strings.Replace(splitOutPipeline, "type: split_out", "type: state_modifier", 1)
	if recorder, _ := do(t, f.router, http.MethodPut, path, map[string]any{"instructions": fixed}); recorder.Code != http.StatusCreated {
		t.Errorf("replacing the node was refused: %d %s", recorder.Code, truncateForLog(recorder.Body.String()))
	}
}
