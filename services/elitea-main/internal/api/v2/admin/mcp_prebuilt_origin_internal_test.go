package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/mcpregistry"
)

// The client secret is write-only, so a save that moves the server, base or
// token URL to another origin must re-enter it (issue 2944). Otherwise a caller
// who may edit the catalogue but was never shown the secret could send it to a
// host it runs. The refusal comes before the vault or the row is touched; the
// same save with the secret typed again, or cleared, lands.

func securedEntry() mcpregistry.PrebuiltServer {
	return mcpregistry.PrebuiltServer{
		Key: "ado", DisplayName: "ADO", ServerURL: "https://a.example/mcp",
		BaseURL: "https://a.example", TokenURL: "https://idp.example/token",
		ClientID: "client", ClientSecretRef: prebuiltSecretName("ado"),
	}
}

func TestPrebuiltMCPSaveRefusesToMoveAKeptSecretToAnotherOrigin(t *testing.T) {
	for name, body := range map[string]string{
		"url":      `{"display_name":"ADO","url":"https://evil.example/mcp","base_url":"https://a.example"}`,
		"base_url": `{"display_name":"ADO","url":"https://a.example/mcp","base_url":"https://evil.example"}`,
		"token_url": `{"display_name":"ADO","url":"https://a.example/mcp","base_url":"https://a.example",` +
			`"token_url":"https://evil.example/token"}`,
		"port":             `{"display_name":"ADO","url":"https://a.example:8443/mcp","base_url":"https://a.example"}`,
		"base_url removed": `{"display_name":"ADO","url":"https://a.example/mcp","base_url":""}`,
	} {
		stored := securedEntry()
		catalogue := &fakeCatalogue{entries: map[string]mcpregistry.PrebuiltServer{"ado": stored}}
		vault := newRecordingVault()
		recorder := httptest.NewRecorder()
		fakeHandler(catalogue, vault).PrebuiltMCPSave(recorder, request(http.MethodPut, "ado", body))
		require.Equal(t, http.StatusBadRequest, recorder.Code, "%s: %s", name, recorder.Body.String())
		var got struct{ Error, Field string }
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &got))
		require.Equal(t, "client_secret", got.Field, name)
		require.Contains(t, got.Error, "re-enter the client secret", name)
		require.Equal(t, stored, catalogue.entries["ado"], "%s: a refused save changed the entry", name)
		require.Empty(t, vault.stored, name)
		require.Empty(t, vault.deleted, name)
	}
}

func TestPrebuiltMCPSaveMovesWithTheSecretReenteredOrCleared(t *testing.T) {
	for _, secret := range []string{`"s3cret-2"`, `""`} {
		catalogue := &fakeCatalogue{entries: map[string]mcpregistry.PrebuiltServer{"ado": securedEntry()}}
		recorder := httptest.NewRecorder()
		fakeHandler(catalogue, newRecordingVault()).PrebuiltMCPSave(recorder, request(http.MethodPut, "ado",
			`{"display_name":"ADO","url":"https://b.example/mcp","base_url":"https://a.example","client_secret":`+
				secret+`}`))
		require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
		require.Equal(t, "https://b.example/mcp", catalogue.entries["ado"].ServerURL)
	}
}

// The common edit (a path, the display name, the timeout, a host spelled in
// another case) keeps the secret without asking for it again.
func TestPrebuiltMCPSaveKeepsTheSecretWhenTheOriginStays(t *testing.T) {
	stored := securedEntry()
	catalogue := &fakeCatalogue{entries: map[string]mcpregistry.PrebuiltServer{"ado": stored}}
	vault := newRecordingVault()
	recorder := httptest.NewRecorder()
	fakeHandler(catalogue, vault).PrebuiltMCPSave(recorder, request(http.MethodPut, "ado",
		`{"display_name":"ADO renamed","url":"https://A.example/v2/mcp","base_url":"https://a.example/root",`+
			`"timeout":30}`))
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.Equal(t, stored.ClientSecretRef, catalogue.entries["ado"].ClientSecretRef)
	require.Equal(t, "https://A.example/v2/mcp", catalogue.entries["ado"].ServerURL)
	require.Empty(t, vault.stored)
	require.Empty(t, vault.deleted)
}

func TestPrebuiltMCPSaveWithNoSecretHasNothingToReenter(t *testing.T) {
	plain := mcpregistry.PrebuiltServer{Key: "plain", DisplayName: "Plain", ServerURL: "https://a.example/mcp"}
	catalogue := &fakeCatalogue{entries: map[string]mcpregistry.PrebuiltServer{"plain": plain}}
	recorder := httptest.NewRecorder()
	fakeHandler(catalogue, newRecordingVault()).PrebuiltMCPSave(recorder, request(http.MethodPut, "plain",
		`{"display_name":"Plain","url":"https://elsewhere.example/mcp"}`))
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
}

func TestPrebuiltOriginComparesSchemeAndHostOnly(t *testing.T) {
	require.Equal(t, prebuiltOrigin("https://A.Example/x"), prebuiltOrigin(" https://a.example/y?z=1 "))
	require.NotEqual(t, prebuiltOrigin("https://a.example"), prebuiltOrigin("https://a.example:444"))
	// The default port written out, and a trailing dot, are the same origin.
	require.Equal(t, prebuiltOrigin("https://a.example/mcp"), prebuiltOrigin("https://a.example:443/mcp"))
	require.Equal(t, prebuiltOrigin("http://a.example"), prebuiltOrigin("http://a.example.:80"))
	require.NotEqual(t, prebuiltOrigin("http://a.example"), prebuiltOrigin("http://a.example:443"))
	require.NotEqual(t, prebuiltOrigin("https://a.example"), prebuiltOrigin("http://a.example"))
	require.NotEqual(t, prebuiltOrigin(""), prebuiltOrigin("https://a.example"))
	// A value with no readable host is compared whole, so any edit is a move.
	require.NotEqual(t, prebuiltOrigin("https://{org}.example/mcp"), prebuiltOrigin("https://{org}.example2/mcp"))
}
