package mcpregistry

// The PRE-BUILT MCP server catalogue: the platform-wide list of MCP servers an
// operator offers as ready-made toolkits, and the rule that merges one of its
// entries into a toolkit's own settings.
//
// # What this replaces
//
// pylon keeps the catalogue in the `mcp_servers` block of the indexer_worker
// plugin's YAML descriptor. `indexer_worker/methods/indexer_mcp_prebuilt_config.py`
// reads that block and emits `application_mcp_prebuilt_config_collected` on the
// Arbiter event bus; `elitea_core` caches the payload in module state and serves
// it through `get_mcp_prebuilt_config()` and `resolve_mcp_prebuilt_settings()`
// (`elitea_core/methods/mcp_prebuilt_config.py`).
//
// This service loads no plugins, has no descriptor to patch and speaks no
// Arbiter, so none of that could be ported as it stands. The catalogue is a
// table here (shared migration 0094) and the two functions are Resolve and the
// store's Lookup.
//
// # One behaviour is deliberately better than the reference
//
// pylon reads the catalogue into module state at plugin load, which is why the
// admin field carries `requires_restart: true`. This reads rows per request, so
// an operator's edit takes effect on the next call. That is the same correction
// `internal/platformconfig` records for the flags on the same admin page.
//
// # The catalogue holds no secret
//
// `ClientSecretRef` NAMES an entry in the global secret vault; the value is
// sealed there by the admin write path and read back only at resolution time.
// pylon's own example block carries `client_secret` in clear text in a YAML
// file, which is exactly what `internal/api/v2/admin/config_values.go` refuses
// for a platform-configuration row.

import "strings"

// PrebuiltServer is one catalogue entry.
//
// Key is the normalised lookup key (see NormalizeCatalogueKey). DisplayName is
// the operator's original spelling — pylon carries the same distinction as
// `original_name` alongside the normalised dictionary key.
type PrebuiltServer struct {
	Key             string
	DisplayName     string
	ServerURL       string
	BaseURL         string
	ClientID        string
	ClientSecretRef string
	TimeoutSeconds  int
	Headers         map[string]string
	ConfigSchema    map[string]any
	Enabled         bool
}

// PrebuiltToolkitTypePrefix is the marker that makes a toolkit type a pre-built
// MCP toolkit.
//
// pylon gates the whole resolution on it — `resolve_mcp_prebuilt_settings`
// returns `raw_data` untouched unless the type starts with `mcp_`. That gate is
// reproduced exactly, because it is what stops a toolkit that merely shares a
// name with a catalogue entry from silently acquiring the catalogue's
// credentials.
const PrebuiltToolkitTypePrefix = "mcp_"

// NormalizeCatalogueKey reduces a catalogue name or a toolkit type to the key
// they are matched on.
//
// This is `normalize_mcp_toolkit_name` in
// `elitea_core/methods/mcp_prebuilt_config.py`: lowercase, spaces to
// underscores, trimmed, and a leading `mcp_` removed. Both sides of the lookup
// go through it, so "Epam Presales" (the operator's spelling) and
// "mcp_epam_presales" (the toolkit type) both reduce to "epam_presales".
//
// The order matters and is pylon's: the prefix is stripped AFTER the
// lower-casing and the space substitution, so "MCP Epam Presales" reduces to
// "epam_presales" too.
func NormalizeCatalogueKey(name string) string {
	if name == "" {
		return ""
	}
	normalized := strings.TrimSpace(strings.ReplaceAll(strings.ToLower(name), " ", "_"))
	return strings.TrimPrefix(normalized, PrebuiltToolkitTypePrefix)
}

// IsPrebuiltToolkitType reports whether a toolkit type names a pre-built MCP
// toolkit, which is pylon's `toolkit_type.startswith('mcp_')`.
//
// The comparison is on the RAW type, not the normalised key, because that is
// what pylon tests. A type that is already normalised carries no prefix and is
// not a pre-built toolkit as far as this gate is concerned.
func IsPrebuiltToolkitType(toolkitType string) bool {
	return strings.HasPrefix(toolkitType, PrebuiltToolkitTypePrefix)
}

// ResolvedSecretLookup resolves a catalogue entry's client-secret reference to
// its plaintext value.
//
// It is a function rather than a field on the entry so that the secret is
// fetched only when a resolution actually needs it, and never travels with an
// entry that is merely being listed. A lookup that fails returns an empty
// string and an error; Resolve treats both as "no secret to inject" and leaves
// the caller's own value, if any, in place.
type ResolvedSecretLookup func(reference string) (string, error)

// Resolve merges a catalogue entry into a toolkit's settings.
//
// This is `resolve_mcp_prebuilt_settings`. The priority order is pylon's, and
// it is the important part: a value the CALLER supplied always wins. The
// catalogue fills a field only when the caller left it absent or empty, so an
// operator's default can never overwrite a toolkit's own configuration.
//
// The fillable set is pylon's list minus one entry:
//
//	url, headers, timeout, client_id, client_secret, base_url
//
// `ssl_verify` is pylon's seventh and is NOT filled here. This service verifies
// TLS unconditionally — `MCPSyncTools` already documents that it reads and
// ignores the caller's `ssl_verify` for that reason — so injecting a catalogue
// value for it would write a setting that nothing honours, which reads as a
// working control and is not one.
//
// settings is not mutated. The caller gets a fresh map, matching pylon's
// `result = dict(raw_data)`, so a stored toolkit configuration is never
// rewritten as a side effect of a discovery.
func Resolve(
	settings map[string]any,
	toolkitType string,
	entry *PrebuiltServer,
	secret ResolvedSecretLookup,
) map[string]any {
	if !IsPrebuiltToolkitType(toolkitType) || entry == nil || !entry.Enabled {
		return settings
	}

	resolved := make(map[string]any, len(settings)+6)
	for key, value := range settings {
		resolved[key] = value
	}

	if isBlank(resolved["url"]) && entry.ServerURL != "" {
		resolved["url"] = entry.ServerURL
	}
	if isBlank(resolved["base_url"]) && entry.BaseURL != "" {
		resolved["base_url"] = entry.BaseURL
	}
	if isBlank(resolved["client_id"]) && entry.ClientID != "" {
		resolved["client_id"] = entry.ClientID
	}
	if isBlank(resolved["timeout"]) && entry.TimeoutSeconds > 0 {
		resolved["timeout"] = entry.TimeoutSeconds
	}
	if isBlank(resolved["headers"]) && len(entry.Headers) > 0 {
		headers := make(map[string]any, len(entry.Headers))
		for name, value := range entry.Headers {
			headers[name] = value
		}
		resolved["headers"] = headers
	}
	if isBlank(resolved["client_secret"]) && entry.ClientSecretRef != "" && secret != nil {
		// A vault miss leaves the field absent rather than writing an empty
		// string. An empty client_secret is a value a downstream OAuth exchange
		// would send and be rejected for; an absent one lets the caller's own
		// error path say the credential is missing.
		if value, err := secret(entry.ClientSecretRef); err == nil && value != "" {
			resolved["client_secret"] = value
		}
	}

	return resolved
}

// isBlank reports whether a settings value counts as "not supplied".
//
// pylon's test is `not result.get(field)`, Python truthiness: absent, None, "",
// 0, an empty dict and an empty list are all falsy and all get filled. This
// reproduces that rather than testing only for absence, because the case that
// actually occurs is a form that posts `"url": ""` for an untouched field —
// treating that as supplied would leave the URL empty and the discovery would
// fail with the catalogue entry sitting right there unused.
func isBlank(value any) bool {
	switch typed := value.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(typed) == ""
	case bool:
		return !typed
	case float64:
		return typed == 0
	case int:
		return typed == 0
	case int64:
		return typed == 0
	case []any:
		return len(typed) == 0
	case map[string]any:
		return len(typed) == 0
	case map[string]string:
		return len(typed) == 0
	default:
		return false
	}
}
