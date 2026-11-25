package turntaking

import (
	"errors"
	"reflect"
	"testing"
)

// RouteMessage is the one rule the cloud start and the desktop Local work
// start route a person's message by (issue 3049).
func TestRouteMessage(t *testing.T) {
	lead := Settings{Policy: PolicyLead, Lead: 12}
	cases := []struct {
		name        string
		settings    Settings
		participant int64
		addressed   []int64
		want        []int64
		err         error
	}{
		{"no target under a lead goes to the lead", lead, 0, nil, []int64{12}, nil},
		{"a sticky target under a lead yields to the lead", lead, 11, nil, []int64{12}, nil},
		{"a target outside the address yields to it", lead, 11, []int64{13}, []int64{13}, nil},
		{"a target inside the address answers first", Default, 13, []int64{12, 13}, []int64{13, 12}, nil},
		{"a target that is no agent is ignored", Default, 99, nil, nil, ErrAgentTargetRequired},
		{"no target and no lead is refused", Default, 0, nil, nil, ErrAgentTargetRequired},
		{"an addressee outside the conversation is refused", Default, 0, []int64{99}, nil, ErrNotInConversation},
	}
	for _, tc := range cases {
		got, err := RouteMessage(tc.settings, tc.participant, tc.addressed, roster)
		if !errors.Is(err, tc.err) || (tc.err == nil && !reflect.DeepEqual(got, tc.want)) {
			t.Errorf("%s: got %v, %v; want %v, %v", tc.name, got, err, tc.want, tc.err)
		}
	}
	if !errors.Is(ErrNoTarget, ErrAgentTargetRequired) {
		t.Fatal("ErrNoTarget must be ErrAgentTargetRequired")
	}
}

func TestLeadIsIgnoredWhenItLeft(t *testing.T) {
	if got := Lead(Settings{Policy: PolicyLead, Lead: 99}, roster); got != 0 {
		t.Fatalf("a departed lead = %d", got)
	}
	if got := Lead(Settings{Policy: PolicyLead, Lead: 12}, roster); got != 12 {
		t.Fatalf("lead = %d", got)
	}
	if got := Lead(Default, roster); got != 0 {
		t.Fatalf("addressed lead = %d", got)
	}
}
