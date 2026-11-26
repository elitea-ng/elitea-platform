package turntaking

import (
	"encoding/json"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// handCases is testdata/hand_cases.json, the fixture the cloud listener's
// repository and the desktop's handLocalFloor run too (issue 3063).
type handCases struct {
	Cases []struct {
		Name      string  `json:"name"`
		First     int64   `json:"first"`
		Routed    []int64 `json:"routed"`
		LocalOnly bool    `json:"local_only"`
		Consults  []struct {
			From int64 `json:"from"`
			To   int64 `json:"to"`
		} `json:"consults"`
		Steps []struct {
			From int64   `json:"from"`
			To   []int64 `json:"to"`
		} `json:"steps"`
		Want struct {
			HandedTo []int64 `json:"handed_to"`
			Stopped  string  `json:"stopped"`
			Limit    int     `json:"limit"`
		} `json:"want"`
	} `json:"cases"`
}

var fixtureAgents = []Agent{{1, "Ada"}, {2, "Bo"}, {3, "Cy"}}

func TestHandCasesFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/hand_cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture handCases
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Cases) < 8 {
		t.Fatalf("the fixture has %d cases", len(fixture.Cases))
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			exchange := Exchange{First: c.First}
			for _, to := range c.Routed {
				exchange.Turns = append(exchange.Turns, Turn{To: to})
			}
			for _, consult := range c.Consults {
				exchange.Consults = append(exchange.Consults, Turn{From: consult.From, To: consult.To})
			}
			stopped := ""
			var last HandOutcome
			for _, step := range c.Steps {
				last = DecideHand(exchange, stopped, fixtureAgents, step.From, step.To, "over to you")
				for _, to := range last.Handed {
					exchange.Turns = append(exchange.Turns, Turn{From: step.From, To: to})
				}
				if last.StopText != "" {
					stopped = last.Stopped
				}
			}
			handed := last.Handed
			if handed == nil {
				handed = []int64{}
			}
			if !slices.Equal(handed, c.Want.HandedTo) || last.Stopped != c.Want.Stopped {
				t.Fatalf("outcome = %+v, want %+v", last, c.Want)
			}
			if c.Want.Limit > 0 && !strings.Contains(last.StopText, strconv.Itoa(c.Want.Limit)) {
				t.Fatalf("stop text %q does not name the limit %d", last.StopText, c.Want.Limit)
			}
			if c.Want.Limit == 0 && last.StopText != "" {
				t.Fatalf("a stopped exchange posted again: %q", last.StopText)
			}
		})
	}
}

// A consult is one hop and one exchange of its pair, never a turn.
func TestHandCountsConsultsAsHopsNotTurns(t *testing.T) {
	exchange := Exchange{First: 1, Consults: []Turn{{From: 1, To: 2}, {From: 1, To: 2}}}
	decision, err := Hand(exchange, 1, []int64{2}, []int64{1, 2})
	if err != nil || !slices.Equal(decision.Handed, []int64{2}) {
		t.Fatalf("decision = %+v, %v", decision, err)
	}
	exchange.Consults = append(exchange.Consults, Turn{From: 2, To: 1})
	decision, err = Hand(exchange, 1, []int64{2}, []int64{1, 2})
	if err != nil || decision.Stopped != StopPairs || decision.Limit != MaxPairExchanges {
		t.Fatalf("decision = %+v, %v", decision, err)
	}
	// A malformed pair counts nothing.
	decision, _ = Hand(Exchange{First: 1, Consults: []Turn{{From: 0, To: 2}}}, 1, []int64{2}, []int64{1, 2})
	if decision.Stopped != "" {
		t.Fatalf("decision = %+v", decision)
	}
}

func TestDecideHandTexts(t *testing.T) {
	if out := DecideHand(Exchange{First: 1}, "", fixtureAgents, 1, []int64{2}, ""); len(out.Handed) != 0 ||
		!strings.Contains(out.Message, "say what you need") {
		t.Fatalf("empty message = %+v", out)
	}
	if out := DecideHand(Exchange{First: 1}, "", fixtureAgents, 1, []int64{2}, strings.Repeat("x", MaxHandMessageBytes+1)); len(out.Handed) != 0 ||
		!strings.Contains(out.Message, "longer than 4000") {
		t.Fatalf("long message = %+v", out)
	}
	if out := DecideHand(Exchange{First: 1}, "", fixtureAgents, 1, []int64{2, 3, 1, 9, 8}, "x"); !strings.Contains(out.Message, "at most 4") {
		t.Fatalf("too many = %+v", out)
	}
	if out := DecideHand(Exchange{First: 1}, StopHops, fixtureAgents, 1, []int64{2}, "x"); out.Stopped != StopHops || out.StopText != "" {
		t.Fatalf("already stopped = %+v", out)
	}
}
