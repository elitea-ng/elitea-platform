package drafts

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/santhosh-tekuri/jsonschema/v6"

	specfiles "github.com/elitea-ng/elitea-platform/services/elitea-main/api/openapi"
)

// TestTheSchemaRouteServesTheContractComponents: the served schemas ARE the
// v2.yaml components (decoded through a second, independent parser), so the
// route and the contract cannot disagree and no fixture is needed.
func TestTheSchemaRouteServesTheContractComponents(t *testing.T) {
	recorder := httptest.NewRecorder()
	NewHandler(nil).GetAgentSchema(recorder, httptest.NewRequest(http.MethodGet, "/agent_schema/prompt_lib/7", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var served AgentSchemas
	if err := json.Unmarshal(recorder.Body.Bytes(), &served); err != nil {
		t.Fatal(err)
	}
	if len(served.SchemaVersion) != 12 || recorder.Header().Get("ETag") != `"`+served.SchemaVersion+`"` {
		t.Fatalf("schema_version = %q etag = %q", served.SchemaVersion, recorder.Header().Get("ETag"))
	}

	doc, err := openapi3.NewLoader().LoadFromData(specfiles.SpecYAML)
	if err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string]json.RawMessage{
		agentDefinitionComponent:      served.AgentDefinition,
		localAgentDefinitionComponent: served.LocalAgentDefinition,
	} {
		ref := doc.Components.Schemas[name]
		if ref == nil || ref.Value == nil {
			t.Fatalf("v2.yaml has no %s component", name)
		}
		want, err := json.Marshal(ref.Value)
		if err != nil {
			t.Fatal(err)
		}
		var wantValue, gotValue any
		_ = json.Unmarshal(want, &wantValue)
		_ = json.Unmarshal(raw, &gotValue)
		wantJSON, _ := json.Marshal(wantValue)
		gotJSON, _ := json.Marshal(gotValue)
		if !bytes.Equal(wantJSON, gotJSON) {
			t.Errorf("%s served differs from v2.yaml:\n served %s\n  spec  %s", name, gotJSON, wantJSON)
		}
	}
}

// TestEachComponentExampleSatisfiesItsOwnSchema compiles both served schemas
// as JSON Schema and validates the component's own example, so a schema that
// is valid OpenAPI but not usable JSON Schema fails here.
func TestEachComponentExampleSatisfiesItsOwnSchema(t *testing.T) {
	compiled, err := loadSchemas()
	if err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string]json.RawMessage{
		agentDefinitionComponent:      compiled.body.AgentDefinition,
		localAgentDefinitionComponent: compiled.body.LocalAgentDefinition,
	} {
		document, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		compiler := jsonschema.NewCompiler()
		if err := compiler.AddResource("test:"+name, document); err != nil {
			t.Fatal(err)
		}
		schema, err := compiler.Compile("test:" + name)
		if err != nil {
			t.Fatalf("%s does not compile as JSON Schema: %v", name, err)
		}
		example := document.(map[string]any)["example"]
		if example == nil {
			t.Fatalf("%s has no example; the generated client mocks read it", name)
		}
		if err := schema.Validate(example); err != nil {
			t.Errorf("%s example does not satisfy the schema: %v", name, err)
		}
	}
}

func TestTheLocalScopePatternRefusesEscapes(t *testing.T) {
	compiled, err := loadSchemas()
	if err != nil {
		t.Fatal(err)
	}
	document, _ := jsonschema.UnmarshalJSON(bytes.NewReader(compiled.body.LocalAgentDefinition))
	compiler := jsonschema.NewCompiler()
	_ = compiler.AddResource("test:local", document)
	schema, err := compiler.Compile("test:local")
	if err != nil {
		t.Fatal(err)
	}
	for scope, ok := range map[string]bool{
		"root": true, "apps/web": true, "src": true,
		"/etc": false, "../up": false, "a/../b": false,
		"": false, "a//b": false,
	} {
		err := schema.Validate(map[string]any{"name": "n", "instructions": "i", "scope": scope})
		if (err == nil) != ok {
			t.Errorf("scope %q: valid = %v, want %v (%v)", scope, err == nil, ok, err)
		}
	}
}

func TestValidateNamesTheMissingFieldsWithoutTheirValues(t *testing.T) {
	err := ValidateApplicationDraft(ApplicationDraft{Description: "secret description text", Instructions: "x"})
	if err == nil {
		t.Fatal("a draft without a name passed")
	}
	if !strings.Contains(err.Error(), "name") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("err = %v, want the field named and the value not echoed", err)
	}
	long := ApplicationDraft{Name: "n", Description: "d", Instructions: "i",
		ConversationStarters: []string{"a", "b", "c", "d", "e"}}
	if err := ValidateApplicationDraft(long); err == nil || !strings.Contains(err.Error(), "conversation_starters") {
		t.Fatalf("err = %v, want five starters refused", err)
	}
}

func TestTheSchemaRouteAnswers304ForAMatchingETag(t *testing.T) {
	first := httptest.NewRecorder()
	NewHandler(nil).GetAgentSchema(first, httptest.NewRequest(http.MethodGet, "/agent_schema/prompt_lib/7", nil))
	etag := first.Header().Get("ETag")
	for _, header := range []string{etag, "W/" + etag, `"other", ` + etag, "*"} {
		request := httptest.NewRequest(http.MethodGet, "/agent_schema/prompt_lib/7", nil)
		request.Header.Set("If-None-Match", header)
		recorder := httptest.NewRecorder()
		NewHandler(nil).GetAgentSchema(recorder, request)
		if recorder.Code != http.StatusNotModified || recorder.Body.Len() != 0 || recorder.Header().Get("ETag") != etag {
			t.Errorf("If-None-Match %q: status = %d body = %d bytes", header, recorder.Code, recorder.Body.Len())
		}
	}
	request := httptest.NewRequest(http.MethodGet, "/agent_schema/prompt_lib/7", nil)
	request.Header.Set("If-None-Match", `"stale"`)
	recorder := httptest.NewRecorder()
	NewHandler(nil).GetAgentSchema(recorder, request)
	if recorder.Code != http.StatusOK || recorder.Body.Len() == 0 {
		t.Errorf("a stale tag: status = %d, want 200 with the schemas", recorder.Code)
	}
}
