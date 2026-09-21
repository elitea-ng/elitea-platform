package toolkits

import (
	"log/slog"
	"net/http"

	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
)

// applyCatalogueSettingsDefaults fills the defaults the SDK catalogue declares
// for the settings keys a write left out, in place, before the body is
// persisted (#978).
//
// WHY IT IS ON THE WRITE PATH AND NOT AT READ TIME. The value has to be in the
// stored row, because the row is what the claim-time materializer projects to
// the worker, and the worker's model refuses the `null` an absent key becomes:
// qTest's `no_of_tests_shown_in_dql_search` is `int` with `default: 10`, and a
// toolkit saved without it died at materialization with
// `Input should be a valid integer`, on the first turn that used it, with no
// field to point at. Defaulting at read time would leave every existing row
// dependent on the reader applying it, which is the shape that let the two
// disagree in the first place — the toolkit FORM renders the defaults and
// therefore sends them, while the API, the MCP surface and an import do not.
//
// A nil catalogue (the eight hand-written native types, and any deployment
// that composes no catalogue source) fills nothing, which is exactly the
// behaviour those types had before: their schemas are declared in this package
// and carry no SDK defaults.
//
// It never refuses a request. A catalogue read that fails is logged and
// skipped: a save that used to work must not start failing because the
// snapshot could not be consulted.
func (h *Handler) applyCatalogueSettingsDefaults(
	r *http.Request,
	action string,
	toolkitType string,
	settings map[string]any,
) {
	if h == nil || settings == nil || toolkitType == "" {
		return
	}
	schema, _, found, err := h.toolkitCatalogueEntry(toolkitType)
	if err != nil {
		slog.WarnContext(r.Context(), "toolkit settings defaults skipped",
			"action", action, "type", toolkitType, "error", err)
		return
	}
	if !found {
		return
	}
	if added := configurationapp.ApplyToolkitSettingsDefaults(schema, settings); added > 0 {
		slog.DebugContext(r.Context(), "toolkit settings defaults applied",
			"action", action, "type", toolkitType, "keys", added)
	}
}
