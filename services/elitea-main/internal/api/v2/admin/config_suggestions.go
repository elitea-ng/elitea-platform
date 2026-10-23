package admin

// `GET /admin/plugin_config_suggestions/administration/{key}` — the choices the
// Configuration form offers for a field that declares an `enum_source`.
//
// # Why this stopped being a 501
//
// It answered "configuration value suggestions are sourced from the legacy
// toolkit registry, which has no equivalent in this service". That was true of
// the mechanism and false of the claim underneath it. The legacy runtime reads
// `elitea_core.toolkit_schemas` — an in-process dict a plugin populates at load —
// and this service has the same registry in a better form: the digest-pinned
// snapshot in internal/runtimecomposition, generated from the exact elitea-sdk
// revision the Python workers are admitted to run. An operator picking from it
// cannot be offered a toolkit or tool name the workers would not recognise.
//
// The 501 was load-bearing while it was true, because the reason it gave was
// also the reason the only sections declaring an `enum_source` were unavailable.
// Guardrails is now live, and a form that offers three free-text boxes where the
// reference offers pickers is a worse port than one that answers this route.
//
// # The response shape
//
// `{"values": [...], "labels": {...}}`. Before A14 this route answered a BARE
// ARRAY, so `admin_ui`'s `SchemaField.jsx` — which reads `data.values` and
// `data.labels` — got `undefined` rather than an empty list. The wrapper is the
// contract; `labels` is present and empty because the identifiers ARE the
// display text for these two sources, and inventing prettified labels would
// print something the operator cannot type back into a config file.

import (
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
)

// ToolkitRegistrySource enumerates the built-in toolkit types and their tools.
//
// An interface here, implemented in internal/runtimecomposition and injected by
// the composition root, for the reason internal/api/v2/toolkits declares
// `ToolkitArgumentSchemaSource` the same way: the snapshot is owned by the
// package that imports this layer, so the dependency edge has to run this way
// round.
type ToolkitRegistrySource interface {
	ToolkitTypes() []string
	ToolkitToolNames(toolkitType string) ([]string, bool)
}

// maxSuggestionValues bounds a response. Both sources are read out of a pinned
// file with 52 types and well under a hundred tools apiece, so this cannot fire
// today; it is here so that a future snapshot cannot turn one form field into an
// unbounded response.
const maxSuggestionValues = 4096

// suggestionsUnavailable is what an unwired registry answers.
//
// Not an empty list. "This deployment cannot enumerate toolkit types" and "this
// deployment has no toolkit types" render identically as `[]`, and the second is
// never true — a form showing an empty picker tells the operator their platform
// has no toolkits, which would be alarming and wrong.
const suggestionsUnavailable = "this deployment cannot enumerate the toolkit registry, so no suggestions are available for this field"

// PluginConfigSuggestions serves one suggestion source.
func (h *Handler) PluginConfigSuggestions(w http.ResponseWriter, r *http.Request) {
	switch chi.URLParam(r, "key") {
	case "toolkit_names":
		h.toolkitNameSuggestions(w, r)
	case "toolkit_tools":
		h.toolkitToolSuggestions(w, r)
	case "projects":
		h.projectSuggestions(w, r)
	default:
		// 400 rather than an empty list: an unknown source is a caller bug, and
		// answering it with "no suggestions" hides a typo in a schema.
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unknown suggestion source"})
	}
}

func (h *Handler) toolkitNameSuggestions(w http.ResponseWriter, _ *http.Request) {
	types, ok := h.toolkitRegistryTypes()
	if !ok {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": suggestionsUnavailable})
		return
	}
	writeSuggestionValues(w, types)
}

// toolkitRegistryTypes reads the registry and reports whether it answered at all.
//
// The EMPTY result is treated as "did not answer", and that guards two failures
// with one rule:
//
//   - a typed nil. `WithToolkitRegistry` boxing a nil *CurrentToolkitSchemaSnapshot
//     produces a non-nil interface holding a nil pointer, so `h.suggestions == nil`
//     is false and the nil-receiver methods return empty — the shape this
//     codebase has shipped before (the gateway's /healthz nil-receiver panic).
//   - a registry that loaded but decoded nothing.
//
// Zero toolkit types is not a state this platform can be in: the pinned snapshot
// declares 52. So an empty answer means the source is broken, and saying "no
// suggestions available" is honest where an empty picker would tell the operator
// their platform has no toolkits.
func (h *Handler) toolkitRegistryTypes() ([]string, bool) {
	if h == nil || h.suggestions == nil {
		return nil, false
	}
	types := h.suggestions.ToolkitTypes()
	if len(types) == 0 {
		return nil, false
	}
	return types, true
}

func (h *Handler) toolkitToolSuggestions(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.toolkitRegistryTypes(); !ok {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": suggestionsUnavailable})
		return
	}
	toolkit := strings.TrimSpace(r.URL.Query().Get("toolkit"))
	if toolkit == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "the toolkit query parameter is required for the toolkit_tools source",
		})
		return
	}
	// A toolkit the registry does not know answers an EMPTY list at 200, not a
	// 404. `blocked_tools` legitimately names a type this snapshot does not
	// declare — the four elitea_core-native types, and anything a newer SDK
	// adds — and a form that errored on those would refuse to render a row the
	// operator had already saved.
	names, found := h.suggestions.ToolkitToolNames(toolkit)
	if !found {
		names = nil
	}
	writeSuggestionValues(w, names)
}

// projectSuggestions serves the `projects` source, which the Features page's
// publishing whitelist declares.
//
// Ordered by id rather than by name, because the value the field stores IS the
// id: the operator is choosing a number, and a list sorted by a display string
// makes an adjacent pair of ids look unrelated.
func (h *Handler) projectSuggestions(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.pool == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": suggestionsUnavailable})
		return
	}
	rows, err := h.pool.Query(r.Context(),
		`SELECT id, name FROM centry.project ORDER BY id LIMIT $1`, maxSuggestionValues)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": "could not read the project list",
		})
		return
	}
	defer rows.Close()

	values := []any{}
	labels := map[string]any{}
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"error": "could not read the project list",
			})
			return
		}
		values = append(values, id)
		labels[strconv.FormatInt(id, 10)] = name
	}
	if err := rows.Err(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": "could not read the project list",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"values": values, "labels": labels})
}

func writeSuggestionValues(w http.ResponseWriter, values []string) {
	sorted := make([]string, 0, len(values))
	sorted = append(sorted, values...)
	sort.Strings(sorted)
	if len(sorted) > maxSuggestionValues {
		sorted = sorted[:maxSuggestionValues]
	}
	// Non-nil so the field encodes as `[]`. The client reads `data.values`
	// directly, and `null` there is the shape that produced the original defect.
	if sorted == nil {
		sorted = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"values": sorted, "labels": map[string]any{}})
}
