package admin

// The admin surface of the PRE-BUILT MCP server catalogue — the replacement for
// the Configuration page's dead "MCP Servers" section.
//
// # Why this is its own surface and not that section made writable
//
// The section is one field of type `object` whose value is the whole
// `mcp_servers` block, and the plugin-configuration write path stores a
// section's fields as plaintext JSONB rows in `centry.platform_config`. The
// catalogue carries `client_secret` — pylon's own documented example block does
// — and `config_values.go:rejectCredentialField` refuses credentials into those
// rows, correctly: they are readable by every holder of `runtime.plugins`.
//
// Making the section writable would therefore have meant one of two things:
// storing operator credentials in clear text, or accepting a catalogue with the
// credential silently dropped. Both are worse than a section that says where
// the real surface is, which is what `config_schemas.go` now says.
//
// # The permission is the one the page it replaces already used
//
// `runtime.plugins`, resolved in administration mode, gates every route here.
// pylon's `plugin_config_values.py` declares the same string, so an operator who
// could edit the section can edit the catalogue and no new grant is needed —
// which also means no new way for the grant gate in
// `internal/api/router_permission_grant_gate_test.go` to be tripped.
//
// # What a caller can and cannot see
//
// A listing returns the catalogue with `client_secret` rendered as a mask when
// one is set and omitted when one is not. The plaintext is never returned by
// any route here. It is written once, sealed into the GLOBAL vault's hidden
// bucket (`internal/api/v2/secrets/admin_hidden.go`), and read back only by the
// resolver inside this service.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/secrets"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/mcpregistry"
)

// PrebuiltSecretStore is the vault seam this surface writes catalogue
// credentials through.
//
// It is an interface so this package does not import the secrets handler, and
// so a test can assert that a refused write seals nothing.
type PrebuiltSecretStore interface {
	StoreAdminHiddenSecret(ctx context.Context, name, value string) error
	DeleteAdminHiddenSecret(ctx context.Context, name string) error
}

// WithPrebuiltMCPCatalogue supplies the catalogue store and the vault.
//
// Without BOTH, every route here answers 503 and writes nothing. It does not
// fall back to storing the catalogue without its secrets: an entry whose
// client_secret vanished is an entry that authenticates nothing, and it would
// fail at the remote server with a message naming neither this service nor the
// missing credential. This is the same fail-closed rule
// `configurations.WithSecretSealer` states for project configuration secrets.
func WithPrebuiltMCPCatalogue(store *mcpregistry.PrebuiltStore, vault PrebuiltSecretStore) Option {
	return func(h *Handler) {
		h.prebuiltMCP = store
		h.prebuiltMCPVault = vault
	}
}

// prebuiltSecretFeature namespaces the vault names this surface derives, so a
// catalogue entry can never collide with another feature's stored credential.
const prebuiltSecretFeature = "mcp_prebuilt"

// prebuiltSecretMask is what a set client secret renders as. It is the same
// mask the global Secrets listing uses, so the two admin screens agree about
// what "a value is set and you may not read it" looks like.
const prebuiltSecretMask = "******"

// prebuiltServerBody is the wire shape of one catalogue entry.
//
// ClientSecret is a POINTER so that three cases stay distinct on a write:
// absent (leave the sealed secret as it is), an empty string (clear it), and a
// value (re-seal it). Collapsing absent and empty is how a save from a form
// that does not echo secrets silently erases the credential.
type prebuiltServerBody struct {
	Key          string            `json:"key"`
	DisplayName  string            `json:"display_name"`
	URL          string            `json:"url"`
	BaseURL      string            `json:"base_url"`
	ClientID     string            `json:"client_id"`
	ClientSecret *string           `json:"client_secret"`
	Timeout      int               `json:"timeout"`
	Headers      map[string]string `json:"headers"`
	ConfigSchema map[string]any    `json:"config_schema"`
	Enabled      *bool             `json:"enabled"`
	Transport    string            `json:"transport"`
}

// prebuiltServerView is the wire shape a read returns. It never carries a
// plaintext secret.
type prebuiltServerView struct {
	Key          string            `json:"key"`
	DisplayName  string            `json:"display_name"`
	URL          string            `json:"url"`
	BaseURL      string            `json:"base_url"`
	ClientID     string            `json:"client_id"`
	ClientSecret string            `json:"client_secret,omitempty"`
	Timeout      int               `json:"timeout"`
	Headers      map[string]string `json:"headers"`
	ConfigSchema map[string]any    `json:"config_schema"`
	Enabled      bool              `json:"enabled"`
}

func prebuiltView(entry mcpregistry.PrebuiltServer) prebuiltServerView {
	headers := entry.Headers
	if headers == nil {
		headers = map[string]string{}
	}
	view := prebuiltServerView{
		Key:          entry.Key,
		DisplayName:  entry.DisplayName,
		URL:          entry.ServerURL,
		BaseURL:      entry.BaseURL,
		ClientID:     entry.ClientID,
		Timeout:      entry.TimeoutSeconds,
		Headers:      headers,
		ConfigSchema: entry.ConfigSchema,
		Enabled:      entry.Enabled,
	}
	if view.ConfigSchema == nil {
		view.ConfigSchema = map[string]any{"properties": map[string]any{}}
	}
	if entry.ClientSecretRef != "" {
		view.ClientSecret = prebuiltSecretMask
	}
	return view
}

// prebuiltReady reports whether both dependencies are wired, answering 503 when
// they are not.
func (h *Handler) prebuiltReady(w http.ResponseWriter) bool {
	if h.prebuiltMCP != nil && h.prebuiltMCPVault != nil {
		return true
	}
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{
		"error": "the pre-built MCP catalogue is not configured on this deployment",
	})
	return false
}

// PrebuiltMCPList answers `GET /admin/mcp_prebuilt_servers/{mode}`.
func (h *Handler) PrebuiltMCPList(w http.ResponseWriter, r *http.Request) {
	if !h.prebuiltReady(w) {
		return
	}
	entries, err := h.prebuiltMCP.List(r.Context())
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable,
			map[string]any{"error": "the pre-built MCP catalogue could not be read"})
		return
	}
	views := make([]prebuiltServerView, 0, len(entries))
	for _, entry := range entries {
		views = append(views, prebuiltView(entry))
	}
	writeJSON(w, http.StatusOK, map[string]any{"servers": views, "total": len(views)})
}

// PrebuiltMCPSave answers `PUT /admin/mcp_prebuilt_servers/{mode}/{key}`.
func (h *Handler) PrebuiltMCPSave(w http.ResponseWriter, r *http.Request) {
	if !h.prebuiltReady(w) {
		return
	}

	var body prebuiltServerBody
	r.Body = http.MaxBytesReader(w, r.Body, 128*1024)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request body"})
		return
	}

	// The path segment is authoritative over the body, so a caller cannot PUT
	// one key and write another.
	key := mcpregistry.NormalizeCatalogueKey(chi.URLParam(r, "key"))
	if key == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid catalogue key"})
		return
	}

	// stdio is refused with its reason rather than stored. A stdio MCP server is
	// a child process on the machine running the MCP client; this service starts
	// no subprocesses, and its discoverer speaks streamable HTTP only. Accepting
	// the definition would put a row in the catalogue that no path in this stack
	// can honour, and the toolkit built from it would fail at use with an error
	// naming none of this.
	if transport := strings.ToLower(strings.TrimSpace(body.Transport)); transport != "" && transport != "http" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "only http MCP servers can be catalogued here: a stdio server is a subprocess " +
				"on the MCP client's host, and this service starts no subprocesses and speaks " +
				"streamable HTTP only",
		})
		return
	}

	display := strings.TrimSpace(body.DisplayName)
	if display == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "display_name is required"})
		return
	}
	if body.Timeout < 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "timeout must not be negative"})
		return
	}
	if body.ConfigSchema != nil {
		if err := mcpregistry.ValidatePrebuiltServer(mcpregistry.PrebuiltServer{
			ServerURL: body.URL, Headers: body.Headers, ConfigSchema: body.ConfigSchema,
		}); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": "the pre-built MCP parameter schema or template is invalid",
			})
			return
		}
	}

	existing, err := h.prebuiltMCP.Lookup(r.Context(), key)
	switch {
	case err == nil, errors.Is(err, mcpregistry.ErrPrebuiltNotFound):
	default:
		writeJSON(w, http.StatusServiceUnavailable,
			map[string]any{"error": "the pre-built MCP catalogue could not be read"})
		return
	}
	configSchema := body.ConfigSchema
	if configSchema == nil {
		configSchema = existing.ConfigSchema
	}
	candidate := mcpregistry.PrebuiltServer{
		Key:            key,
		DisplayName:    display,
		ServerURL:      body.URL,
		BaseURL:        body.BaseURL,
		ClientID:       body.ClientID,
		TimeoutSeconds: body.Timeout,
		Headers:        body.Headers,
		ConfigSchema:   configSchema,
		Enabled:        true,
	}
	if err := mcpregistry.ValidatePrebuiltServer(candidate); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "the pre-built MCP parameter schema or template is invalid",
		})
		return
	}

	// The secret is sealed BEFORE the row is written. A sealed secret with no
	// row is an orphan in the vault that nothing reads; a row that points at a
	// secret which was never sealed is a catalogue entry that fails at use. The
	// first is inert, the second is a broken feature, so the order is: vault
	// first, and abandon the row write if it fails.
	secretRef := existing.ClientSecretRef
	if body.ClientSecret != nil {
		name := prebuiltSecretName(key)
		if *body.ClientSecret == "" {
			if err := h.prebuiltMCPVault.DeleteAdminHiddenSecret(r.Context(), name); err != nil {
				writeJSON(w, http.StatusServiceUnavailable,
					map[string]any{"error": "the platform secret vault is unavailable"})
				return
			}
			secretRef = ""
		} else {
			if err := h.prebuiltMCPVault.StoreAdminHiddenSecret(r.Context(), name, *body.ClientSecret); err != nil {
				writeJSON(w, http.StatusServiceUnavailable,
					map[string]any{"error": "the platform secret vault is unavailable"})
				return
			}
			secretRef = name
		}
	}

	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}

	saved, err := h.prebuiltMCP.Upsert(r.Context(), mcpregistry.PrebuiltServer{
		Key:             key,
		DisplayName:     display,
		ServerURL:       body.URL,
		BaseURL:         body.BaseURL,
		ClientID:        body.ClientID,
		ClientSecretRef: secretRef,
		TimeoutSeconds:  body.Timeout,
		Headers:         body.Headers,
		ConfigSchema:    configSchema,
		Enabled:         enabled,
	})
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable,
			map[string]any{"error": "the pre-built MCP catalogue could not be written"})
		return
	}
	writeJSON(w, http.StatusOK, prebuiltView(saved))
}

// PrebuiltMCPDelete answers `DELETE /admin/mcp_prebuilt_servers/{mode}/{key}`.
func (h *Handler) PrebuiltMCPDelete(w http.ResponseWriter, r *http.Request) {
	if !h.prebuiltReady(w) {
		return
	}
	key := mcpregistry.NormalizeCatalogueKey(chi.URLParam(r, "key"))
	if key == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid catalogue key"})
		return
	}

	reference, err := h.prebuiltMCP.Delete(r.Context(), key)
	if errors.Is(err, mcpregistry.ErrPrebuiltNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such pre-built MCP server"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable,
			map[string]any{"error": "the pre-built MCP catalogue could not be written"})
		return
	}

	// The row is gone either way. A vault cleanup that fails leaves an entry
	// nothing reads, which is inert — so it is reported in the response rather
	// than turned into a failure that would invite the operator to retry a
	// delete that already happened.
	if reference != "" {
		if err := h.prebuiltMCPVault.DeleteAdminHiddenSecret(r.Context(), reference); err != nil {
			writeJSON(w, http.StatusOK, map[string]any{
				"deleted": key,
				"warning": "the catalogue entry was removed; its stored client secret could not be " +
					"deleted and remains in the platform vault",
			})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": key})
}

// prebuiltSecretName derives the vault name for one catalogue entry's client
// secret. It is derived rather than stored so that a row and its secret can
// never drift apart.
func prebuiltSecretName(key string) string {
	return secrets.AdminHiddenSecretName(prebuiltSecretFeature, key, "client_secret")
}
