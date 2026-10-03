package eliteacore_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
)

// TestApplicationRelationRefusesNonNumericIDs checks the decode guard of the
// relation body. Numbers and numeric strings pass (the Postgres integration
// test proves the string form end to end); every other shape answers 400
// before the handler opens a transaction, so a nil pool is enough here.
func TestApplicationRelationRefusesNonNumericIDs(t *testing.T) {
	router := chi.NewRouter()
	router.Patch("/elitea_core/application_relation/prompt_lib/{projectID}/{appID}/{versionID}",
		eliteacore.NewHandler(nil).UpdateApplicationRelation)

	for name, body := range map[string]string{
		"word application id":    `{"application_id":"abc","version_id":7,"has_relation":true}`,
		"word version id":        `{"application_id":7,"version_id":"seven","has_relation":true}`,
		"float string":           `{"application_id":"1.5","version_id":7,"has_relation":true}`,
		"zero":                   `{"application_id":0,"version_id":7,"has_relation":true}`,
		"negative string":        `{"application_id":"-3","version_id":7,"has_relation":true}`,
		"object":                 `{"application_id":{"id":1},"version_id":7,"has_relation":true}`,
		"missing application id": `{"version_id":7,"has_relation":true}`,
		"null version id":        `{"application_id":7,"version_id":null,"has_relation":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPatch,
				"/elitea_core/application_relation/prompt_lib/1/2/3", strings.NewReader(body))
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", recorder.Code, recorder.Body.String())
			}
		})
	}
}
