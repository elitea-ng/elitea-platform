package applications

import (
	"net/http"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/citations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// checkCitations refuses a version save whose meta.citations is not "off",
// "required" or null, with 400 (ADR-0043 decision 5, #1500). It answers
// false when it wrote the error. The rule is citations.Check, which the
// import, the fork and the internal MCP version tools apply too.
func checkCitations(w http.ResponseWriter, body map[string]any) bool {
	meta, _ := body["meta"].(map[string]any)
	if err := citations.Check(meta); err != nil {
		apierr.Write(w, apierr.BadRequest(err.Error()))
		return false
	}
	return true
}
