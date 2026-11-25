package handoffcontinue

import (
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/turntaking"
)

func TestRouteThreadFollowsTheFloor(t *testing.T) {
	model := ThreadCandidate{ParticipantID: 10}
	coder := ThreadCandidate{ParticipantID: 11, Agent: true, VersionID: 31}
	reviewer := ThreadCandidate{ParticipantID: 12, Agent: true, VersionID: 41}
	team := []ThreadCandidate{model, coder, reviewer}
	lead := turntaking.Settings{Policy: turntaking.PolicyLead, Lead: reviewer.ParticipantID}
	gone := turntaking.Settings{Policy: turntaking.PolicyLead, Lead: 99}

	for _, tc := range []struct {
		name       string
		settings   turntaking.Settings
		sticky     int64
		candidates []ThreadCandidate
		want       ThreadCandidate
	}{
		{"a team thread's lead answers, not the last local turn's agent", lead, coder.ParticipantID, team, reviewer},
		{"the lead answers a thread with no local turn", lead, 0, team, reviewer},
		{"addressed: the last local turn's agent answers", turntaking.Default, reviewer.ParticipantID, team, reviewer},
		{"addressed with no local turn: the first agent", turntaking.Default, 0, team, coder},
		{"a departed lead: the last local turn's agent", gone, reviewer.ParticipantID, team, reviewer},
		{"one agent: the last local turn's participant, as before", lead, coder.ParticipantID, []ThreadCandidate{model, coder}, coder},
		{"one agent and no local turn: the agent", turntaking.Default, 0, []ThreadCandidate{model, coder}, coder},
		{"no agent: the model", turntaking.Default, 0, []ThreadCandidate{model}, model},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := RouteThread(tc.settings, tc.sticky, tc.candidates)
			if !ok || got != tc.want {
				t.Fatalf("RouteThread = %+v, %v; want %+v", got, ok, tc.want)
			}
		})
	}
	if _, ok := RouteThread(lead, 0, nil); ok {
		t.Fatal("a thread with no participant routed somewhere")
	}
}
