package eliteacore

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/mcpregistry"
)

func syncRequest(body string) *http.Request {
	return httptest.NewRequest(http.MethodPost,
		"/mcp_sync_tools/prompt_lib/1", strings.NewReader(body))
}

// Without a catalogue the route behaves exactly as it did before the catalogue
// existed: a body with no URL is refused rather than invented. A deployment
// that has configured no catalogue must not start failing differently.
func TestMCPSyncToolsWithoutACatalogueStillRequiresAURL(t *testing.T) {
	recorder := httptest.NewRecorder()
	NewHandler(nil).MCPSyncTools(recorder, syncRequest(`{"toolkit_type":"mcp_github_copilot"}`))

	require.Equal(t, http.StatusBadRequest, recorder.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	require.Equal(t, false, body["success"])
}

// An UNREADABLE catalogue is not an uncatalogued toolkit. Proceeding would open
// the connection without the credentials the operator configured, and the
// remote server's 401 would name neither this service nor its own catalogue.
//
// The store here is built over a nil pool, so every read returns ErrNoPool.
func TestMCPSyncToolsRefusesWhenTheCatalogueIsUnreadable(t *testing.T) {
	handler := NewHandler(nil, WithPrebuiltMCPCatalogue(mcpregistry.NewPrebuiltStore(nil), nil))
	recorder := httptest.NewRecorder()

	handler.MCPSyncTools(recorder, syncRequest(`{"toolkit_type":"mcp_github_copilot"}`))

	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	require.Contains(t, body["error"], "catalogue")
}

// A toolkit that is NOT pre-built must never touch the catalogue, so an
// unreadable catalogue cannot break a hand-configured remote MCP toolkit that
// carries its own URL.
func TestMCPSyncToolsIgnoresTheCatalogueForANonPrebuiltToolkit(t *testing.T) {
	handler := NewHandler(nil, WithPrebuiltMCPCatalogue(mcpregistry.NewPrebuiltStore(nil), nil))
	recorder := httptest.NewRecorder()

	// No toolkit_type at all: the resolution must be skipped entirely. The URL
	// is invalid, so the route stops at its own validation rather than at a
	// catalogue read — which is the proof that the catalogue was not consulted.
	handler.MCPSyncTools(recorder, syncRequest(`{"url":"not-a-url"}`))

	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.NotEqual(t, http.StatusServiceUnavailable, recorder.Code)
}

// stubSecretReader stands in for the platform vault.
type stubSecretReader struct {
	value string
	err   error
}

func (s stubSecretReader) LookupAdminHiddenSecret(context.Context, string) (string, error) {
	return s.value, s.err
}

// resolvePrebuiltSettings is a no-op for a toolkit type that is not pre-built,
// and for a handler with no catalogue — in both cases without an error, so the
// callers proceed rather than refusing.
func TestResolvePrebuiltSettingsIsANoOpWhenItShouldBe(t *testing.T) {
	settings := map[string]any{"url": ""}

	resolved, err := NewHandler(nil).resolvePrebuiltSettings(
		context.Background(), settings, "mcp_github_copilot")
	require.NoError(t, err)
	require.Equal(t, settings, resolved, "no catalogue configured")

	handler := NewHandler(nil, WithPrebuiltMCPCatalogue(
		mcpregistry.NewPrebuiltStore(nil), stubSecretReader{value: "s"}))
	resolved, err = handler.resolvePrebuiltSettings(context.Background(), settings, "sharepoint")
	require.NoError(t, err)
	require.Equal(t, settings, resolved, "not a pre-built toolkit type")
}

// A catalogue read failure is reported, never folded into "no entry".
func TestResolvePrebuiltSettingsReportsAReadFailure(t *testing.T) {
	handler := NewHandler(nil, WithPrebuiltMCPCatalogue(
		mcpregistry.NewPrebuiltStore(nil), stubSecretReader{value: "s"}))

	_, err := handler.resolvePrebuiltSettings(
		context.Background(), map[string]any{}, "mcp_github_copilot")

	require.Error(t, err)
	require.True(t, errors.Is(err, mcpregistry.ErrNoPool))
}

// prebuiltURLFor answers "" without an error when there is no catalogue and
// when the toolkit is not pre-built, and reports a read failure otherwise.
func TestPrebuiltURLForSeparatesAbsenceFromFailure(t *testing.T) {
	url, err := NewHandler(nil).prebuiltURLFor(context.Background(), "mcp_github_copilot")
	require.NoError(t, err)
	require.Empty(t, url)

	handler := NewHandler(nil, WithPrebuiltMCPCatalogue(
		mcpregistry.NewPrebuiltStore(nil), stubSecretReader{value: "s"}))

	url, err = handler.prebuiltURLFor(context.Background(), "sharepoint")
	require.NoError(t, err)
	require.Empty(t, url)

	_, err = handler.prebuiltURLFor(context.Background(), "mcp_github_copilot")
	require.Error(t, err)
}

// The settings-map helpers must not turn a wrong type into a zero value.
func TestSettingsHelpersKeepTheFallbackOnAWrongType(t *testing.T) {
	require.Equal(t, "fallback", stringSetting(map[string]any{"url": 42}, "url", "fallback"))
	require.Equal(t, "fallback", stringSetting(map[string]any{"url": ""}, "url", "fallback"))
	require.Equal(t, 7, intSetting(map[string]any{"timeout": "30"}, "timeout", 7))
	// JSON numbers arrive as float64; a non-integral one is ignored rather than
	// truncated, because a 1.5-second timeout is not a 1-second one.
	require.Equal(t, 7, intSetting(map[string]any{"timeout": 1.5}, "timeout", 7))
	require.Equal(t, 30, intSetting(map[string]any{"timeout": float64(30)}, "timeout", 7))

	fallback := map[string]string{"X-Kept": "yes"}
	// A header whose value is not a string is dropped rather than formatted:
	// rendering a number into an HTTP header would send one the operator never
	// wrote. With every entry dropped, the fallback stands.
	require.Equal(t, fallback, headerSettings(map[string]any{"headers": map[string]any{"X": 1}}, fallback))
	require.Equal(t, map[string]string{"X": "1"},
		headerSettings(map[string]any{"headers": map[string]any{"X": "1"}}, fallback))
	require.Equal(t, fallback, headerSettings(map[string]any{}, fallback))
}
