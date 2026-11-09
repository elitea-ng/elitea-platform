package configurations

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
)

// A cloud browser engine the ADR-0049 bake-off did not admit (`lite`) is a
// 400 whose message names the appendix.
func TestCurrentConfigurationMutationEngineNotAdmittedMessage(t *testing.T) {
	t.Parallel()
	recorder := httptest.NewRecorder()
	writeCurrentConfigurationMutationServiceError(recorder, &configurationapp.CurrentConfigurationMutationError{
		Code: configurationapp.CurrentConfigurationMutationEngineNotAdmitted, Field: "data.engine",
	})
	body := recorder.Body.String()
	if recorder.Code != http.StatusBadRequest || !strings.Contains(body, "ADR-0049 engine bake-off (Appendix A)") ||
		!strings.Contains(body, `"field":"data.engine"`) {
		t.Fatalf("status=%d body=%s", recorder.Code, body)
	}
}
