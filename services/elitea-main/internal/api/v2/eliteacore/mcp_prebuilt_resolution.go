package eliteacore

// Resolution of the PRE-BUILT MCP server catalogue into a request's settings.
//
// This is the read half of shared migration 0094. The catalogue is defined by
// an operator through `internal/api/v2/admin/mcp_prebuilt.go`; this file is
// what makes a definition do something, by filling the fields a pre-built MCP
// toolkit leaves for the platform to supply.
//
// # The call sites are pylon's, and two of the five have a Go counterpart
//
// pylon resolves in five places. Two are wired here:
//
//	mcp_sync_tools.py:81   → MCPSyncTools    (fills url, headers, timeout)
//	mcp_oauth_proxy.py:112 → MCPOAuthProxy   (fills client_id, client_secret)
//
// The other three are NOT wired, and neither is an omission:
//
//   - `toolkit_discover_tools.py:72` injects a catalogue URL into an OUTBOUND
//     discovery, because pylon's route dispatches `discover_mcp_tools` over RPC
//     to indexer_worker. The Go route of that name is a different thing
//     entirely: `toolkits.pgRepo.DiscoverTools` is a SELECT over the project's
//     own `elitea_tools` rows of a given type. It makes no outbound call, so
//     there is nothing for a URL to point at, and injecting one would be a
//     setting no code reads.
//   - `internal_tools.py:531,550,730` build an agent's tools at execution time.
//     That belongs to the Python execution worker's side of the split and has
//     no Go counterpart to wire.
//
// The MCP discovery this service really performs is `MCPSyncTools`, and that is
// where the catalogue reaches it.
//
// # Why a failure is never silent here
//
// A catalogue read that fails is NOT treated as "no catalogue entry". The two
// have opposite consequences: no entry means the caller's own settings stand,
// while an unreadable catalogue means an entry may exist whose credentials the
// request should have carried and now will not. The second produces an
// authentication failure at a remote server, reported to the user as the remote
// server's problem. So the resolver reports the error and its callers refuse,
// rather than proceeding with a request they know is under-configured.

import (
	"context"
	"errors"
	"log/slog"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/mcpregistry"
)

// PrebuiltSecretReader reads a catalogue client secret out of the platform
// vault by the name the catalogue stored.
//
// It is an interface so this package does not import the secrets handler, and
// so the composition root decides whether a deployment has a vault at all.
type PrebuiltSecretReader interface {
	LookupAdminHiddenSecret(ctx context.Context, name string) (string, error)
}

// WithPrebuiltMCPCatalogue wires the catalogue and the vault that holds its
// client secrets.
//
// Left unwired, resolution is a no-op and every pre-built MCP path behaves
// exactly as it did before the catalogue existed: a toolkit must carry its own
// URL and credentials, and `MCPSyncTools` refuses a URL-less body with the
// sentence it already had. That is the honest degradation — a deployment
// without a catalogue has no definitions to inject — and it is why this is an
// option rather than a required dependency.
func WithPrebuiltMCPCatalogue(store *mcpregistry.PrebuiltStore, vault PrebuiltSecretReader) Option {
	return func(handler *Handler) {
		if store != nil {
			handler.prebuiltMCP = store
		}
		handler.prebuiltMCPVault = vault
	}
}

// prebuiltLookup is the read half of the catalogue store; an interface so a
// test can stand in for the database.
type prebuiltLookup interface {
	Lookup(ctx context.Context, nameOrType string) (mcpregistry.PrebuiltServer, error)
}

// resolvePrebuiltSettings fills a pre-built MCP toolkit's settings from the
// catalogue.
//
// It returns the settings unchanged, and no error, in every case that is not a
// pre-built toolkit or where no catalogue is configured. It returns an error
// only when a catalogue IS configured and could not be read — see the package
// comment for why that is not folded into "no entry".
func (h *Handler) resolvePrebuiltSettings(
	ctx context.Context,
	settings map[string]any,
	toolkitType string,
) (map[string]any, error) {
	resolved, _, err := h.resolvePrebuiltSettingsReport(ctx, settings, toolkitType)
	return resolved, err
}

// resolvePrebuiltSettingsReport is resolvePrebuiltSettings plus the entry used
// and what the catalogue filled. Callers that decide where a credential may go
// need both: a filled client secret is the operator's, not the toolkit's.
func (h *Handler) resolvePrebuiltSettingsReport(
	ctx context.Context,
	settings map[string]any,
	toolkitType string,
) (map[string]any, prebuiltResolution, error) {
	if h.prebuiltMCP == nil || !mcpregistry.IsPrebuiltToolkitType(toolkitType) {
		return settings, prebuiltResolution{}, nil
	}

	entry, err := h.prebuiltMCP.Lookup(ctx, toolkitType)
	if errors.Is(err, mcpregistry.ErrPrebuiltNotFound) {
		// pylon logs and carries on here, and so does this: a toolkit type that
		// merely starts with `mcp_` need not be catalogued, and refusing it
		// would break every hand-configured remote MCP toolkit whose type
		// happens to carry the prefix.
		return settings, prebuiltResolution{}, nil
	}
	if err != nil {
		return nil, prebuiltResolution{}, err
	}

	resolved, filled := mcpregistry.ResolveReport(settings, toolkitType, &entry, func(reference string) (string, error) {
		if h.prebuiltMCPVault == nil {
			return "", errors.New("eliteacore: no platform vault is configured")
		}
		value, err := h.prebuiltMCPVault.LookupAdminHiddenSecret(ctx, reference)
		if err != nil {
			// The secret is logged as missing and the field is left unfilled.
			// Resolve treats an error as "nothing to inject", so the caller's
			// own value, if it had one, survives. An operator reading this line
			// learns which catalogue entry lost its credential.
			slog.ErrorContext(ctx, "mcp prebuilt catalogue: client secret unreadable",
				"toolkit_type", toolkitType, "secret_ref", reference, "err", err)
			return "", err
		}
		return value, nil
	})
	return resolved, prebuiltResolution{entry: entry, filled: filled}, nil
}

// prebuiltResolution is what a catalogue resolution used and supplied.
type prebuiltResolution struct {
	entry  mcpregistry.PrebuiltServer
	filled mcpregistry.Filled
}

// prebuiltURLFor returns the catalogue URL for a toolkit type, or "".
//
// This is the narrow form `toolkit_discover_tools.py` and `internal_tools.py`
// use — `(get_mcp_prebuilt_config(type) or {}).get('url')` — where the only
// missing field is the endpoint. An error is reported so the caller can tell an
// unreadable catalogue from an uncatalogued toolkit.
func (h *Handler) prebuiltURLFor(ctx context.Context, toolkitType string) (string, error) {
	if h.prebuiltMCP == nil || !mcpregistry.IsPrebuiltToolkitType(toolkitType) {
		return "", nil
	}
	entry, err := h.prebuiltMCP.Lookup(ctx, toolkitType)
	if errors.Is(err, mcpregistry.ErrPrebuiltNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if !entry.Enabled {
		return "", nil
	}
	return entry.ServerURL, nil
}

// The settings map is `map[string]any` because that is the shape pylon's
// resolution works on and the shape the OAuth proxy already holds. These four
// helpers move a typed request body in and out of it without letting a wrong
// type silently become a zero value.

func headersAsAny(headers map[string]string) any {
	if len(headers) == 0 {
		// nil rather than an empty map, so Resolve's blank test treats an
		// unset header set as fillable. An empty map is also blank by that
		// test, but nil states the intent at the call site.
		return nil
	}
	converted := make(map[string]any, len(headers))
	for name, value := range headers {
		converted[name] = value
	}
	return converted
}

func stringSetting(settings map[string]any, key, fallback string) string {
	if value, ok := settings[key].(string); ok && value != "" {
		return value
	}
	return fallback
}

func intSetting(settings map[string]any, key string, fallback int) int {
	switch value := settings[key].(type) {
	case int:
		if value != 0 {
			return value
		}
	case int64:
		if value != 0 {
			return int(value)
		}
	case float64:
		// JSON numbers arrive as float64. A non-integral value is ignored
		// rather than truncated: a 1.5-second timeout is not a 1-second one.
		if value != 0 && value == float64(int(value)) {
			return int(value)
		}
	}
	return fallback
}

// headerSettings reads a resolved header map back out.
//
// An entry whose value is not a string is DROPPED rather than formatted. These
// become HTTP header values; rendering a number or an object into one would
// send a header the operator never wrote.
func headerSettings(settings map[string]any, fallback map[string]string) map[string]string {
	raw, ok := settings["headers"].(map[string]any)
	if !ok || len(raw) == 0 {
		return fallback
	}
	headers := make(map[string]string, len(raw))
	for name, value := range raw {
		if text, ok := value.(string); ok {
			headers[name] = text
		}
	}
	if len(headers) == 0 {
		return fallback
	}
	return headers
}
