package storage

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/guardrails"
)

// Issue 1239: a local turn runs only its own agent's direct nested agents as
// cloud children; nothing else, and no grandchild.
func TestAuthorizeChildAgentAcceptsOnlyTheTurnsOwnNestedAgents(t *testing.T) {
	t.Parallel()
	service := clientRemoteToolService(t, guardrails.Policy{})
	request := func(mutate func(*ChildAgentAuthorization)) ChildAgentAuthorization {
		value := ChildAgentAuthorization{
			ProjectID: 90106, ActorID: 11, TurnApplicationID: 1, TurnVersionID: 2,
			ApplicationID: 3, VersionID: 4,
		}
		if mutate != nil {
			mutate(&value)
		}
		return value
	}

	grant, err := service.AuthorizeChildAgent(t.Context(), request(nil))
	require.NoError(t, err)
	require.Equal(t, "Child", grant.Name)
	require.Equal(t, "agent", grant.AgentType)

	for name, mutate := range map[string]func(*ChildAgentAuthorization){
		"the turn's own agent":         func(r *ChildAgentAuthorization) { r.ApplicationID, r.VersionID = 1, 2 },
		"another version of the child": func(r *ChildAgentAuthorization) { r.VersionID = 5 },
		"an agent the turn does not run": func(r *ChildAgentAuthorization) {
			r.TurnApplicationID, r.TurnVersionID = 3, 4
		},
		"a model turn":  func(r *ChildAgentAuthorization) { r.TurnApplicationID, r.TurnVersionID = 0, 0 },
		"no actor":      func(r *ChildAgentAuthorization) { r.ActorID = 0 },
		"no such agent": func(r *ChildAgentAuthorization) { r.TurnApplicationID, r.TurnVersionID = 50, 60 },
	} {
		_, err := service.AuthorizeChildAgent(t.Context(), request(mutate))
		require.ErrorIs(t, err, ErrChildAgentNotInAgent, name)
	}
}

// ADR-0050 decision 7f (issue 1806): a turn that answers for a cloud agent
// of a desktop thread relays that agent, and only that agent, to the cloud.
func TestAuthorizeChildAgentRelaysOnlyACloudAgentsOwnVersion(t *testing.T) {
	t.Parallel()
	service := clientRemoteToolService(t, guardrails.Policy{})
	own := ChildAgentAuthorization{
		ProjectID: 90106, ActorID: 11, TurnApplicationID: 1, TurnVersionID: 2,
		ApplicationID: 1, VersionID: 2, TurnIsCloudAgent: true,
	}
	_, err := service.AuthorizeChildAgent(t.Context(), own)
	require.NoError(t, err)

	nested := own
	nested.ApplicationID, nested.VersionID = 3, 4
	_, err = service.AuthorizeChildAgent(t.Context(), nested)
	require.ErrorIs(t, err, ErrChildAgentNotInAgent, "a cloud agent's turn relays no nested agent")

	local := own
	local.TurnIsCloudAgent = false
	_, err = service.AuthorizeChildAgent(t.Context(), local)
	require.ErrorIs(t, err, ErrChildAgentNotInAgent, "a local turn does not run its own agent as a child")
}

// A pipeline never runs as the child of a local turn, even one the turn's
// agent nests: the desktop refuses it, and so does the server.
func TestAuthorizeChildAgentRefusesANestedPipeline(t *testing.T) {
	t.Parallel()
	freezer, err := agentexecutionapp.NewCurrentApplicationToolSnapshotService(
		nestedVersionToolkitSettingsStub{result: clientCanarySettings()},
		nestedVersionToolkitNameStub{result: "gh"},
		&nestedVersionModelCatalogStub{},
		clientPolicyStub{}, nestedVersionProjectContextStub{},
		1,
	)
	require.NoError(t, err)
	pipeline := strings.Replace(clientChildVersionDetails, `"agent_type": "agent"`, `"agent_type": "pipeline"`, 1)
	require.NotEqual(t, clientChildVersionDetails, pipeline)
	source := currentApplicationVersionSourceFunc(func(
		_ context.Context, _ int64, applicationID int64, versionID int64,
	) (CurrentApplicationVersionRecord, error) {
		switch {
		case applicationID == 1 && versionID == 2:
			return CurrentApplicationVersionRecord{ApplicationID: 1, VersionID: 2, VersionDetails: json.RawMessage(clientVersionStoredDetails)}, nil
		case applicationID == 3 && versionID == 4:
			return CurrentApplicationVersionRecord{ApplicationID: 3, VersionID: 4, VersionDetails: json.RawMessage(pipeline)}, nil
		}
		return CurrentApplicationVersionRecord{}, ErrContentNotFound
	})
	service, err := NewClientApplicationVersionService(source, freezer)
	require.NoError(t, err)

	_, err = service.WithGuardrails(clientPolicyStub{policy: guardrails.Policy{}}).AuthorizeChildAgent(t.Context(), ChildAgentAuthorization{
		ProjectID: 90106, ActorID: 11, TurnApplicationID: 1, TurnVersionID: 2, ApplicationID: 3, VersionID: 4,
	})
	require.ErrorIs(t, err, ErrChildAgentPipeline)
}
