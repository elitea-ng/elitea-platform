package configurations

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
)

// The `pgvector` configuration type is retired: a create is refused with a
// 400 that says so, before anything is stored. As an unknown type it would
// otherwise be written with an empty section.
func TestCreateRefusesTheRetiredPgvectorType(t *testing.T) {
	recorder := createWithPinnedCatalog(t, map[string]any{
		"elitea_title": "autotest_pgvector",
		"label":        "autotest pgvector",
		"type":         "pgvector",
		"data":         map[string]any{"connection_string": "postgresql://u:p@db/x"},
	})
	assertRetiredTypeRefusal(t, recorder)
}

// An update that changes a row TO the retired type is refused before the
// store is read (this handler has no pool: a body that got past the check
// would answer 503).
func TestUpdateRefusesAChangeToTheRetiredPgvectorType(t *testing.T) {
	catalog, err := configurationapp.LoadPinnedCurrentAvailableCatalog()
	if err != nil {
		t.Fatal(err)
	}
	handler := &Handler{catalog: catalog}
	raw, _ := json.Marshal(map[string]any{"type": "pgvector", "data": map[string]any{}})
	request := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(string(raw)))
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("projectID", "7")
	routeContext.URLParams.Add("configID", "3")
	request = request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, routeContext))
	recorder := httptest.NewRecorder()
	handler.Update(recorder, request)
	assertRetiredTypeRefusal(t, recorder)
}

func assertRetiredTypeRefusal(t *testing.T, recorder *httptest.ResponseRecorder) {
	t.Helper()
	var body struct {
		Error string `json:"error"`
		Field string `json:"field"`
	}
	if recorder.Code != http.StatusBadRequest || json.Unmarshal(recorder.Body.Bytes(), &body) != nil {
		t.Fatalf("status = %d, want 400; body = %s", recorder.Code, recorder.Body.String())
	}
	if body.Error != configurationapp.RetiredPgvectorConfigurationMessage || body.Field != "type" {
		t.Fatalf("body = %+v, want the retired-type refusal on field type", body)
	}
}
