package configurations

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

func TestApplyLLMModelDescriptionRule(t *testing.T) {
	cases := []struct {
		name     string
		typeName string
		data     map[string]any
		ok       bool
		want     any
		present  bool
	}{
		{"40 characters", "llm_model", map[string]any{"description": strings.Repeat("a", 40)}, true, strings.Repeat("a", 40), true},
		{"41 characters", "llm_model", map[string]any{"description": strings.Repeat("a", 41)}, false, nil, true},
		{"trimmed", "llm_model", map[string]any{"description": "  Fast for everyday tasks  "}, true, "Fast for everyday tasks", true},
		{"spaces only", "llm_model", map[string]any{"description": "   "}, true, nil, false},
		{"not text", "llm_model", map[string]any{"description": 7}, false, nil, true},
		{"other type keeps any value", "embedding_model", map[string]any{"description": strings.Repeat("a", 90)}, true, strings.Repeat("a", 90), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ok := applyLLMModelDescriptionRule(recorder, tc.typeName, tc.data)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if !ok {
				var body map[string]any
				_ = json.Unmarshal(recorder.Body.Bytes(), &body)
				if recorder.Code != http.StatusBadRequest || body["field"] != "description" ||
					!strings.Contains(body["error"].(string), "description") {
					t.Fatalf("refusal = %d %v, want a 400 naming the description field", recorder.Code, body)
				}
				return
			}
			value, present := tc.data["description"]
			if present != tc.present || (present && value != tc.want) {
				t.Fatalf("description = %v (present %v), want %v (present %v)", value, present, tc.want, tc.present)
			}
		})
	}
}

// The compatibility create refuses a long description before anything is
// stored. The handler has no pool here, so a 400 can only come from the rule.
func TestCreateRefusesALongModelDescription(t *testing.T) {
	handler := NewHandler(nil)
	body := `{"type":"llm_model","elitea_title":"m","label":"M","data":{"name":"gpt","ai_credentials":{"elitea_title":"c","private":false},` +
		`"description":"` + strings.Repeat("x", 41) + `"}}`
	request := httptest.NewRequest(http.MethodPost, "/configurations/7", strings.NewReader(body))
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("projectID", "7")
	request = request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, routeContext))
	recorder := httptest.NewRecorder()
	handler.Create(recorder, request)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "description") {
		t.Fatalf("status %d body %s, want a 400 naming the description", recorder.Code, recorder.Body.String())
	}
}

func TestTheLLMModelSchemaAdvertisesTheDescriptionAndTheTest(t *testing.T) {
	handler := NewHandler(nil)
	entry, ok := handler.catalog.EntryByType("llm_model")
	if !ok || !entry.HasTestConnection {
		t.Fatalf("llm_model entry = %v / %v, want has_test_connection", ok, entry.HasTestConnection)
	}
	schema, ok := handler.catalog.DataSchemaByType("llm_model")
	if !ok {
		t.Fatal("no llm_model data schema")
	}
	field, ok := schema["properties"].(map[string]any)["description"].(map[string]any)
	if !ok {
		t.Fatal("the llm_model schema has no description field")
	}
	encoded, _ := json.Marshal(field)
	if !strings.Contains(string(encoded), `"maxLength":40`) {
		t.Fatalf("description field = %s, want maxLength 40", encoded)
	}
	for _, other := range []string{"embedding_model", "image_generation_model", "asr_model", "tts_model"} {
		otherSchema, _ := handler.catalog.DataSchemaByType(other)
		if _, has := otherSchema["properties"].(map[string]any)["description"]; has {
			t.Fatalf("%s must not get the description field", other)
		}
	}
}
