package eliteacore

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	specfiles "github.com/EliteaAI/elitea-platform/services/elitea-main/api/openapi"
)

// TestListPublicApplicationsSpecAdmitsTheQueryRefusal: contract 1.3 locks
// listPublicApplications, whose 400 the version gate also answers. The
// handler's own 400 for a refused query parameter has a different body
// ({error, param, msg}, no error_description), so the declared 400 schema must
// admit both, or a client that checks the body against the contract fails to
// decode the parameter error it is meant to show.
func TestListPublicApplicationsSpecAdmitsTheQueryRefusal(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromData(specfiles.SpecYAML)
	if err != nil {
		t.Fatal(err)
	}
	var op *openapi3.Operation
	for _, item := range doc.Paths.Map() {
		if item.Get != nil && item.Get.OperationID == "listPublicApplications" {
			op = item.Get
		}
	}
	if op == nil {
		t.Fatal("v2.yaml has no listPublicApplications operation")
	}
	bad := op.Responses.Value("400")
	if bad == nil || bad.Value == nil || bad.Value.Content.Get("application/json") == nil {
		t.Fatal("listPublicApplications declares no JSON 400")
	}
	schema := bad.Value.Content.Get("application/json").Schema.Value

	for _, query := range []string{"sort_by=nope", "sort_order=sideways", "limit=-1", "offset=x"} {
		values, _ := url.ParseQuery(query)
		_, parseErr := parsePublicApplicationsFilter(values)
		if parseErr == nil {
			t.Fatalf("%s was not refused", query)
		}
		recorder := httptest.NewRecorder()
		writePublicApplicationsQueryError(recorder, parseErr)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s status=%d", query, recorder.Code)
		}
		var body any
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if err := schema.VisitJSON(body); err != nil {
			t.Errorf("%s: the handler's 400 %s does not match the declared 400: %v", query, recorder.Body.String(), err)
		}
	}
	gate := map[string]any{"error": "invalid_client_version", "error_description": "X-Client-Version does not parse"}
	if err := schema.VisitJSON(gate); err != nil {
		t.Errorf("the version gate's 400 does not match the declared 400: %v", err)
	}
}
