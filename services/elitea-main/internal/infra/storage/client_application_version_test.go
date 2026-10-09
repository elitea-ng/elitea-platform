package storage

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
)

// The canary values. Each is something a toolkit or a version can hold that a
// desktop must never receive. The test asserts each one is in the FROZEN
// document first, so its absence from the response is the projection's doing
// and not a fixture that never carried it.
const (
	clientCanaryPlaintext       = "sk-canary-plaintext-6f1d"
	clientCanaryConfigPlaintext = "sk-canary-configuration-91ab"
	clientCanarySecretName      = "canary_toolkit_secret"
	clientCanaryVariableSecret  = "canary_variable_secret"
	clientCanaryInstruction     = "canary_instruction_secret"
)

const clientVersionStoredDetails = `{
  "id": 2,
  "application_id": 1,
  "name": "Desktop",
  "status": "all",
  "agent_type": "agent",
  "instructions": "Use {{secret.` + clientCanaryInstruction + `}} carefully.",
  "welcome_message": "",
  "llm_settings": {"model_name": "model"},
  "meta": {"internal_tools": ["internal_mcp"]},
  "conversation_starters": [],
  "pipeline_settings": {},
  "author_id": 11,
  "tools": [
    {"id": 7, "type": "github", "name": "gh", "toolkit_name": "gh", "description": "GitHub",
     "author_id": 11, "settings": {"selected_tools": ["read_file"]},
     "meta": {"oauth": "` + clientCanaryPlaintext + `"}, "created_at": "2026-01-01T00:00:00Z",
     "author": null, "agent_type": null, "online": null, "icon_meta": null,
     "variables": [], "is_pinned": false, "indexes_count": null},
    {"id": 8, "type": "application", "name": "Child", "toolkit_name": "Child", "description": "child",
     "author_id": 11, "settings": {"application_id": 3, "application_version_id": 4},
     "meta": {}, "created_at": "2026-01-01T00:00:00Z", "author": null, "agent_type": "agent",
     "online": null, "icon_meta": null, "variables": [], "is_pinned": false, "indexes_count": null}
  ],
  "skills": [{"skill_id": 1, "skill_version_id": 2, "name": "s", "description": "d", "version_name": "base", "icon_meta": null, "instructions": "skill text"}],
  "tags": [],
  "variables": [{"name": "token", "value": "{{secret.` + clientCanaryVariableSecret + `}}"}]
}`

// clientCanarySettings is what the shared freeze's settings resolver hands
// back in reference mode: a sealed secret reference, an EXPANDED configuration
// (marker and all) and, deliberately, a plaintext value in a field the
// resolver does not know to be secret.
func clientCanarySettings() map[string]any {
	return map[string]any{
		"selected_tools": []any{"read_file"},
		"repository":     clientCanaryPlaintext,
		"access_token":   "{{secret." + clientCanarySecretName + "}}",
		"github_configuration": map[string]any{
			configurationapp.CurrentFrozenConfigurationMarker: true,
			"configuration_project_id":                        1,
			"token":                                           clientCanaryConfigPlaintext,
		},
	}
}

func newClientVersionFreezer(t *testing.T) agentexecutionapp.CurrentApplicationVersionFreezer {
	t.Helper()
	freezer, err := agentexecutionapp.NewCurrentApplicationToolSnapshotService(
		nestedVersionToolkitSettingsStub{result: clientCanarySettings()},
		nestedVersionToolkitNameStub{result: "gh"},
		&nestedVersionModelCatalogStub{},
		nestedVersionGuardrailStub{}, nestedVersionProjectContextStub{},
		1,
	)
	require.NoError(t, err)
	return freezer
}

func clientVersionSource(details string) CurrentApplicationVersionSource {
	return currentApplicationVersionSourceFunc(func(
		_ context.Context, _ int64, applicationID int64, versionID int64,
	) (CurrentApplicationVersionRecord, error) {
		if applicationID != 1 || versionID != 2 {
			return CurrentApplicationVersionRecord{}, ErrContentNotFound
		}
		return CurrentApplicationVersionRecord{
			ApplicationID: 1, VersionID: 2, VersionDetails: json.RawMessage(details),
		}, nil
	})
}

func TestClientApplicationVersionCarriesNoSecretMaterial(t *testing.T) {
	t.Parallel()
	freezer := newClientVersionFreezer(t)

	// The fixture check: the shared freeze really does put every canary in
	// the document it hands both routes.
	frozen, err := freezer.FreezeCurrentApplicationVersion(t.Context(),
		agentexecutionapp.CurrentApplicationVersionFreezeRequest{
			ProjectID: 90106, ActorUserID: 11, VersionDetails: json.RawMessage(clientVersionStoredDetails),
		})
	require.NoError(t, err)
	for _, canary := range []string{
		clientCanaryPlaintext, clientCanaryConfigPlaintext, clientCanarySecretName,
		clientCanaryVariableSecret, clientCanaryInstruction, configurationapp.CurrentFrozenConfigurationMarker,
	} {
		require.Contains(t, string(frozen), canary, "the fixture must carry %s into the freeze", canary)
	}

	service, err := NewClientApplicationVersionService(clientVersionSource(clientVersionStoredDetails), freezer)
	require.NoError(t, err)
	resolved, err := service.Resolve(t.Context(), 90106, 11, 1, 2)
	require.NoError(t, err)
	encoded, err := json.Marshal(resolved)
	require.NoError(t, err)
	for _, canary := range []string{
		clientCanaryPlaintext, clientCanaryConfigPlaintext, clientCanarySecretName,
		clientCanaryVariableSecret, clientCanaryInstruction, configurationapp.CurrentFrozenConfigurationMarker,
		"{{secret.",
	} {
		require.NotContains(t, string(encoded), canary)
	}

	require.Equal(t, ClientApplicationVersionSchemaVersion, resolved.SchemaVersion)
	require.EqualValues(t, 90106, resolved.ProjectID)
	require.Regexp(t, `^[0-9a-f]{64}$`, resolved.DefinitionSHA256)
	var details map[string]any
	require.NoError(t, json.Unmarshal(resolved.VersionDetails, &details))
	require.Equal(t, "Use [secret withheld] carefully.", details["instructions"])
	// Every replaced reference is named, so the desktop can decide to run
	// this agent in the cloud instead.
	require.Equal(t, []string{"/instructions", "/variables/0/value"}, resolved.WithheldSecrets)
	require.Equal(t, "[secret withheld]", details["variables"].([]any)[0].(map[string]any)["value"])
	require.Len(t, details["skills"], 1, "frozen skills travel")

	tools := details["tools"].([]any)
	kinds := map[string]map[string]any{}
	for _, raw := range tools {
		tool := raw.(map[string]any)
		kinds[tool["kind"].(string)] = tool
		require.NotContains(t, tool, "settings")
		require.NotContains(t, tool, "meta")
	}
	toolkit := kinds[ClientToolKindRemoteToolkit]
	require.NotNil(t, toolkit)
	require.EqualValues(t, 7, toolkit["id"])
	require.Equal(t, []any{"read_file"}, toolkit["selected_tools"])
	ref := toolkit["toolkit_ref"].(map[string]any)
	require.EqualValues(t, 7, ref["toolkit_id"])
	require.EqualValues(t, 90106, ref["project_id"])
	require.Equal(t, ClientToolkitRef(ClientVersionIdentity{ProjectID: 90106, ApplicationID: 1, VersionID: 2}, 7, "github"), ref["ref"])

	child := kinds[ClientToolKindApplication]
	require.NotNil(t, child)
	require.EqualValues(t, 3, child["application_id"])
	require.EqualValues(t, 4, child["application_version_id"])

	platform := kinds[ClientToolKindPlatformMCP]
	require.NotNil(t, platform, "the internal MCP the version selects is named, not materialized")
	require.True(t, strings.HasPrefix(platform["server_name"].(string), "mcp_elitea_internal_"))
}

func TestClientToolkitRefIsStableAndScoped(t *testing.T) {
	t.Parallel()
	at := func(project, application, version int64) ClientVersionIdentity {
		return ClientVersionIdentity{ProjectID: project, ApplicationID: application, VersionID: version}
	}
	ref := ClientToolkitRef(at(1, 2, 3), 7, "github")
	require.Regexp(t, `^tkr1_[0-9a-f]{32}$`, ref)
	require.Equal(t, ref, ClientToolkitRef(at(1, 2, 3), 7, "github"))
	require.NotEqual(t, ref, ClientToolkitRef(at(2, 2, 3), 7, "github"))
	require.NotEqual(t, ref, ClientToolkitRef(at(1, 9, 3), 7, "github"), "the agent is part of the binding")
	require.NotEqual(t, ref, ClientToolkitRef(at(1, 2, 9), 7, "github"), "the version is part of the binding")
	require.NotEqual(t, ref, ClientToolkitRef(at(1, 2, 3), 8, "github"))
	require.NotEqual(t, ref, ClientToolkitRef(at(1, 2, 3), 7, "gitlab"))
}

type clientVersionFailingFreezer struct{ err error }

func (f clientVersionFailingFreezer) FreezeCurrentApplicationVersion(
	context.Context, agentexecutionapp.CurrentApplicationVersionFreezeRequest,
) (json.RawMessage, error) {
	return nil, f.err
}

func TestClientApplicationVersionErrorTaxonomy(t *testing.T) {
	t.Parallel()
	source := clientVersionSource(clientVersionStoredDetails)

	service, err := NewClientApplicationVersionService(source, newClientVersionFreezer(t))
	require.NoError(t, err)
	_, err = service.Resolve(t.Context(), 90106, 11, 1, 99)
	require.ErrorIs(t, err, ErrContentNotFound)
	_, err = service.Resolve(t.Context(), 90106, 11, 0, 2)
	require.ErrorIs(t, err, ErrContentNotFound)

	refused := agentexecutionapp.UnsupportedCurrentAgentStart("a tool entry has no settings object")
	service, err = NewClientApplicationVersionService(source, clientVersionFailingFreezer{err: refused})
	require.NoError(t, err)
	_, err = service.Resolve(t.Context(), 90106, 11, 1, 2)
	require.ErrorIs(t, err, ErrClientApplicationVersionUnresolvable)

	// A refusal whose cause is a dependency fault is a 503, not a 422: the
	// version is fine, the vault or the database is not.
	dependency := errors.Join(agentexecutionapp.ErrUnsupportedCurrentAgentStart, configurationapp.ErrCurrentToolkitSettingsDependency)
	service, err = NewClientApplicationVersionService(source, clientVersionFailingFreezer{err: dependency})
	require.NoError(t, err)
	_, err = service.Resolve(t.Context(), 90106, 11, 1, 2)
	require.ErrorIs(t, err, ErrContentUnavailable)

	service, err = NewClientApplicationVersionService(source, clientVersionFailingFreezer{err: errors.New("catalogue down")})
	require.NoError(t, err)
	_, err = service.Resolve(t.Context(), 90106, 11, 1, 2)
	require.ErrorIs(t, err, ErrContentUnavailable)

	_, err = NewClientApplicationVersionService(nil, newClientVersionFreezer(t))
	require.Error(t, err)
	_, err = NewClientApplicationVersionService(source, nil)
	require.Error(t, err)
}

func TestProjectClientApplicationVersionRefusesAToolItCannotName(t *testing.T) {
	t.Parallel()
	identity := ClientVersionIdentity{ProjectID: 1, ApplicationID: 2, VersionID: 3}
	_, _, err := ProjectClientApplicationVersion(identity, json.RawMessage(`{"tools":[{"type":"github","settings":{}}]}`))
	require.Error(t, err, "a toolkit with no id has no reference to give")
	_, _, err = ProjectClientApplicationVersion(identity, json.RawMessage(`{"tools":{}}`))
	require.Error(t, err)
	_, _, err = ProjectClientApplicationVersion(ClientVersionIdentity{}, json.RawMessage(`{"tools":[]}`))
	require.Error(t, err)
}

func TestProjectClientApplicationVersionNamesEveryWithheldSecret(t *testing.T) {
	t.Parallel()
	_, withheld, err := ProjectClientApplicationVersion(ClientVersionIdentity{ProjectID: 1, ApplicationID: 2, VersionID: 3},
		json.RawMessage(`{"tools":[],"instructions":"plain","meta":{"a/b":"{{ secret.x }}","list":["{{secret.y}}","ok"]}}`))
	require.NoError(t, err)
	require.Equal(t, []string{"/meta/a~1b", "/meta/list/0"}, withheld)
	_, withheld, err = ProjectClientApplicationVersion(ClientVersionIdentity{ProjectID: 1, ApplicationID: 2, VersionID: 3},
		json.RawMessage(`{"tools":[]}`))
	require.NoError(t, err)
	require.NotNil(t, withheld, "never null on the wire")
	require.Empty(t, withheld)
}
