package agentexecution

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

// #977 — `user_ids` is the mention list, no longer a parity gate.
func TestParseMentionedUserIDs(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		snake, camel string
		want         []int64
		ok           bool
	}{
		"absent on both is an ordinary message": {"", "", []int64{}, true},
		"snake_case alone":                      {`[11,12]`, "", []int64{11, 12}, true},
		// The spelling the composer actually sends. Before #977 this field was
		// not bound at all, so a real mention was silently dropped.
		"camelCase alone":               {"", `[11,12]`, []int64{11, 12}, true},
		"both, merged and deduplicated": {`[11]`, `[11,12]`, []int64{11, 12}, true},
		"explicit null is absent":       {`null`, `null`, []int64{}, true},
		"an empty array names nobody":   {`[]`, "", []int64{}, true},
		// A client that thinks it tagged somebody and did not is worth a 400:
		// the alternative is an admitted turn whose sender believes a colleague
		// was told.
		"a non-array is refused":       {`{"a":1}`, "", nil, false},
		"a non-positive id is refused": {`[0]`, "", nil, false},
		"a negative id is refused":     {`[-3]`, "", nil, false},
		"a non-numeric id is refused":  {`["11"]`, "", nil, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, ok := parseMentionedUserIDs(json.RawMessage(tc.snake), json.RawMessage(tc.camel))
			if ok != tc.ok {
				t.Fatalf("parseMentionedUserIDs(%q,%q) ok = %v, want %v", tc.snake, tc.camel, ok, tc.ok)
			}
			if !tc.ok {
				return
			}
			if len(got) != len(tc.want) {
				t.Fatalf("parseMentionedUserIDs(%q,%q) = %v, want %v", tc.snake, tc.camel, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("parseMentionedUserIDs(%q,%q) = %v, want %v", tc.snake, tc.camel, got, tc.want)
				}
			}
		})
	}
}

// The ceiling exists because the list is attacker-influenced and every entry
// becomes a row.
func TestParseMentionedUserIDsRefusesAnUnboundedAudience(t *testing.T) {
	t.Parallel()

	ids := make([]string, 0, maxMentionedUsers+1)
	for i := 1; i <= maxMentionedUsers+1; i++ {
		ids = append(ids, strconv.Itoa(i))
	}
	if _, ok := parseMentionedUserIDs(json.RawMessage("["+strings.Join(ids, ",")+"]"), nil); ok {
		t.Fatalf("a mention list past %d entries must be refused", maxMentionedUsers)
	}
}
