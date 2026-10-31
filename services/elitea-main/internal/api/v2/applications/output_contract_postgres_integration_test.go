package applications_test

// ADR-0043 decision 2 (#1500): a pipeline's output contracts belong on its
// LLM nodes. A version save that puts one in a pipeline's meta is refused
// with 422 OUTPUT_CONTRACT_INVALID (pipeline_contract_on_node), also when
// the body names no agent_type and the type comes from the stored version.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func metaContractBody(name string) map[string]any {
	return map[string]any{
		"name": name,
		"meta": map[string]any{"output_contract": map[string]any{"type": "format", "format": "json"}},
	}
}

func requirePipelineContractRefusal(t *testing.T, what string, recorder *httptest.ResponseRecorder, body map[string]any) {
	t.Helper()
	if code := recorder.Code; code != http.StatusUnprocessableEntity {
		t.Fatalf("%s: status = %d, want 422; body=%v", what, code, body)
	}
	if body["code"] != "OUTPUT_CONTRACT_INVALID" || body["reason"] != "pipeline_contract_on_node" {
		t.Fatalf("%s: body = %v", what, body)
	}
}

func TestHandlerPostgres_PipelineMetaContractIsRefused(t *testing.T) {
	f := newLimitsFixture(t)
	appID, versionID := f.createPipeline(t, "contract-pipeline", "pipeline")

	// A new version whose body names no agent_type: the stored base version
	// says pipeline.
	recorder, body := do(t, f.router, http.MethodPost, "/versions/prompt_lib/1/"+appID, metaContractBody("v2"))
	requirePipelineContractRefusal(t, "create version", recorder, body)

	// An update of the stored pipeline version, again without agent_type.
	recorder, body = do(t, f.router, http.MethodPut, "/version/prompt_lib/1/"+appID+"/"+versionID, metaContractBody("base"))
	requirePipelineContractRefusal(t, "update version", recorder, body)

	if n := f.count(t, `SELECT COUNT(*) FROM p_1.application_versions WHERE application_id = $1`, mustAtoi(t, appID)); n != 1 {
		t.Fatalf("versions = %d, want 1: the refused create wrote a row", n)
	}

	// The same contract on an agent application is accepted.
	created := map[string]any{
		"name": "contract-agent", "description": "agent", "type": "interface",
		"versions": []any{map[string]any{"name": "base", "agent_type": "agent", "instructions": "Answer in JSON.",
			"meta": map[string]any{"step_limit": float64(25)}}},
	}
	recorder, agent := do(t, f.router, http.MethodPost, "/applications/prompt_lib/1", created)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("seed agent: %d %v", recorder.Code, agent)
	}
	recorder, body = do(t, f.router, http.MethodPost, "/versions/prompt_lib/1/"+agent["id"].(string), metaContractBody("v2"))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("agent version with a contract: %d %v", recorder.Code, body)
	}
}
