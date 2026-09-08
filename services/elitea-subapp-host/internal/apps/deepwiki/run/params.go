// Package run is the DeepWiki tool runner's engine-facing half, ported from
// the Python shell's legacy_runner.py: the parameter merge, the wikis_query
// rewrite, the per-tool argument sets, the failure-to-exception mapping,
// result composition and artifact upload. Everything here is pinned by the
// P0 fixture conformance/provider/fixtures/deepwiki/generation/
// composed_result.json, which was recorded by running the legacy composer
// itself, and by the Python tests ported alongside it.
//
// The analysis engine — what the tools DO — is not here. A Runner takes a
// table of Tool functions; the fixture tools in fixture.go are one such
// table, the copied Python engine (ADR-0023 H2) will be another.
package run

import (
	"reflect"

	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/spi"
)

// Params is a tool's merged parameter set: JSON-decoded values, so a nested
// object is a map[string]any and a list is []any.
type Params = map[string]any

// Truthy is Python's truth for a JSON-decoded value, because the legacy
// merge and the legacy composer branch on `if value` and `or`, not on
// presence. nil, false, 0, "", an empty list and an empty object are false.
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

// object returns value as a JSON object, or nil when it is not one.
func object(value any) map[string]any {
	m, _ := value.(map[string]any)
	return m
}

// str returns value as a string, or "" when it is not one.
func str(value any) string {
	s, _ := value.(string)
	return s
}

// firstTruthy is Python's `a or b or c` over string-ish values.
func firstTruthy(values ...any) any {
	for _, v := range values {
		if Truthy(v) {
			return v
		}
	}
	if len(values) == 0 {
		return nil
	}
	return values[len(values)-1]
}

// Parameters reads request_data.configuration.parameters, or an empty map.
func configurationParameters(request map[string]any) map[string]any {
	if p := object(object(request["configuration"])["parameters"]); p != nil {
		return p
	}
	return map[string]any{}
}

// MergeParameters overlays the tool's own parameters on the toolkit
// configuration's — the legacy line is `if key not in params or value`.
// Two consequences, both preserved because a caller may depend on either:
// a tool argument absent from the configuration always lands, and one that
// is present only overrides when the tool's value is truthy, so an explicit
// exclude_tests=false does NOT override a configured true.
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

// TransformQueryRequest rewrites a wikis_query request to reference the
// target Wikis toolkit. The legacy service resolved a toolkit id by calling
// the platform's configuration API; under ADR-0022 decision 6 that is the
// FACADE's job, so a bare id is refused rather than resolved, and only an
// expanded object is merged.
func TransformQueryRequest(request map[string]any) (map[string]any, error) {
	parameters := configurationParameters(request)
	reference := parameters["wikis_toolkit"]
	if !Truthy(reference) {
		reference = parameters["deepwiki_toolkit"]
	}
	if !Truthy(reference) {
		return nil, spi.Failf(spi.KindValue, "wikis_toolkit parameter is required - specify which Wikis toolkit to use")
	}
	expanded := object(reference)
	if expanded == nil {
		return nil, spi.Failf(spi.KindValue, "wikis_toolkit must arrive expanded; this service does not resolve toolkit references, the facade does")
	}
	merged := map[string]any{}
	for k, v := range expanded {
		merged[k] = v
	}
	for _, key := range []string{"llm_settings", "llm_model", "embedding_model"} {
		if v, ok := parameters[key]; ok {
			merged[key] = v
		}
	}
	toolParameters := object(request["parameters"])
	if toolParameters == nil {
		toolParameters = map[string]any{}
	}
	return map[string]any{
		"configuration": map[string]any{"parameters": merged},
		"parameters":    toolParameters,
	}, nil
}

// get is params[key] when present, else the default — Python's dict.get.
func get(params Params, key string, fallback any) any {
	if v, ok := params[key]; ok {
		return v
	}
	return fallback
}

// ArgumentsFor is the per-tool keyword set the legacy handler passed to the
// engine, defaults included. Pinned by composed_result.json's engine_call,
// which recorded the exact set for generate_wiki.
func ArgumentsFor(tool string, params Params) map[string]any {
	common := map[string]any{
		"llm_settings":    firstTruthy(params["llm_settings"], map[string]any{}),
		"embedding_model": params["embedding_model"],
	}
	switch tool {
	case "generate_wiki":
		return merge(common, map[string]any{
			"query":               params["query"],
			"repo_config":         ExtractRepoConfig(params).Map(),
			"active_branch":       get(params, "active_branch", "main"),
			"force_rebuild_index": get(params, "force_rebuild_index", true),
			"indexing_method":     get(params, "indexing_method", "filesystem"),
			"planner_mode":        firstTruthy(params["planner_mode"], params["planner_type"]),
			"exclude_tests":       params["exclude_tests"],
			"run_in_subprocess":   get(params, "run_in_subprocess", true),
		})
	case "ask", "deep_research":
		arguments := merge(common, map[string]any{
			"question":                 get(params, "question", ""),
			"repo_config":              ExtractRepoConfig(params).Map(),
			"chat_history":             get(params, "chat_history", []any{}),
			"k":                        get(params, "k", 15),
			"repo_identifier_override": params["repo_identifier_override"],
			"analysis_key_override":    params["analysis_key_override"],
		})
		if tool == "deep_research" {
			arguments["research_type"] = get(params, "research_type", "general")
			arguments["enable_subagents"] = get(params, "enable_subagents", true)
		}
		return arguments
	case "list_wikis":
		// The wiki_query family, from the legacy `_handle_wiki_query_tool`
		// merge: each handler read its keywords off the merged `params`
		// dict, so the keyword set is the keys each one actually reads.
		return merge(common, map[string]any{
			"include_metadata": get(params, "include_metadata", false),
		})
	case "resolve_and_ask", "resolve_and_deep_research":
		arguments := merge(common, map[string]any{
			"question":     get(params, "question", ""),
			"wiki_id_hint": params["wiki_id_hint"],
			"chat_history": get(params, "chat_history", []any{}),
			"k":            get(params, "k", 15),
		})
		if tool == "resolve_and_deep_research" {
			arguments["research_type"] = get(params, "research_type", "general")
			arguments["enable_subagents"] = get(params, "enable_subagents", true)
		}
		return arguments
	case "delete_wiki":
		return merge(common, map[string]any{"wiki_id": get(params, "wiki_id", "")})
	}
	arguments := map[string]any{}
	for k, v := range params {
		if len(k) > 0 && k[0] != '_' {
			arguments[k] = v
		}
	}
	return arguments
}

func merge(maps ...map[string]any) map[string]any {
	out := map[string]any{}
	for _, m := range maps {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

// EngineError rebuilds the exception the legacy handler raised for a failed
// result: {"success": false, "error": …} with optional error_type /
// error_category hints from the subprocess workers. A "[SERVICE_BUSY]"
// prefix is stripped BEFORE the message is built — and the classifier then
// looks for that very marker, so an explained busy signal classifies as
// runtime_error while a bare one falls through to the default text and
// classifies as service_busy. A legacy defect, preserved because the error
// contract is frozen (see the Python test that names it).
func EngineError(result map[string]any) error {
	message := str(result["error"])
	errorType := str(result["error_type"])
	category := str(result["error_category"])
	trimmed := trimSpace(message)
	if hasPrefix(trimmed, "[SERVICE_BUSY]") {
		clean := trimSpace(trimmed[len("[SERVICE_BUSY]"):])
		if clean == "" {
			clean = "DeepWiki service is busy. Please try again later."
		}
		return spi.Failf(spi.KindRuntime, "%s", clean)
	}
	if category == "invalid_input" || errorType == "ValueError" {
		if message == "" {
			message = "Invalid input"
		}
		return spi.Failf(spi.KindValue, "%s", message)
	}
	if message == "" {
		message = "Unknown error"
	}
	return spi.Failf(spi.KindRuntime, "%s", message)
}
