package agentexecution

import (
	"context"
	"encoding/json"
	"errors"
	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"testing"
)

type currentProjectContextStub struct {
	source             CurrentAgentProjectContext
	err                error
	projectID, actorID int32
	calls              int
}

func (s *currentProjectContextStub) ResolveCurrentAgentProjectContext(_ context.Context, projectID, actorID int32) (CurrentAgentProjectContext, error) {
	s.projectID, s.actorID = projectID, actorID
	s.calls++
	return s.source, s.err
}
func TestCurrentInstructionSnapshotFreezesSourceAndPreservesSkillProjection(t *testing.T) {
	reader := &currentProjectContextStub{source: CurrentAgentProjectContext{ID: 9, Enabled: true, Content: " Keep exact whitespace \n", ActivationDescription: " apply when coding "}}
	service, err := NewCurrentApplicationToolSnapshotService(&currentAgentSettingsResolverStub{}, &currentAgentNameResolverStub{}, currentAgentModelCatalogForTest(true), &currentAgentGuardrailStub{}, reader, 1)
	require.NoError(t, err)
	frozen, err := service.FreezeCurrentApplicationVersion(context.Background(), CurrentApplicationVersionFreezeRequest{ProjectID: 7, ActorUserID: 11, VersionDetails: json.RawMessage(`{"agent_type":"agent","llm_settings":{"model_name":"model"},"tools":[],"project_context":{"content":"forged"},"skills":[{"skill_id":3,"skill_version_id":4,"name":"Build","instructions":"Use tests"}]}`)})
	require.NoError(t, err)
	require.Equal(t, int32(7), reader.projectID)
	require.Equal(t, int32(11), reader.actorID)
	version, err := decodeCurrentApplicationVersion(frozen)
	require.NoError(t, err)
	snapshot, err := currentFrozenProjectContext(version)
	require.NoError(t, err)
	require.Equal(t, "project-context:7:9", snapshot.Id)
	require.Equal(t, "project:7", snapshot.Scope)
	require.Equal(t, reader.source.Content, snapshot.Content)
	require.Equal(t, currentInstructionRevision(snapshot.Content), snapshot.Revision)
	require.Equal(t, "apply when coding", snapshot.ActivationDescription)
	reader.source.Content = "changed after admission"
	require.NotEqual(t, reader.source.Content, snapshot.Content)
	projection, err := projectCurrentApplicationSkills("~Build go", frozen)
	require.NoError(t, err)
	for _, raw := range []json.RawMessage{projection.attached, projection.invoked, projection.applied} {
		var skills []map[string]any
		require.NoError(t, json.Unmarshal(raw, &skills))
		require.Len(t, skills, 1)
		require.Equal(t, "skill:3:version:4", skills[0]["id"])
		require.Equal(t, currentInstructionRevision("Use tests"), skills[0]["revision"])
		require.Equal(t, "project:7", skills[0]["scope"])
	}
}
func TestCurrentInstructionSnapshotDeliveryModesAndFailure(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version string
		source  CurrentAgentProjectContext
		err     error
		want    bool
		calls   int
	}{
		{name: "eager", version: `{}`, source: CurrentAgentProjectContext{ID: 1, Enabled: true, Content: "eager"}, want: true, calls: 1},
		{name: "disabled", version: `{}`, source: CurrentAgentProjectContext{ID: 1, Content: "disabled"}, calls: 1},
		{name: "empty", version: `{}`, source: CurrentAgentProjectContext{ID: 1, Enabled: true}, calls: 1},
		{name: "pipeline", version: `{"agent_type":"pipeline"}`},
		{name: "ignore", version: `{"meta":{"ignore_project_context":true}}`},
		{name: "read fails", version: `{}`, err: errors.New("database unavailable"), calls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := &currentProjectContextStub{source: tc.source, err: tc.err}
			service := &CurrentApplicationToolSnapshotService{projectContext: reader}
			version, err := decodeCurrentApplicationVersion([]byte(tc.version))
			require.NoError(t, err)
			version["project_context"] = map[string]any{"content": "forged"}
			err = service.freezeCurrentInstructionSnapshots(context.Background(), CurrentApplicationVersionFreezeRequest{ProjectID: 7, ActorUserID: 11}, version)
			if tc.err != nil {
				require.ErrorIs(t, err, ErrUnsupportedCurrentAgentStart)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tc.want, version["project_context"] != nil)
			require.Equal(t, tc.calls, reader.calls)
		})
	}
	require.False(t, validCurrentProjectContextSnapshot(&runtimev1.ProjectContextSnapshotV1{Id: "id", Scope: "project:7", Content: "changed", Revision: currentInstructionRevision("old")}))
}

func TestCurrentInstructionSnapshotReachesApplicationAndAdhocBundles(t *testing.T) {
	reader := &currentProjectContextStub{source: CurrentAgentProjectContext{ID: 9, Enabled: true, Content: "Project rules"}}
	freezer, err := NewCurrentApplicationToolSnapshotService(&currentAgentSettingsResolverStub{}, &currentAgentNameResolverStub{}, currentAgentModelCatalogForTest(true), &currentAgentGuardrailStub{}, reader, 1)
	require.NoError(t, err)
	frozen, err := freezer.FreezeCurrentApplicationVersion(t.Context(), CurrentApplicationVersionFreezeRequest{ProjectID: 7, ActorUserID: 11, VersionDetails: json.RawMessage(`{"agent_type":"agent","llm_settings":{"model_name":"model"},"tools":[]}`)})
	require.NoError(t, err)
	app, err := currentApplicationInput(validCurrentApplicationStartRequest(), CurrentApplicationTarget{ApplicationID: 31, ApplicationVersionID: 41, Variables: json.RawMessage(`[]`), VersionDetails: frozen, ChatHistory: json.RawMessage(`[]`), InternalTools: json.RawMessage(`[]`)}, nil, nil, nil, "", "must not bypass activation")
	require.NoError(t, err)
	adhoc, err := currentAdhocInput(validCurrentAdhocStartRequest(), CurrentAdhocTarget{Instructions: "Adhoc", ChatHistory: json.RawMessage(`[]`), ConversationMeta: json.RawMessage(`{}`)}, frozen, nil, nil, nil, "", "must not bypass activation")
	require.NoError(t, err)
	for _, input := range []*runtimev1.AgentExecutionInputV1{app, adhoc} {
		require.Equal(t, "Project rules", input.GetProjectContext().GetContent())
		bundle, _, err := testInputBundleFactory(t).Build(t.Context(), input, "conversation", "message", "chat_predict")
		require.NoError(t, err)
		var restored runtimev1.AgentExecutionInputV1
		require.NoError(t, proto.Unmarshal(bundle.Entries[0].Content, &restored))
		require.True(t, proto.Equal(input.ProjectContext, restored.ProjectContext))
		input.ProjectContext.Content = "tampered"
		_, _, err = testInputBundleFactory(t).Build(t.Context(), input, "conversation", "message", "chat_predict")
		require.ErrorIs(t, err, ErrInvalidAuthoritativeAgentInput)
	}
}

func TestCurrentInstructionSnapshotsRequireSource(t *testing.T) {
	service, err := NewCurrentApplicationToolSnapshotService(&currentAgentSettingsResolverStub{}, &currentAgentNameResolverStub{}, currentAgentModelCatalogForTest(true), &currentAgentGuardrailStub{}, nil, 1)
	require.Error(t, err)
	require.Nil(t, service)
}
