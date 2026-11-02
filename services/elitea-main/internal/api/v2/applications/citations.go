package applications

import (
	"net/http"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// citationsMetaKey is the agent version meta key of the citation setting
// (ADR-0043 decision 5, #1500). "required" makes the worker ask the model to
// cite its sources and report a grounding indicator on the final answer;
// "off", null or absent turn it off. Pipelines and nested agents ignore it.
const citationsMetaKey = "citations"

// checkCitations refuses a version save whose meta.citations is not "off",
// "required" or null, with 400. It answers false when it wrote the error.
func checkCitations(w http.ResponseWriter, body map[string]any) bool {
	meta, _ := body["meta"].(map[string]any)
	raw, present := meta[citationsMetaKey]
	if !present || raw == nil {
		return true
	}
	if mode, ok := raw.(string); ok && (mode == "off" || mode == "required") {
		return true
	}
	apierr.Write(w, apierr.BadRequest(`meta.citations must be "off", "required" or null`))
	return false
}
