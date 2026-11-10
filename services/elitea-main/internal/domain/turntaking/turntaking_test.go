package turntaking

import (
	"errors"
	"reflect"
	"testing"
)

var roster = []int64{11, 12, 13}

func TestParse(t *testing.T) {
	cases := []struct {
		name string
		raw  any
		want Settings
		ok   bool
	}{
		{"absent", nil, Default, true},
		{"addressed", map[string]any{"policy": "addressed"}, Default, true},
		{"lead", map[string]any{"policy": "lead", "lead_participant_id": float64(12)}, Settings{PolicyLead, 12}, true},
		{"addressed drops the lead", map[string]any{"policy": "addressed", "lead_participant_id": float64(12)}, Default, true},
		{"lead needs an id", map[string]any{"policy": "lead"}, Settings{}, false},
		{"bad policy", map[string]any{"policy": "free_for_all"}, Settings{}, false},
		{"bad id", map[string]any{"policy": "lead", "lead_participant_id": 1.5}, Settings{}, false},
		{"unknown key", map[string]any{"router": true}, Settings{}, false},
		{"not an object", "lead", Settings{}, false},
	}
	for _, c := range cases {
		got, err := Parse(c.raw)
		if c.ok != (err == nil) || (c.ok && got != c.want) {
			t.Errorf("%s: Parse = %+v, %v", c.name, got, err)
		}
		if err != nil && !errors.Is(err, ErrInvalidFloor) {
			t.Errorf("%s: error %v is not ErrInvalidFloor", c.name, err)
		}
	}
}

// The floor-policy acceptance cases of ADR-0050 (Verification 8).
func TestRoute(t *testing.T) {
	lead := Settings{Policy: PolicyLead, Lead: 13}
	cases := []struct {
		name        string
		settings    Settings
		participant int64
		addressed   []int64
		agents      []int64
		want        []int64
		err         error
	}{
		{"addressed: one agent addressed", Default, 12, []int64{12}, roster, []int64{12}, nil},
		{"addressed: several, the participant first", Default, 12, []int64{11, 12}, roster, []int64{12, 11}, nil},
		{"addressed: several, no participant", Default, 0, []int64{13, 11, 13}, roster, []int64{13, 11}, nil},
		{"no address, one agent", Default, 0, nil, []int64{11}, []int64{11}, nil},
		{"no address, the sticky target", Default, 12, nil, roster, []int64{12}, nil},
		{"no address, several agents, first message", Default, 0, nil, roster, nil, ErrNoTarget},
		{"lead: no address goes to the lead", lead, 12, nil, roster, []int64{13}, nil},
		{"lead: an address beats the lead", lead, 0, []int64{11}, roster, []int64{11}, nil},
		{"lead: a removed lead falls back to the sticky target", lead, 12, nil, []int64{11, 12}, []int64{12}, nil},
		// Issue 1804: participant_id alone is the composer's target, never an
		// address, so it does not override the lead; an address does.
		{"lead: a composer pick without an address goes to the lead", lead, 11, nil, roster, []int64{13}, nil},
		{"lead: a composer pick that is addressed answers", lead, 11, []int64{11}, roster, []int64{11}, nil},
		{"lead: the lead's own pick goes to the lead", lead, 13, nil, roster, []int64{13}, nil},
		{"lead: several addressed, the picked one first, no lead", lead, 12, []int64{11, 12}, roster, []int64{12, 11}, nil},
		{"lead: no pick and no address goes to the lead", lead, 0, nil, roster, []int64{13}, nil},
		{"lead: a removed lead with no pick asks for one", lead, 0, nil, []int64{11, 12}, nil, ErrNoTarget},
		{"an addressee that is not an agent", Default, 0, []int64{99}, roster, nil, ErrNotInConversation},
		{"a participant outside the address", Default, 11, []int64{12}, roster, nil, ErrNotInConversation},
	}
	for _, c := range cases {
		got, err := Route(c.settings, c.participant, c.addressed, c.agents)
		if !errors.Is(err, c.err) || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: Route = %v, %v; want %v, %v", c.name, got, err, c.want, c.err)
		}
	}
}

func TestHandRefusesWhatIsNotAHandOff(t *testing.T) {
	for name, targets := range map[string][]int64{
		StopEmpty:      nil,
		StopSelf:       {11},
		StopNotAnAgent: {99},
		StopTooMany:    {12, 13, 12, 13, 12},
	} {
		_, err := Hand(Exchange{First: 11}, 11, targets, roster)
		var invalid *InvalidHandError
		if !errors.As(err, &invalid) || invalid.Code != name {
			t.Errorf("%s: Hand error = %v", name, err)
		}
	}
	_, err := Hand(Exchange{First: 11}, 11, []int64{12, 12}, roster)
	var invalid *InvalidHandError
	if !errors.As(err, &invalid) {
		t.Errorf("a repeated target is refused, got %v", err)
	}
}

// Each guard fires exactly at its limit.
func TestHopGuardFiresAtSix(t *testing.T) {
	agents := []int64{1, 2, 3, 4, 5, 6, 7, 8}
	exchange := Exchange{First: 1}
	from := int64(1)
	for hop := 1; hop <= MaxHops; hop++ {
		to := int64(hop + 1)
		decision, err := Hand(exchange, from, []int64{to}, agents)
		if err != nil || decision.Stopped != "" || !reflect.DeepEqual(decision.Handed, []int64{to}) {
			t.Fatalf("hop %d: %+v, %v", hop, decision, err)
		}
		exchange.Turns = append(exchange.Turns, Turn{From: from, To: to})
		from = to
	}
	decision, err := Hand(exchange, from, []int64{8}, agents)
	if err != nil || decision.Stopped != StopHops || decision.Limit != MaxHops || len(decision.Handed) != 0 {
		t.Fatalf("hop 7: %+v, %v", decision, err)
	}
}

func TestTurnGuardFiresAtTwoTurnsOfOneAgent(t *testing.T) {
	agents := []int64{1, 2, 3}
	// 1 answered the person, handed to 2, 2 handed back to 1: 1 has two turns.
	exchange := Exchange{First: 1, Turns: []Turn{{From: 1, To: 2}, {From: 2, To: 1}}}
	decision, err := Hand(exchange, 1, []int64{3}, agents)
	if err != nil || decision.Stopped != "" {
		t.Fatalf("a third agent may still answer: %+v, %v", decision, err)
	}
	exchange.Turns = append(exchange.Turns, Turn{From: 1, To: 3})
	decision, err = Hand(exchange, 3, []int64{1}, agents)
	if err != nil || decision.Stopped != StopAgentTurns || decision.Limit != MaxTurnsPerAgent {
		t.Fatalf("a third turn of agent 1: %+v, %v", decision, err)
	}
}

func TestPairGuardFiresAtThreeExchanges(t *testing.T) {
	agents := []int64{1, 2, 3}
	// 3 answered the person and handed to 1; then 1 and 2 hand the floor to
	// each other three times. Neither has passed two turns, so the fourth
	// hand-off between them is stopped by the pair guard.
	exchange := Exchange{First: 3, Turns: []Turn{{From: 3, To: 1}, {From: 1, To: 2}, {From: 2, To: 1}}}
	if decision, err := Hand(exchange, 1, []int64{2}, agents); err != nil || decision.Stopped != "" {
		t.Fatalf("the third exchange is made: %+v, %v", decision, err)
	}
	exchange.Turns = append(exchange.Turns, Turn{From: 1, To: 2})
	decision, err := Hand(exchange, 2, []int64{1}, agents)
	if err != nil {
		t.Fatal(err)
	}
	// 1 would take its third turn as well; the turn guard is checked first.
	if decision.Stopped != StopAgentTurns {
		t.Fatalf("1's third turn: %+v", decision)
	}
	// With the pair alone at its limit, the pair guard fires.
	pairOnly := Exchange{Turns: []Turn{{From: 1, To: 2}, {From: 2, To: 1}, {From: 1, To: 2}}}
	decision, err = Hand(pairOnly, 2, []int64{1}, agents)
	if err != nil || decision.Stopped != StopPairs || decision.Limit != MaxPairExchanges {
		t.Fatalf("the pair guard: %+v, %v", decision, err)
	}
}

func TestHandStopsPartWayThroughTheTargets(t *testing.T) {
	agents := []int64{1, 2, 3, 4, 5, 6, 7, 8}
	exchange := Exchange{First: 1}
	for i := int64(1); i <= 5; i++ {
		exchange.Turns = append(exchange.Turns, Turn{From: i, To: i + 1})
	}
	decision, err := Hand(exchange, 6, []int64{7, 8}, agents)
	if err != nil || len(decision.Handed) != 0 || decision.Stopped != StopHops {
		t.Fatalf("a call whose second target passes the guard hands nothing: %+v, %v", decision, err)
	}
}

func TestAddressedTurnsAreNotHops(t *testing.T) {
	agents := []int64{1, 2, 3}
	exchange := Exchange{First: 1, Turns: []Turn{{To: 2}, {To: 3}}}
	decision, err := Hand(exchange, 3, []int64{1}, agents)
	if err != nil || decision.Stopped != "" || !reflect.DeepEqual(decision.Handed, []int64{1}) {
		t.Fatalf("%+v, %v", decision, err)
	}
}

func TestStopText(t *testing.T) {
	if got := StopText(StopHops, 6, "", [2]string{}); got != "Loop guard stopped the exchange after 6 hops." {
		t.Errorf("hops: %q", got)
	}
	if got := StopText(StopPairs, 3, "", [2]string{"Mira", "CI Doctor"}); got !=
		"Loop guard stopped the exchange after 3 exchanges between Mira and CI Doctor." {
		t.Errorf("pairs: %q", got)
	}
}
