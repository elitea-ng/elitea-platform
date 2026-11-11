package repos

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"testing"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/contextview"
)

func contextViewEntries(spec ...string) []agentexecutionapp.ContextViewEntry {
	out := make([]agentexecutionapp.ContextViewEntry, 0, len(spec))
	for _, s := range spec {
		entry := agentexecutionapp.ContextViewEntry{MessageID: s}
		if len(s) > 1 && s[len(s)-1] == '!' {
			entry = agentexecutionapp.ContextViewEntry{MessageID: s[:len(s)-1], Reason: contextview.ReasonBeforeJoined}
		}
		out = append(out, entry)
	}
	return out
}

func roundTripContextViewDelta(t *testing.T, base, next []agentexecutionapp.ContextViewEntry) []contextViewOp {
	t.Helper()
	ops := encodeContextViewDelta(base, next)
	// Through JSON, as the frame stores it.
	raw, err := json.Marshal(ops)
	if err != nil {
		t.Fatal(err)
	}
	var decoded []contextViewOp
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	got, err := applyContextViewDelta(base, decoded)
	if err != nil {
		t.Fatalf("apply %s: %v", raw, err)
	}
	if len(got) == 0 && len(next) == 0 {
		return ops
	}
	if !reflect.DeepEqual(got, next) {
		t.Fatalf("apply %s to %v = %v, want %v", raw, base, got, next)
	}
	return ops
}

func TestContextViewDeltaShapes(t *testing.T) {
	for _, tc := range []struct {
		name       string
		base, next []agentexecutionapp.ContextViewEntry
		want       string
	}{
		{"both empty", nil, nil, `[]`},
		{"from empty", nil, contextViewEntries("a", "b"), `[{"add":[{"message_id":"a"},{"message_id":"b"}]}]`},
		{"to empty", contextViewEntries("a", "b"), nil, `[{"drop":2}]`},
		{"grew at the end", contextViewEntries("a", "b", "c"), contextViewEntries("a", "b", "c", "d", "e"),
			`[{"keep":3},{"add":[{"message_id":"d"},{"message_id":"e"}]}]`},
		{"unchanged", contextViewEntries("a", "b"), contextViewEntries("a", "b"), `[{"keep":2}]`},
		{"a message deleted since", contextViewEntries("a", "b", "c"), contextViewEntries("a", "c", "d"),
			`[{"keep":1},{"drop":1},{"keep":1},{"add":[{"message_id":"d"}]}]`},
		{"a reason changed", contextViewEntries("a", "b", "c"), contextViewEntries("a", "b!", "c"),
			`[{"keep":1},{"drop":1},{"add":[{"message_id":"b","reason":"before_joined"}]},{"keep":1}]`},
		{"a shorter regenerated view", contextViewEntries("a", "b", "c", "d"), contextViewEntries("a", "b"),
			`[{"keep":2},{"drop":2}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ops := roundTripContextViewDelta(t, tc.base, tc.next)
			raw, _ := json.Marshal(ops)
			if string(raw) != tc.want {
				t.Fatalf("delta = %s, want %s", raw, tc.want)
			}
		})
	}
}

// Any two lists round-trip, reordered and duplicated ids included: the ids
// only shape the script, never its correctness.
func TestContextViewDeltaRoundTripsAnyLists(t *testing.T) {
	random := rand.New(rand.NewSource(1893))
	list := func() []agentexecutionapp.ContextViewEntry {
		out := make([]agentexecutionapp.ContextViewEntry, random.Intn(12))
		for i := range out {
			out[i].MessageID = fmt.Sprintf("m%d", random.Intn(8))
			if random.Intn(3) == 0 {
				out[i].Reason = contextview.ReasonBeforeJoined
			}
		}
		return out
	}
	for range 5000 {
		roundTripContextViewDelta(t, list(), list())
	}
}

func TestContextViewDeltaRejectsDamagedScripts(t *testing.T) {
	base := contextViewEntries("a", "b", "c")
	for name, ops := range map[string][]contextViewOp{
		"keeps past the base":      {{Keep: 4}},
		"drops past the base":      {{Keep: 1}, {Drop: 3}},
		"does not cover the base":  {{Keep: 2}},
		"two fields in one op":     {{Keep: 1, Drop: 2}},
		"an empty op":              {{}},
		"a negative count":         {{Keep: -1}, {Keep: 4}},
		"adds but leaves the base": {{Add: contextViewEntries("d")}},
	} {
		if _, err := applyContextViewDelta(base, ops); err == nil {
			t.Errorf("%s: applied", name)
		}
	}
}

// The storage a 2,000-message conversation costs, the view of one reader
// recorded on every other message (the answers): the 0167 full list per
// answer against one keyframe every contextViewKeyframeInterval and a delta
// otherwise, measured as the bytes of the stored jsonb text.
func TestContextViewDeltaStorageForALongConversation(t *testing.T) {
	const messages = 2000
	var fullBytes, frameBytes, keyframes int
	var previous []agentexecutionapp.ContextViewEntry
	depth := 0
	for answer := 1; answer <= messages/2; answer++ {
		view := make([]agentexecutionapp.ContextViewEntry, 0, 2*answer)
		for i := range 2*answer - 1 {
			view = append(view, agentexecutionapp.ContextViewEntry{MessageID: fmt.Sprintf("%08d-0000-4000-8000-%012d", i, i)})
		}
		full, _ := json.Marshal(view)
		fullBytes += len(full)
		stored := len(full)
		if previous != nil && depth+1 < contextViewKeyframeInterval {
			delta, _ := json.Marshal(encodeContextViewDelta(previous, view))
			if len(delta) < len(full) {
				stored, depth = len(delta), depth+1
			} else {
				depth = 0
			}
		} else {
			depth = 0
		}
		if depth == 0 {
			keyframes++
		}
		frameBytes += stored
		previous = view
	}
	t.Logf("%d messages, %d answers: full rows %d bytes, frames %d bytes (%d keyframes), %.1fx smaller",
		messages, messages/2, fullBytes, frameBytes, keyframes, float64(fullBytes)/float64(frameBytes))
	if frameBytes*10 > fullBytes {
		t.Fatalf("frames %d bytes are not a tenth of full rows %d bytes", frameBytes, fullBytes)
	}
}
