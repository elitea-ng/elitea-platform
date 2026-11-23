package admin

// `GET /admin/plugin_config_suggestions/administration/{key}`.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

type toolkitRegistryStub struct {
	types map[string][]string
}

// Nil-receiver safe, mirroring *runtimecomposition.CurrentToolkitSchemaSnapshot.
// That contract is what makes the typed-nil case below a 501 rather than a
// panic: the handler's guard turns an EMPTY answer into "unavailable", and it can
// only do that if the nil source answers rather than crashes.
func (stub *toolkitRegistryStub) ToolkitTypes() []string {
	if stub == nil {
		return nil
	}
	names := make([]string, 0, len(stub.types))
	for name := range stub.types {
		names = append(names, name)
	}
	return names
}

func (stub *toolkitRegistryStub) ToolkitToolNames(toolkitType string) ([]string, bool) {
	if stub == nil {
		return nil, false
	}
	tools, found := stub.types[toolkitType]
	return tools, found
}

func suggestionsRouter(handler *Handler) *chi.Mux {
	router := chi.NewRouter()
	router.Get("/admin/plugin_config_suggestions/{mode}/{key}", handler.PluginConfigSuggestions)
	return router
}

func suggestionsGet(t *testing.T, handler *Handler, path string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request = request.WithContext(auth.ContextWithUser(request.Context(), auth.User{ID: "5", UserID: "5"}))
	suggestionsRouter(handler).ServeHTTP(recorder, request)
	body := map[string]any{}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not an object: %s", recorder.Body.String())
	}
	return recorder, body
}

// suggestionsReader holds runtime.plugins, so it reads the guardrails
// section, which declares both toolkit sources.
var suggestionsReader = fixedResolver{adminaccess.RuntimePlugins}

func registryHandler() *Handler {
	return &Handler{resolver: suggestionsReader, suggestions: &toolkitRegistryStub{types: map[string][]string{
		"github":     {"get_issue", "create_issue"},
		"sharepoint": {"search", "read"},
	}}}
}

// TestSuggestionsAnswerTheWrappedShape is the correction the A14 note records:
// the route used to answer a BARE ARRAY, where every client reads `data.values`
// and `data.labels`, so the field asking for suggestions got `undefined`.
func TestSuggestionsAnswerTheWrappedShape(t *testing.T) {
	recorder, body := suggestionsGet(t, registryHandler(),
		"/admin/plugin_config_suggestions/administration/toolkit_names")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	values, ok := body["values"].([]any)
	if !ok {
		t.Fatalf("values is not an array: %v", body)
	}
	if len(values) != 2 || values[0] != "github" || values[1] != "sharepoint" {
		t.Fatalf("values=%v, want the type names sorted", values)
	}
	if _, present := body["labels"]; !present {
		t.Fatalf("labels is absent: %v", body)
	}
}

func TestToolkitToolSuggestionsNeedTheirToolkit(t *testing.T) {
	recorder, _ := suggestionsGet(t, registryHandler(),
		"/admin/plugin_config_suggestions/administration/toolkit_tools")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", recorder.Code)
	}

	recorder, body := suggestionsGet(t, registryHandler(),
		"/admin/plugin_config_suggestions/administration/toolkit_tools?toolkit=github")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d", recorder.Code)
	}
	values, _ := body["values"].([]any)
	if len(values) != 2 || values[0] != "create_issue" || values[1] != "get_issue" {
		t.Fatalf("values=%v, want the tool names sorted", values)
	}
}

// TestAnUnknownToolkitAnswersAnEmptyListNotAnError: `blocked_tools` legitimately
// names types this snapshot does not declare — the elitea_core-native ones, and
// anything a newer SDK adds — and a form that errored on those would refuse to
// render a row the operator had already saved.
func TestAnUnknownToolkitAnswersAnEmptyListNotAnError(t *testing.T) {
	recorder, body := suggestionsGet(t, registryHandler(),
		"/admin/plugin_config_suggestions/administration/toolkit_tools?toolkit=not_a_toolkit")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d", recorder.Code)
	}
	values, ok := body["values"].([]any)
	if !ok || len(values) != 0 {
		t.Fatalf("values=%v, want an empty array", body["values"])
	}
	if strings.Contains(recorder.Body.String(), "null") {
		t.Fatalf("values must encode as [] rather than null: %s", recorder.Body.String())
	}
}

// TestAnEmptyRegistryReportsUnavailableRatherThanEmpty covers BOTH the unwired
// handler and the typed nil — a non-nil interface holding a nil pointer, whose
// nil-receiver methods answer empty. Zero toolkit types is not a state this
// platform can be in, so an empty answer means the source is broken, and an
// empty picker would tell the operator their platform has no toolkits.
func TestAnEmptyRegistryReportsUnavailableRatherThanEmpty(t *testing.T) {
	for name, handler := range map[string]*Handler{
		"unwired":   {resolver: suggestionsReader},
		"typed nil": {resolver: suggestionsReader, suggestions: (*toolkitRegistryStub)(nil)},
		"empty":     {resolver: suggestionsReader, suggestions: &toolkitRegistryStub{}},
	} {
		t.Run(name, func(t *testing.T) {
			for _, source := range []string{"toolkit_names", "toolkit_tools?toolkit=github"} {
				recorder, body := suggestionsGet(t, handler,
					"/admin/plugin_config_suggestions/administration/"+source)
				if recorder.Code != http.StatusNotImplemented {
					t.Fatalf("%s: status=%d body=%s", source, recorder.Code, recorder.Body.String())
				}
				if _, present := body["error"]; !present {
					t.Fatalf("%s: no reason given: %v", source, body)
				}
			}
		})
	}
}

func TestAnUnknownSuggestionSourceIsRefused(t *testing.T) {
	// 400, not an empty list: an unknown source is a caller bug, and answering
	// "no suggestions" would hide a typo in a field's schema.
	recorder, _ := suggestionsGet(t, registryHandler(),
		"/admin/plugin_config_suggestions/administration/not_a_source")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", recorder.Code)
	}
}
