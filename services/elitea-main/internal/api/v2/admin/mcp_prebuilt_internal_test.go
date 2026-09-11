package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/mcpregistry"
)

// recordingVault answers every call and remembers what it was asked to do, so a
// test can assert that a REFUSED write sealed nothing. A refusal that still
// wrote to the vault would leave an orphaned credential behind every rejected
// save.
type recordingVault struct {
	stored  map[string]string
	deleted []string
	err     error
}

func newRecordingVault() *recordingVault {
	return &recordingVault{stored: map[string]string{}}
}

func (v *recordingVault) StoreAdminHiddenSecret(_ context.Context, name, value string) error {
	if v.err != nil {
		return v.err
	}
	v.stored[name] = value
	return nil
}

func (v *recordingVault) DeleteAdminHiddenSecret(_ context.Context, name string) error {
	if v.err != nil {
		return v.err
	}
	v.deleted = append(v.deleted, name)
	return nil
}

// request builds a request whose chi context carries {key}, which is how the
// handlers read the catalogue key.
func request(method, key, body string) *http.Request {
	req := httptest.NewRequest(method, "/mcp_prebuilt_servers/administration/"+key, strings.NewReader(body))
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("key", key)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, routeContext))
}

// An unwired catalogue answers 503 and never pretends to have saved anything.
// This is the #128 shape the repository keeps rediscovering: a route that
// answers 200 while nothing behind it is composed.
func TestPrebuiltMCPRoutesRefuseWhenUnwired(t *testing.T) {
	handler := NewHandler(nil)

	for name, call := range map[string]func(http.ResponseWriter, *http.Request){
		"list":   handler.PrebuiltMCPList,
		"save":   handler.PrebuiltMCPSave,
		"delete": handler.PrebuiltMCPDelete,
	} {
		recorder := httptest.NewRecorder()
		call(recorder, request(http.MethodPut, "github_copilot", `{"display_name":"GitHub Copilot"}`))
		require.Equal(t, http.StatusServiceUnavailable, recorder.Code, "route %s", name)
	}
}

// The vault is half the dependency. A handler given a catalogue store and NO
// vault must refuse too, rather than storing a definition whose client secret
// silently vanished.
func TestPrebuiltMCPRefusesWithACatalogueButNoVault(t *testing.T) {
	handler := NewHandler(nil, WithPrebuiltMCPCatalogue(mcpregistry.NewPrebuiltStore(nil), nil))
	recorder := httptest.NewRecorder()
	handler.PrebuiltMCPSave(recorder, request(http.MethodPut, "x", `{"display_name":"X"}`))
	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
}

func wiredHandler(vault *recordingVault) *Handler {
	// A store over a nil pool: every read and write returns ErrNoPool. That is
	// exactly what these tests want — the refusals under test must all happen
	// BEFORE the store is ever consulted.
	return NewHandler(nil, WithPrebuiltMCPCatalogue(mcpregistry.NewPrebuiltStore(nil), vault))
}

// A stdio server is a subprocess on the MCP client's host. This service starts
// none and its discoverer speaks streamable HTTP only, so the definition is
// refused with that reason rather than stored as a row nothing can honour.
func TestPrebuiltMCPSaveRefusesStdio(t *testing.T) {
	vault := newRecordingVault()
	recorder := httptest.NewRecorder()

	wiredHandler(vault).PrebuiltMCPSave(recorder, request(http.MethodPut, "local_thing",
		`{"display_name":"Local Thing","transport":"stdio","client_secret":"s3cret"}`))

	require.Equal(t, http.StatusBadRequest, recorder.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	require.Contains(t, body["error"], "stdio")
	require.Empty(t, vault.stored, "a refused save must not seal a secret")
}

func TestPrebuiltMCPSaveAcceptsAnExplicitHTTPTransport(t *testing.T) {
	recorder := httptest.NewRecorder()
	wiredHandler(newRecordingVault()).PrebuiltMCPSave(recorder, request(http.MethodPut, "x",
		`{"display_name":"X","transport":"HTTP"}`))

	// The store is over a nil pool, so this gets as far as the write and fails
	// there — which is the point: it passed the transport gate.
	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
}

func TestPrebuiltMCPSaveRequiresADisplayName(t *testing.T) {
	vault := newRecordingVault()
	recorder := httptest.NewRecorder()

	wiredHandler(vault).PrebuiltMCPSave(recorder, request(http.MethodPut, "x",
		`{"display_name":"   ","client_secret":"s3cret"}`))

	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Empty(t, vault.stored, "a refused save must not seal a secret")
}

func TestPrebuiltMCPSaveRefusesANegativeTimeout(t *testing.T) {
	vault := newRecordingVault()
	recorder := httptest.NewRecorder()

	wiredHandler(vault).PrebuiltMCPSave(recorder, request(http.MethodPut, "x",
		`{"display_name":"X","timeout":-1,"client_secret":"s3cret"}`))

	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Empty(t, vault.stored)
}

func TestPrebuiltMCPSaveRefusesAnUnusableKey(t *testing.T) {
	recorder := httptest.NewRecorder()
	wiredHandler(newRecordingVault()).PrebuiltMCPSave(recorder, request(http.MethodPut, "",
		`{"display_name":"X"}`))
	require.Equal(t, http.StatusBadRequest, recorder.Code)
}

func TestPrebuiltMCPSaveValidatesTemplatesBeforeSealingSecrets(t *testing.T) {
	vault := newRecordingVault()
	recorder := httptest.NewRecorder()

	wiredHandler(vault).PrebuiltMCPSave(recorder, request(http.MethodPut, "ado",
		`{"display_name":"ADO","url":"https://{org}.example.test/mcp",`+
			`"client_secret":"must-not-be-sealed","config_schema":{"properties":{`+
			`"org":{"type":"string","required":true}}}}`))

	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Empty(t, vault.stored, "an invalid template must not seal a secret")
}

// The mask is what makes this surface safe to render. A set secret must show as
// the mask and never as its value; an unset one must be omitted entirely, so
// "no secret" and "a secret you may not read" stay distinguishable.
func TestPrebuiltViewNeverCarriesAPlaintextSecret(t *testing.T) {
	withSecret := prebuiltView(mcpregistry.PrebuiltServer{
		Key: "k", DisplayName: "K", ClientSecretRef: "mcp_prebuilt__k__client_secret",
	})
	require.Equal(t, prebuiltSecretMask, withSecret.ClientSecret)

	withoutSecret := prebuiltView(mcpregistry.PrebuiltServer{Key: "k", DisplayName: "K"})
	require.Empty(t, withoutSecret.ClientSecret)

	encoded, err := json.Marshal(withoutSecret)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "client_secret",
		"an unset secret must be omitted, not rendered as an empty string")
}

// A view must never leak the vault NAME either. The name is derived from the
// catalogue key, so it is guessable, but publishing it invites a caller to go
// looking for it in the Secrets surface.
func TestPrebuiltViewOmitsTheVaultReference(t *testing.T) {
	encoded, err := json.Marshal(prebuiltView(mcpregistry.PrebuiltServer{
		Key: "k", DisplayName: "K", ClientSecretRef: "mcp_prebuilt__k__client_secret",
	}))
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "mcp_prebuilt__k__client_secret")
}

// Headers must encode as an object even when the entry has none. A null would
// make a client that iterates them fail rather than render an empty set.
func TestPrebuiltViewRendersAbsentHeadersAsAnObject(t *testing.T) {
	encoded, err := json.Marshal(prebuiltView(mcpregistry.PrebuiltServer{Key: "k", DisplayName: "K"}))
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"headers":{}`)
}

func TestPrebuiltViewCarriesTheDynamicConfigurationSchema(t *testing.T) {
	view := prebuiltView(mcpregistry.PrebuiltServer{
		Key: "ado", DisplayName: "ADO",
		ConfigSchema: map[string]any{"properties": map[string]any{
			"api_token": map[string]any{"type": "string", "secret": true},
		}},
	})
	properties := view.ConfigSchema["properties"].(map[string]any)
	require.Equal(t, true, properties["api_token"].(map[string]any)["secret"])
}

// The vault name is derived, never stored, so a row and its secret cannot drift
// apart. It must also be a name the vault will accept: `validSecretName` in the
// secrets package admits `[A-Za-z0-9_]+` only, and catalogue keys come from
// operator-typed display names.
func TestPrebuiltSecretNameIsVaultSafe(t *testing.T) {
	for _, key := range []string{"github_copilot", "épam-présales", "a b/c", "__padded__"} {
		name := prebuiltSecretName(mcpregistry.NormalizeCatalogueKey(key))
		require.Regexp(t, `^[A-Za-z0-9_]+$`, name, "key %q", key)
		require.Contains(t, name, prebuiltSecretFeature)
		require.True(t, strings.HasSuffix(name, "client_secret"))
	}
}

// Two different catalogue keys must never derive the same vault name, or one
// entry's save would overwrite the other's credential and the loser would then
// authenticate with a secret belonging to a different server.
//
// The pairs below are the reachable collisions, not invented ones. Padding
// survives normalisation (pylon strips whitespace only after the spaces have
// become underscores), and every character outside `[a-z0-9_]` reduces to an
// underscore, so `a-b` and `a/b` are two storable keys with one readable form.
func TestPrebuiltSecretNamesAreDistinctPerKey(t *testing.T) {
	for _, pair := range [][2]string{
		{"github_copilot", "epam_presales"},
		{"epam_presales", "__epam_presales__"},
		{"a-b", "a/b"},
		{"a_b", "a-b"},
	} {
		require.NotEqual(t, prebuiltSecretName(pair[0]), prebuiltSecretName(pair[1]),
			"keys %q and %q must not share a vault name", pair[0], pair[1])
	}
}
