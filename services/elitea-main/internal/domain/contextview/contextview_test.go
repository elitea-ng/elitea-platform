package contextview

import (
	"errors"
	"testing"
)

func TestParseReadsTheBlockAndRefusesUnknownKeys(t *testing.T) {
	if got, err := Parse(nil); err != nil || got != Default {
		t.Fatalf("absent: %+v %v", got, err)
	}
	got, err := Parse(map[string]any{"history": "addressed_only", "include_side_talk": "none", "activity_notes": true})
	if err != nil || got != (Settings{History: HistoryAddressedOnly, SideTalk: SideTalkNone, ActivityNotes: true}) {
		t.Fatalf("full block: %+v %v", got, err)
	}
	if got, err := Parse(map[string]any{"history": "none"}); err != nil || got.SideTalk != SideTalkParty {
		t.Fatalf("partial block keeps defaults: %+v %v", got, err)
	}
	for name, raw := range map[string]any{
		"not object":  "full",
		"bad mode":    map[string]any{"history": "everything"},
		"mode type":   map[string]any{"history": 1},
		"bad side":    map[string]any{"include_side_talk": "all"},
		"notes type":  map[string]any{"activity_notes": "yes"},
		"unknown key": map[string]any{"window": 1000},
	} {
		if _, err := Parse(raw); !errors.Is(err, ErrInvalidBlock) {
			t.Fatalf("%s: err=%v", name, err)
		}
	}
}

func TestWithParticipantOnlyNarrowsFull(t *testing.T) {
	if got := Default.WithParticipant("since_joined"); got.History != HistorySinceJoined {
		t.Fatalf("full → %s", got.History)
	}
	none := Settings{History: HistoryNone, SideTalk: SideTalkParty}
	if got := none.WithParticipant("since_joined"); got.History != HistoryNone {
		t.Fatalf("none widened to %s", got.History)
	}
	if got := Default.WithParticipant("full"); got.History != HistoryFull {
		t.Fatalf("full participant changed mode to %s", got.History)
	}
	if got := Default.WithParticipant(""); got != Default {
		t.Fatalf("empty participant changed %+v", got)
	}
}

func TestEligibleKeepsSideTalkToItsParty(t *testing.T) {
	foreignSide := Facts{Side: true, AfterJoined: true}
	ownSide := Facts{Side: true, OwnAgent: true}
	addressedSide := Facts{Side: true, Addressed: true}
	if Default.Eligible(foreignSide) {
		t.Fatal("side talk between two other agents is eligible")
	}
	if !Default.Eligible(ownSide) || !Default.Eligible(addressedSide) {
		t.Fatal("the reader's own side talk is not eligible")
	}
	noSide := Settings{History: HistoryFull, SideTalk: SideTalkNone}
	if noSide.Eligible(ownSide) || !noSide.Eligible(Facts{}) {
		t.Fatal("include_side_talk none")
	}
}

func TestEligibleModes(t *testing.T) {
	before := Facts{}
	after := Facts{AfterJoined: true}
	own := Facts{OwnAgent: true}
	addressed := Facts{Addressed: true}
	readersOwn := Facts{ReadersOwn: true}
	answered := Facts{AnsweredByReader: true}
	cases := []struct {
		mode History
		f    Facts
		want bool
	}{
		{HistoryFull, before, true},
		{HistorySinceJoined, before, false},
		{HistorySinceJoined, after, true},
		{HistorySinceJoined, own, true},
		{HistoryAddressedOnly, before, false},
		{HistoryAddressedOnly, after, false},
		{HistoryAddressedOnly, own, true},
		{HistoryAddressedOnly, addressed, true},
		{HistoryAddressedOnly, readersOwn, true},
		{HistoryAddressedOnly, answered, true},
		{HistoryNone, own, false},
		{HistoryNone, readersOwn, false},
	}
	for _, c := range cases {
		if got := (Settings{History: c.mode, SideTalk: SideTalkParty}).Eligible(c.f); got != c.want {
			t.Errorf("%s %+v = %v, want %v", c.mode, c.f, got, c.want)
		}
	}
}

func TestDigestCoversEveryField(t *testing.T) {
	refs := []Ref{{MessageID: "a", ReadersOwn: true}}
	base := Digest(HistoryFull, 128000, []byte(`[1]`), refs)
	if len(base) != 64 || base != Digest(HistoryFull, 128000, []byte(`[1]`), []Ref{{MessageID: "a", ReadersOwn: true}}) {
		t.Fatalf("digest not stable: %s", base)
	}
	for name, other := range map[string]string{
		"mode":    Digest(HistoryNone, 128000, []byte(`[1]`), refs),
		"window":  Digest(HistoryFull, 32000, []byte(`[1]`), refs),
		"history": Digest(HistoryFull, 128000, []byte(`[2]`), refs),
		"ref id":  Digest(HistoryFull, 128000, []byte(`[1]`), []Ref{{MessageID: "b", ReadersOwn: true}}),
		"ref own": Digest(HistoryFull, 128000, []byte(`[1]`), []Ref{{MessageID: "a"}}),
		"count":   Digest(HistoryFull, 128000, []byte(`[1]`), nil),
	} {
		if other == base {
			t.Errorf("%s does not change the digest", name)
		}
	}
	// A fixed vector the worker's test repeats.
	if got := Digest(HistorySinceJoined, 8192, []byte(`[]`), nil); got != digestVector {
		t.Fatalf("vector = %s", got)
	}
}

func TestExclusionNamesTheRuleThatDroppedTheMessage(t *testing.T) {
	sinceJoined := Settings{History: HistorySinceJoined, SideTalk: SideTalkParty}
	addressedOnly := Settings{History: HistoryAddressedOnly, SideTalk: SideTalkParty}
	noSide := Settings{History: HistoryFull, SideTalk: SideTalkNone}
	none := Settings{History: HistoryNone, SideTalk: SideTalkParty}
	cases := []struct {
		name string
		s    Settings
		f    Facts
		want Reason
	}{
		{"foreign side talk", Default, Facts{Side: true, AfterJoined: true}, ReasonSideTalkOfOthers},
		{"own side talk, side talk off", noSide, Facts{Side: true, OwnAgent: true}, ReasonSideTalkOff},
		{"none", none, Facts{OwnAgent: true}, ReasonHistoryNone},
		{"before joined", sinceJoined, Facts{}, ReasonBeforeJoined},
		{"not addressed", addressedOnly, Facts{AfterJoined: true}, ReasonNotAddressed},
		{"in view", Default, Facts{}, ""},
	}
	for _, c := range cases {
		if got := c.s.Exclusion(c.f); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
		if c.s.Eligible(c.f) != (c.want == "") {
			t.Errorf("%s: Eligible disagrees with Exclusion", c.name)
		}
	}
}

// digestVector is Digest(since_joined, 8192, `[]`, no refs); the worker's
// context_view test pins the same value.
const digestVector = "98c87e67997f565a4d27a3a984ac5d8e9aa3353c0457a2222ac27566fd08b5dc"
