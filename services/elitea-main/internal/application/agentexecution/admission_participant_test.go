package agentexecution

import "testing"

func TestAdmissionParticipantIDIsTheTurnTarget(t *testing.T) {
	cases := []struct {
		name      string
		admission Admission
		want      int64
	}{
		{"current", Admission{CurrentTurn: &CurrentApplicationTurn{TargetParticipantID: 3}}, 3},
		{"adhoc", Admission{CurrentAdhocTurn: &CurrentAdhocTurn{TargetParticipantID: 4}}, 4},
		{"regenerate", Admission{CurrentRegenerateTurn: &CurrentRegenerateTurn{TargetParticipantID: 5}}, 5},
		{"continue", Admission{CurrentContinueTurn: &CurrentContinueTurn{TargetParticipantID: 6}}, 6},
		{"fanout wake", Admission{CurrentFanoutWakeTurn: &CurrentFanoutWakeTurn{}}, 0},
		{"none", Admission{}, 0},
	}
	for _, c := range cases {
		if got := c.admission.ParticipantID(); got != c.want {
			t.Errorf("%s: ParticipantID() = %d, want %d", c.name, got, c.want)
		}
	}
}
