package executioninterrupt

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// mapCard is the contract card fixture raised by Map item `index` (member =
// the decimal index, ordinal = index + 1) of a Map with `total` items.
func mapCard(t *testing.T, kind, member string, ordinal, total int) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(contractDir, "fixtures", "fanout-interrupt-card-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var card map[string]any
	if err := json.Unmarshal(raw, &card); err != nil {
		t.Fatal(err)
	}
	fanout := card["fanout_v1"].(map[string]any)
	fanout["kind"], fanout["member"], fanout["ordinal"], fanout["total"] = kind, member, ordinal, total
	for _, tier := range card["parent_agent_path"].([]any) {
		tier.(map[string]any)["sibling_ordinal"] = ordinal
	}
	encoded, err := json.Marshal(card)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

// T-2A3: a Map item's card goes through the same card contract and decide
// path as a Parallel member's (no Map route): kind `map`, the item's decimal
// index as its member, up to 64 items. A Parallel keeps its 16-member bound,
// and a Map member must be an index.
func TestParseCardAcceptsAMapItemCardOnTheSharedContract(t *testing.T) {
	card, _, err := ParseCard(mapCard(t, "map", "39", 40, 64))
	if err != nil {
		t.Fatalf("a Map item card is refused: %v", err)
	}
	frontier, err := card.MemberFrontier()
	if err != nil {
		t.Fatalf("the Map item card has no frontier: %v", err)
	}
	if frontier.FanoutNode != "research" || frontier.Ordinal != 40 {
		t.Fatalf("frontier = %+v, want the Map node and ordinal 40", frontier)
	}
	for name, raw := range map[string][]byte{
		"parallel over 16": mapCard(t, "parallel", "jira_branch", 40, 64),
		"map member name":  mapCard(t, "map", "jira_branch", 1, 3),
		"map member 64":    mapCard(t, "map", "64", 64, 64),
	} {
		if _, _, err := ParseCard(raw); !errors.Is(err, ErrInvalidCard) {
			t.Errorf("%s: accepted (%v)", name, err)
		}
	}
}
