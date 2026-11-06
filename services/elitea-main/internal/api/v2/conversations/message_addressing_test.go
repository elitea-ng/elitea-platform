package conversations

import (
	"encoding/json"
	"reflect"
	"testing"
)

// The map-shaped reads and Message serve the same keys with the same
// omissions.
func TestMessageAddressingFieldsMatchTheMessageJSON(t *testing.T) {
	kind, cause := "agent", "agent_mention"
	for _, addressing := range []MessageAddressing{
		{Visibility: "all"},
		{AuthorKind: &kind, AddressedTo: []int{3, 4}, Visibility: "side", Cause: &cause},
	} {
		encoded, err := json.Marshal(addressing)
		if err != nil {
			t.Fatal(err)
		}
		var want map[string]any
		if err := json.Unmarshal(encoded, &want); err != nil {
			t.Fatal(err)
		}
		got := map[string]any{}
		addressing.Fields(got)
		roundTrip, _ := json.Marshal(got)
		var gotJSON map[string]any
		_ = json.Unmarshal(roundTrip, &gotJSON)
		if want["addressed_to"] == nil {
			want["addressed_to"] = []any{}
		}
		if !reflect.DeepEqual(gotJSON, want) {
			t.Fatalf("Fields = %v, Message JSON = %v", gotJSON, want)
		}
	}
}
