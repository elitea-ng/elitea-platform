// Package run is the Inventory tool runner's engine-facing half: the parameter
// merge, the per-tool argument set, the deferred-tool refusals, the source
// check, result composition and artifact upload.
//
// It is the shape DeepWiki's package has (internal/apps/deepwiki/run), and
// deliberately so — ADR-0023's claim is that a second provider needs no host
// changes, and what is per-application is exactly this: which tools exist, what
// arguments they take, and how the engine's result composes. The transport
// (internal/engine), the upload (internal/artifacts) and the SPI (internal/spi)
// are shared and unchanged.
//
// The knowledge-graph engine — what the tools DO — is not here. A Runner takes
// a table of Tool functions; the engine sidecar
// (services/elitea-inventory-engine) is one such table.
package run

import (
	"reflect"
	"strings"

	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/spi"
)

// Params is a tool's merged parameter set: JSON-decoded values, so a nested
// object is a map[string]any and a list is []any.
type Params = map[string]any

// Truthy is Python's truth for a JSON-decoded value, because the legacy merge
// branches on `if value`, not on presence. nil, false, 0, "", an empty list and
// an empty object are false.
func Truthy(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case bool:
		return v
	case string:
		return v != ""
	case float64:
		return v != 0
	case int:
		return v != 0
	case int64:
		return v != 0
	case []any:
		return len(v) > 0
	case map[string]any:
		return len(v) > 0
	default:
		rv := reflect.ValueOf(value)
		switch rv.Kind() {
		case reflect.Slice, reflect.Map, reflect.Array, reflect.String:
			return rv.Len() > 0
		}
		return true
	}
}

func object(value any) map[string]any {
	m, _ := value.(map[string]any)
	return m
}

func str(value any) string {
	s, _ := value.(string)
	return s
}

func configurationParameters(request map[string]any) map[string]any {
	if p := object(object(request["configuration"])["parameters"]); p != nil {
		return p
	}
	return map[string]any{}
}

// MergeParameters overlays the tool's own parameters on the toolkit
// configuration's — the legacy line is `if key not in params or value`, which
// the Inventory plugin carried verbatim from the same shell DeepWiki's did
// (methods/invoke.py::perform_invoke_request).
//
// Two consequences, both preserved because a caller may depend on either: a
// tool argument absent from the configuration always lands, and one that is
// present only overrides when the tool's value is truthy — so an explicit
// full_rebuild=false does NOT override a configured true.
func MergeParameters(request map[string]any) Params {
	params := Params{}
	for k, v := range configurationParameters(request) {
		params[k] = v
	}
	if tool := object(request["parameters"]); tool != nil {
		for k, v := range tool {
			if _, present := params[k]; !present || Truthy(v) {
				params[k] = v
			}
		}
	}
	return params
}

// Identity is which project and which toolkit a call is about — the pair the
// graph is stored under.
type Identity struct {
	ProjectID     any
	ApplicationID any
}

// ExtractIdentity reads the ids the legacy handler read: from `configuration`
// first, then from the merged parameters.
//
// The search family is the exception the legacy code carried: an
// `inventory_search` toolkit REFERENCES another toolkit's graph, so the
// application id is the referenced toolkit's, and the legacy handler read the
// project id off the request ROOT rather than off `configuration` — see
// _handle_inventory_search_tool. Both shapes are read here, so neither a
// facade that sends one nor one that sends the other loses its graph.
func ExtractIdentity(family string, request map[string]any, params Params) Identity {
	configuration := object(request["configuration"])
	identity := Identity{
		ProjectID:     firstPresent(configuration["project_id"], request["project_id"], params["project_id"]),
		ApplicationID: firstPresent(configuration["application_id"], params["application_id"]),
	}
	if family != searchFamily {
		return identity
	}
	// inventory_search names its target through `inventory_toolkit`, which the
	// facade expands to an object or leaves as an id.
	reference := params["inventory_toolkit"]
	if expanded := object(reference); expanded != nil {
		if id := expanded["id"]; Truthy(id) {
			identity.ApplicationID = id
		}
		return identity
	}
	if Truthy(reference) {
		identity.ApplicationID = reference
	}
	return identity
}

func firstPresent(values ...any) any {
	for _, value := range values {
		if Truthy(value) {
			return value
		}
	}
	return nil
}

// ArgumentsFor is the argument set the sidecar receives for one call.
//
// Unlike DeepWiki's, this is uniform: every Inventory tool takes the same
// merged parameter set, and the engine's copied handlers read what they need
// out of it — the legacy handlers were called `(params, graph_path,
// request_data)` with exactly that dict. Deriving a per-tool keyword set here
// would be a SECOND declaration of every tool's arguments, disagreeing with the
// descriptor's args_schema at the first divergence.
//
// What the host does add is the four things the copied handlers cannot derive
// for themselves any more: the admitted family (dispatch is per-family), the
// project and toolkit ids (the graph's address), and nothing else. The source
// object and llm_settings are already IN params, forwarded by the facade.
func ArgumentsFor(family, tool string, params Params, identity Identity) map[string]any {
	arguments := map[string]any{
		"family": family,
		"tool":   tool,
		"params": params,
	}
	if identity.ProjectID != nil {
		arguments["project_id"] = identity.ProjectID
	}
	if identity.ApplicationID != nil {
		arguments["application_id"] = identity.ApplicationID
	}
	return arguments
}

// EngineError rebuilds the exception a failed engine result stands for:
// {"success": false, "error": …} with optional error_type / error_category
// hints. The same mapping DeepWiki's runner performs, minus the
// "[SERVICE_BUSY]" branch — the Inventory plugin never emitted that marker
// (its slot refusal was an `ingestion_slots_busy` JSON body, which is a
// SUCCESSFUL result, and under ADR-0023 slots are refused by the host before a
// runner is reached at all).
func EngineError(result map[string]any) error {
	message := strings.TrimSpace(str(result["error"]))
	errorType := str(result["error_type"])
	category := str(result["error_category"])
	if category == "invalid_input" || errorType == "ValueError" {
		if message == "" {
			message = "Invalid input"
		}
		return spi.Failf(spi.KindValue, "%s", message)
	}
	if category == "resource_not_found" || errorType == "FileNotFoundError" {
		if message == "" {
			message = "Not found"
		}
		return spi.Failf(spi.KindNotFound, "%s", message)
	}
	if message == "" {
		message = "Unknown error"
	}
	return spi.Failf(spi.KindRuntime, "%s", message)
}
