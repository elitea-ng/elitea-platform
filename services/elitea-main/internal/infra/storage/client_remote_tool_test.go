package storage

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/guardrails"
)

type clientPolicyStub struct{ policy guardrails.Policy }

func (stub clientPolicyStub) ResolveCurrentAgentGuardrails(context.Context) (guardrails.Policy, error) {
	return stub.policy, nil
}

// The nested agent (3, 4) holds toolkit 9; the parent (1, 2) holds toolkit 7
// and the child.
const clientChildVersionDetails = `{
  "id": 4, "application_id": 3, "name": "Child", "status": "all", "agent_type": "agent",
  "instructions": "", "welcome_message": "", "llm_settings": {"model_name": "model"},
  "meta": {}, "conversation_starters": [], "pipeline_settings": {}, "author_id": 11,
  "tools": [
    {"id": 9, "type": "jira", "name": "jr", "toolkit_name": "jr", "description": "Jira",
     "author_id": 11, "settings": {"selected_tools": ["read_file"]},
     "meta": {}, "created_at": "2026-01-01T00:00:00Z",
     "author": null, "agent_type": null, "online": null, "icon_meta": null,
     "variables": [], "is_pinned": false, "indexes_count": null}
  ],
  "skills": [], "tags": [], "variables": []
}`

func clientRemoteToolService(t *testing.T, policy guardrails.Policy) *ClientApplicationVersionService {
	t.Helper()
	freezer, err := agentexecutionapp.NewCurrentApplicationToolSnapshotService(
		nestedVersionToolkitSettingsStub{result: clientCanarySettings()},
		nestedVersionToolkitNameStub{result: "gh"},
		&nestedVersionModelCatalogStub{},
		clientPolicyStub{policy: policy}, nestedVersionProjectContextStub{},
		1,
	)
	require.NoError(t, err)
	source := currentApplicationVersionSourceFunc(func(
		_ context.Context, _ int64, applicationID int64, versionID int64,
	) (CurrentApplicationVersionRecord, error) {
		switch {
		case applicationID == 1 && versionID == 2:
			return CurrentApplicationVersionRecord{ApplicationID: 1, VersionID: 2, VersionDetails: json.RawMessage(clientVersionStoredDetails)}, nil
		case applicationID == 3 && versionID == 4:
			return CurrentApplicationVersionRecord{ApplicationID: 3, VersionID: 4, VersionDetails: json.RawMessage(clientChildVersionDetails)}, nil
		}
		return CurrentApplicationVersionRecord{}, ErrContentNotFound
	})
	service, err := NewClientApplicationVersionService(source, freezer)
	require.NoError(t, err)
	return service.WithGuardrails(clientPolicyStub{policy: policy})
}

func clientRemoteToolRequest(mutate func(*RemoteToolAuthorization)) RemoteToolAuthorization {
	request := RemoteToolAuthorization{
		ProjectID: 90106, ActorID: 11, TurnApplicationID: 1, TurnVersionID: 2,
		ApplicationID: 1, VersionID: 2, ToolkitID: 7, ToolName: "read_file",
		ToolkitRef: ClientToolkitRef(ClientVersionIdentity{ProjectID: 90106, ApplicationID: 1, VersionID: 2}, 7, "github"),
	}
	if mutate != nil {
		mutate(&request)
	}
	return request
}

// A remote call has the authority of a chat turn of the turn's agent: one of
// its toolkits, one of that toolkit's selected tools, nothing the guardrails
// block, through the reference the resolved version handed out.
func TestAuthorizeRemoteToolHasExactlyTheTurnsAuthority(t *testing.T) {
	t.Parallel()
	service := clientRemoteToolService(t, guardrails.Policy{})

	grant, err := service.AuthorizeRemoteTool(t.Context(), clientRemoteToolRequest(nil))
	require.NoError(t, err)
	require.Equal(t, "github", grant.ToolkitType)
	require.Equal(t, "gh", grant.ToolkitName)
	require.Nil(t, grant.Sensitive)
	// The model is the version's, as the freeze resolved it.
	require.NotEmpty(t, grant.LLMModel)

	for name, mutate := range map[string]func(*RemoteToolAuthorization){
		"a tool outside selected_tools": func(r *RemoteToolAuthorization) { r.ToolName = "create_file" },
		"a toolkit the agent does not have": func(r *RemoteToolAuthorization) {
			r.ToolkitID = 99
			r.ToolkitRef = ClientToolkitRef(ClientVersionIdentity{ProjectID: 90106, ApplicationID: 1, VersionID: 2}, 99, "github")
		},
		"a version the turn does not run": func(r *RemoteToolAuthorization) { r.TurnApplicationID, r.TurnVersionID = 5, 6 },
		"a model turn":                    func(r *RemoteToolAuthorization) { r.TurnApplicationID, r.TurnVersionID = 0, 0 },
		"a nested agent's toolkit named under the parent": func(r *RemoteToolAuthorization) {
			r.ToolkitID = 9
			r.ToolkitRef = ClientToolkitRef(ClientVersionIdentity{ProjectID: 90106, ApplicationID: 1, VersionID: 2}, 9, "jira")
		},
	} {
		_, err := service.AuthorizeRemoteTool(t.Context(), clientRemoteToolRequest(mutate))
		require.ErrorIs(t, err, ErrRemoteToolNotInAgent, name)
	}

	_, err = service.AuthorizeRemoteTool(t.Context(), clientRemoteToolRequest(func(r *RemoteToolAuthorization) {
		r.ToolkitRef = ClientToolkitRef(ClientVersionIdentity{ProjectID: 90106, ApplicationID: 3, VersionID: 4}, 7, "github")
	}))
	require.ErrorIs(t, err, ErrRemoteToolkitRefMismatch, "a reference from another version")
	_, err = service.AuthorizeRemoteTool(t.Context(), clientRemoteToolRequest(func(r *RemoteToolAuthorization) { r.ToolkitRef = "" }))
	require.ErrorIs(t, err, ErrRemoteToolkitRefMismatch)

	// A nested agent reachable from the turn's agent, named as the running
	// version, reaches ITS toolkits.
	grant, err = service.AuthorizeRemoteTool(t.Context(), clientRemoteToolRequest(func(r *RemoteToolAuthorization) {
		r.ApplicationID, r.VersionID, r.ToolkitID = 3, 4, 9
		r.ToolkitRef = ClientToolkitRef(ClientVersionIdentity{ProjectID: 90106, ApplicationID: 3, VersionID: 4}, 9, "jira")
	}))
	require.NoError(t, err)
	require.Equal(t, "jira", grant.ToolkitType)
}

func TestAuthorizeRemoteToolRefusesWhatTheFreezeDrops(t *testing.T) {
	t.Parallel()
	for name, policy := range map[string]guardrails.Policy{
		"blocked toolkit": guardrails.NewPolicy(guardrails.PolicyInput{BlockedToolkits: []string{"github"}}),
		"blocked tool":    guardrails.NewPolicy(guardrails.PolicyInput{BlockedTools: map[string][]string{"github": {"read_file"}}}),
	} {
		_, err := clientRemoteToolService(t, policy).AuthorizeRemoteTool(t.Context(), clientRemoteToolRequest(nil))
		require.ErrorIs(t, err, ErrRemoteToolBlocked, name)
	}
}

// The freeze strips a blocked name out of selected_tools; a selection emptied
// that way must not read as "every tool".
func TestAuthorizeRemoteToolKeepsTheSavedSelectionWhenTheFreezeEmptiesIt(t *testing.T) {
	t.Parallel()
	policy := guardrails.NewPolicy(guardrails.PolicyInput{BlockedTools: map[string][]string{"github": {"read_file"}}})
	_, err := clientRemoteToolService(t, policy).AuthorizeRemoteTool(t.Context(),
		clientRemoteToolRequest(func(r *RemoteToolAuthorization) { r.ToolName = "delete_repository" }))
	require.ErrorIs(t, err, ErrRemoteToolNotInAgent)
}

// selectionService resolves version (1, 2) whose github toolkit (7) is saved
// with savedSelection and whose settings resolve with frozenSettings'
// selected_tools, under policy.
func selectionService(t *testing.T, policy guardrails.Policy, savedSelection string, frozenSelection []any) *ClientApplicationVersionService {
	t.Helper()
	settings := clientCanarySettings()
	if frozenSelection == nil {
		delete(settings, "selected_tools")
	} else {
		settings["selected_tools"] = frozenSelection
	}
	freezer, err := agentexecutionapp.NewCurrentApplicationToolSnapshotService(
		nestedVersionToolkitSettingsStub{result: settings}, nestedVersionToolkitNameStub{result: "gh"},
		&nestedVersionModelCatalogStub{}, clientPolicyStub{policy: policy}, nestedVersionProjectContextStub{}, 1,
	)
	require.NoError(t, err)
	const savedFixture = `"settings": {"selected_tools": ["read_file"]}`
	require.Contains(t, clientVersionStoredDetails, savedFixture)
	details := strings.Replace(clientVersionStoredDetails, savedFixture, `"settings": {"selected_tools": `+savedSelection+`}`, 1)
	service, err := NewClientApplicationVersionService(clientVersionSource(details), freezer)
	require.NoError(t, err)
	return service.WithGuardrails(clientPolicyStub{policy: policy})
}

// resolvedGithubSelection answers toolkit 7's selected_tools and all_tools in
// the desktop's resolved document.
func resolvedGithubSelection(t *testing.T, service *ClientApplicationVersionService) ([]any, any) {
	t.Helper()
	document, err := service.Resolve(t.Context(), 90106, 11, 1, 2)
	require.NoError(t, err)
	var details struct {
		Tools []map[string]any `json:"tools"`
	}
	require.NoError(t, json.Unmarshal(document.VersionDetails, &details))
	for _, tool := range details.Tools {
		if tool["kind"] == ClientToolKindRemoteToolkit && tool["id"] == float64(7) {
			selected, _ := tool["selected_tools"].([]any)
			return selected, tool["all_tools"]
		}
	}
	t.Fatal("the resolved document has no toolkit 7")
	return nil, nil
}

// The desktop's document and the remote call agree on a toolkit's selection:
// both use the frozen selection, and a selection the freeze emptied reads as
// no tool, never as "every tool"; a blocked tool is still refused as blocked.
func TestRemoteToolSelectionIsTheResolvedDocumentsSelection(t *testing.T) {
	t.Parallel()
	authorize := func(service *ClientApplicationVersionService, tool string) error {
		_, err := service.AuthorizeRemoteTool(t.Context(), clientRemoteToolRequest(func(r *RemoteToolAuthorization) { r.ToolName = tool }))
		return err
	}

	// Every selected tool blocked: the freeze empties the list.
	emptied := selectionService(t, guardrails.NewPolicy(guardrails.PolicyInput{BlockedTools: map[string][]string{"github": {"read_file"}}}),
		`["read_file"]`, []any{"read_file"})
	selected, allTools := resolvedGithubSelection(t, emptied)
	require.Empty(t, selected)
	require.Equal(t, false, allTools, "an emptied selection must not read as every tool")
	require.ErrorIs(t, authorize(emptied, "read_file"), ErrRemoteToolBlocked)
	require.ErrorIs(t, authorize(emptied, "delete_repository"), ErrRemoteToolNotInAgent)

	// One of two selected tools blocked.
	partial := selectionService(t, guardrails.NewPolicy(guardrails.PolicyInput{BlockedTools: map[string][]string{"github": {"create_file"}}}),
		`["read_file", "create_file"]`, []any{"read_file", "create_file"})
	selected, allTools = resolvedGithubSelection(t, partial)
	require.Equal(t, []any{"read_file"}, selected)
	require.Equal(t, false, allTools)
	require.NoError(t, authorize(partial, "read_file"))
	require.ErrorIs(t, authorize(partial, "create_file"), ErrRemoteToolBlocked)
	require.ErrorIs(t, authorize(partial, "delete_repository"), ErrRemoteToolNotInAgent)

	// Nothing selected by the author: every tool, as the SDK reads it, except
	// what the guardrails block.
	every := selectionService(t, guardrails.NewPolicy(guardrails.PolicyInput{BlockedTools: map[string][]string{"github": {"create_file"}}}),
		`[]`, nil)
	selected, allTools = resolvedGithubSelection(t, every)
	require.Empty(t, selected)
	require.Equal(t, true, allTools)
	require.NoError(t, authorize(every, "delete_repository"))
	require.ErrorIs(t, authorize(every, "create_file"), ErrRemoteToolBlocked)
}

func TestAuthorizeRemoteToolMarksASensitiveTool(t *testing.T) {
	t.Parallel()
	policy := guardrails.NewPolicy(guardrails.PolicyInput{SensitiveTools: map[string][]string{"github": {"read_file"}}})
	grant, err := clientRemoteToolService(t, policy).AuthorizeRemoteTool(t.Context(), clientRemoteToolRequest(nil))
	require.NoError(t, err)
	require.NotNil(t, grant.Sensitive)
	require.Equal(t, "gh.read_file", grant.Sensitive.ActionLabel)
	require.Contains(t, grant.Sensitive.PolicyMessage, "gh.read_file")
}

func TestAuthorizeRemoteToolNeedsThePolicy(t *testing.T) {
	t.Parallel()
	service, err := NewClientApplicationVersionService(clientVersionSource(clientVersionStoredDetails), newClientVersionFreezer(t))
	require.NoError(t, err)
	_, err = service.AuthorizeRemoteTool(t.Context(), clientRemoteToolRequest(nil))
	require.ErrorIs(t, err, ErrContentUnavailable)
}

func TestFrozenRunModelKeepsOnlyTheRunsModelSettings(t *testing.T) {
	t.Parallel()
	model, settings := frozenRunModel(json.RawMessage(
		`{"llm_settings":{"model_name":"gpt-x","model_project_id":1,"temperature":0.3,"max_tokens":512,` +
			`"reasoning_effort":"low","openai_compatible":true,"max_tokens_auto":true}}`))
	require.Equal(t, "gpt-x", model)
	require.JSONEq(t, `{"temperature":0.3,"max_tokens":512,"reasoning_effort":"low"}`, string(settings))
	model, settings = frozenRunModel(json.RawMessage(`{"llm_settings":{"model_name":"m","temperature":7}}`))
	require.Equal(t, "m", model)
	require.Nil(t, settings)
}
