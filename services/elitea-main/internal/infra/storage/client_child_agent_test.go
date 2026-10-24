package storage

import (
	"testing"

	"github.com/stretchr/testify/require"

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
