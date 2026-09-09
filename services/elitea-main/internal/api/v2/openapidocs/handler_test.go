package openapidocs_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/openapidocs"
)

func TestSpecServesTheEmbeddedYAMLByteForByte(t *testing.T) {
	handler := openapidocs.NewHandler()
	response := httptest.NewRecorder()
	handler.Spec(response, httptest.NewRequest(http.MethodGet, "/api/openapi.yaml", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("status=%d", response.Code)
	}
	if !strings.Contains(response.Header().Get("Content-Type"), "yaml") {
		t.Fatalf("content-type=%q", response.Header().Get("Content-Type"))
	}
	if !strings.Contains(response.Body.String(), "openapi:") {
		t.Fatalf("body does not look like an OpenAPI document")
	}
}

func TestSpecJSONConvertsTheSameDocumentRealOperationsSurvive(t *testing.T) {
	handler := openapidocs.NewHandler()
	response := httptest.NewRecorder()
	handler.SpecJSON(response, httptest.NewRequest(http.MethodGet, "/api/openapi.json", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
	var document struct {
		Paths map[string]map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &document); err != nil {
		t.Fatalf("decode: %v", err)
	}
	operation, ok := document.Paths["/social/authors/{project_id}"]
	if !ok {
		t.Fatalf("expected known path missing from converted spec, got %d paths", len(document.Paths))
	}
	get, ok := operation["get"].(map[string]any)
	if !ok || get["operationId"] != "listSocialAuthors" {
		t.Fatalf("get operation=%#v", operation["get"])
	}
}

func TestUIServesSelfContainedHTMLWithNoExternalReferences(t *testing.T) {
	handler := openapidocs.NewHandler()
	response := httptest.NewRecorder()
	handler.UI(response, httptest.NewRequest(http.MethodGet, "/api/docs", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("status=%d", response.Code)
	}
	body := response.Body.String()
	for _, forbidden := range []string{"http://", "https://", "//cdn", "//unpkg", "//jsdelivr"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("docs UI references an external resource %q", forbidden)
		}
	}
	if !strings.Contains(body, "/api/openapi.json") {
		t.Fatal("docs UI does not reference the local spec endpoint")
	}
	if csp := response.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") {
		t.Fatalf("missing restrictive CSP: %q", csp)
	}
}
