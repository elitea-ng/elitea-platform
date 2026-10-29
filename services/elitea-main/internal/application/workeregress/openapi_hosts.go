package workeregress

import (
	"bytes"
	"encoding/json"
	"net/url"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"
)

// The OpenAPI toolkit's hosts are not URL-valued settings fields: the worker
// (libs/rust/agent-runtime/src/toolkits/families/openapi) fetches a spec
// named by URL under `spec`/`schema_settings`/`openapi_spec` (source.rs
// `load`), and calls every operation on ONE base URL (spec.rs
// `parse_base_url`): the `base_url`/`base_url_override` setting when set,
// else the spec's `servers[0].url` with each `{variable}` replaced by its
// `default`. openAPIHosts mirrors both so that an allowlisted run reaches
// exactly the hosts the worker contacts.
//
// A spec given by URL is read by the worker, never by Main: Main cannot see
// its `servers`. A relative or absent server resolves against the spec URL
// (source.rs `resolve_servers`), so it is the spec URL's host, which is
// admitted. An absolute server URL inside a fetched spec is not knowable
// here: the operator names it in `base_url`, or allowlists its host.

// openAPISpecKeys are the settings keys the worker reads the spec from, in
// its order (config.rs / source.rs): the first key present wins.
var openAPISpecKeys = []string{"spec", "schema_settings", "openapi_spec"}

// Bounds the worker applies too (spec.rs MAX_SPEC_BYTES, MAX_URL_BYTES, the
// 128 variable substitutions of parse_base_url).
const (
	maxOpenAPISpecBytes     = 1024 * 1024
	maxOpenAPIURLBytes      = 8 * 1024
	maxOpenAPISubstitutions = 128
)

func isOpenAPISpecKey(key string) bool {
	for _, candidate := range openAPISpecKeys {
		if key == candidate {
			return true
		}
	}
	return false
}

// openAPIHosts answers the hosts an OpenAPI toolkit's worker contacts that
// the URL-field walk does not find: the spec URL's host, and the operations'
// base host (an override, or the spec's first server). A settings document
// that is a typed toolkit (`{"type": ..., "settings": {...}}`) is read from
// its `settings`.
func openAPIHosts(content []byte) []string {
	var document map[string]any
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.UseNumber()
	if err := decoder.Decode(&document); err != nil {
		return nil
	}
	settings := document
	if !hasOpenAPISpecKey(settings) {
		if nested, ok := document["settings"].(map[string]any); ok {
			settings = nested
		}
	}
	var hosts []string
	override := ""
	for _, key := range []string{"base_url", "base_url_override"} {
		if text, ok := settings[key].(string); ok && text != "" {
			override = text
			break
		}
	}
	if override != "" {
		if host, ok := httpsBaseHost(override); ok {
			hosts = append(hosts, host)
		}
	}
	var spec any
	found := false
	for _, key := range openAPISpecKeys {
		if value, ok := settings[key]; ok {
			spec, found = value, true
			break
		}
	}
	if !found {
		return hosts
	}
	switch typed := spec.(type) {
	case string:
		trimmed := strings.TrimSpace(typed)
		if strings.HasPrefix(trimmed, "https://") || strings.HasPrefix(trimmed, "http://") {
			if host, ok := openAPISourceHost(trimmed); ok {
				hosts = append(hosts, host)
			}
			return hosts
		}
		if override != "" || len(typed) > maxOpenAPISpecBytes {
			return hosts
		}
		root, ok := inlineOpenAPISpec(trimmed)
		if !ok {
			return hosts
		}
		spec = root
	}
	if override != "" {
		return hosts
	}
	root, ok := spec.(map[string]any)
	if !ok {
		return hosts
	}
	if host, ok := openAPIServerHost(root); ok {
		hosts = append(hosts, host)
	}
	return hosts
}

func hasOpenAPISpecKey(settings map[string]any) bool {
	for _, key := range openAPISpecKeys {
		if _, ok := settings[key]; ok {
			return true
		}
	}
	return false
}

// inlineOpenAPISpec reads an inline spec as JSON, else YAML (spec.rs
// parse_source). yaml.v3 refuses excessive alias expansion; the size is
// bounded by the caller.
func inlineOpenAPISpec(raw string) (map[string]any, bool) {
	var document map[string]any
	if err := json.Unmarshal([]byte(raw), &document); err == nil {
		return document, true
	}
	document = nil
	if err := yaml.Unmarshal([]byte(raw), &document); err != nil || document == nil {
		return nil, false
	}
	return document, true
}

// openAPIServerHost expands `servers[0]` as spec.rs parse_base_url does and
// answers its host when the result is an https base the worker accepts.
func openAPIServerHost(root map[string]any) (string, bool) {
	servers, ok := root["servers"].([]any)
	if !ok || len(servers) == 0 {
		return "", false
	}
	server, ok := servers[0].(map[string]any)
	if !ok {
		return "", false
	}
	template, ok := server["url"].(string)
	if !ok || len(template) > maxOpenAPIURLBytes {
		return "", false
	}
	variables, _ := server["variables"].(map[string]any)
	expanded := template
	for substitutions := 0; ; substitutions++ {
		start := strings.IndexByte(expanded, '{')
		if start < 0 {
			break
		}
		if substitutions >= maxOpenAPISubstitutions {
			return "", false
		}
		length := strings.IndexByte(expanded[start+1:], '}')
		if length < 0 {
			return "", false
		}
		end := start + 1 + length
		variable, _ := variables[expanded[start+1:end]].(map[string]any)
		value, ok := variable["default"].(string)
		if !ok || value == "" || len(value) > maxOpenAPIURLBytes || strings.IndexFunc(value, unicode.IsControl) >= 0 {
			return "", false
		}
		expanded = expanded[:start] + value + expanded[end+1:]
		if len(expanded) > maxOpenAPIURLBytes {
			return "", false
		}
	}
	if strings.Contains(expanded, "}") {
		return "", false
	}
	return httpsBaseHost(expanded)
}

// httpsBaseHost answers the host of an absolute https base URL with no
// user information, query or fragment (spec.rs parse_https_base).
func httpsBaseHost(raw string) (string, bool) {
	if len(raw) > maxOpenAPIURLBytes || strings.Contains(raw, `\`) {
		return "", false
	}
	parsed, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(parsed.Scheme, "https") || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", false
	}
	return hostOf(raw)
}

// openAPISourceHost answers the host of a spec URL the worker fetches:
// https, no user information, no fragment; a query is allowed (source.rs
// source_url).
func openAPISourceHost(raw string) (string, bool) {
	if len(raw) > maxOpenAPIURLBytes {
		return "", false
	}
	parsed, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(parsed.Scheme, "https") || parsed.User != nil || parsed.Fragment != "" {
		return "", false
	}
	return hostOf(raw)
}
