package runfinished

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestNotifyIsTheNoiseRule(t *testing.T) {
	cases := []struct {
		name  string
		facts Facts
		want  bool
	}{
		{"a short completed answer", Facts{Outcome: OutcomeCompleted, Duration: 30 * time.Second, TriggerOrigin: "manual"}, false},
		{"completed just under two minutes", Facts{Outcome: OutcomeCompleted, Duration: MinDuration - time.Millisecond}, false},
		{"completed at two minutes", Facts{Outcome: OutcomeCompleted, Duration: MinDuration}, true},
		{"completed after a continuation", Facts{Outcome: OutcomeCompleted, Continuations: 1}, true},
		{"completed api run, short", Facts{Outcome: OutcomeCompleted, TriggerOrigin: "api"}, false},
		{"completed schedule run", Facts{Outcome: OutcomeCompleted, TriggerOrigin: "schedule"}, true},
		{"completed webhook run", Facts{Outcome: OutcomeCompleted, TriggerOrigin: "webhook"}, true},
		{"completed cloud turn of a handed-off thread", Facts{Outcome: OutcomeCompleted, HandedOff: true}, true},
		{"failed at once", Facts{Outcome: OutcomeFailed}, true},
		{"waiting at once", Facts{Outcome: OutcomeWaiting}, true},
		{"limit reached", Facts{Outcome: OutcomeLimitReached}, true},
		{"cancelled, short", Facts{Outcome: OutcomeCancelled}, false},
		{"cancelled, long with continuations", Facts{Outcome: OutcomeCancelled, Continuations: 5, Duration: time.Hour}, false},
		{"cancelled schedule run", Facts{Outcome: OutcomeCancelled, TriggerOrigin: "schedule", HandedOff: true}, false},
		{"unknown outcome", Facts{Outcome: "exploded", Continuations: 3}, false},
		{"empty outcome", Facts{Duration: time.Hour}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Notify(tc.facts); got != tc.want {
				t.Fatalf("Notify(%+v) = %v, want %v", tc.facts, got, tc.want)
			}
		})
	}
}

func TestMetaIsContentFreeAndUsesTheContractNames(t *testing.T) {
	raw, err := json.Marshal(Meta{
		Outcome: OutcomeLimitReached, Limit: "spend", ProjectID: 7,
		ConversationUUID: "0b7f5c86-29f4-4c5a-9b0f-5a6f8e0b2c11", ChainRootExecutionID: strings.Repeat("a", 32),
		ExecutionID: strings.Repeat("b", 32), Origin: OriginCloud, Continuations: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"outcome": true, "limit": true, "project_id": true, "conversation_uuid": true,
		"chain_root_execution_id": true, "execution_id": true, "origin": true, "continuations": true,
	}
	for key := range decoded {
		if !want[key] {
			t.Errorf("meta carries %q, which the contract does not name", key)
		}
	}
	for key := range want {
		if _, ok := decoded[key]; !ok {
			t.Errorf("meta lacks %q", key)
		}
	}
	// limit and thread_id are left out when empty.
	raw, _ = json.Marshal(Meta{Outcome: OutcomeCompleted, Origin: OriginDesktop})
	if strings.Contains(string(raw), "limit") || strings.Contains(string(raw), "thread_id") {
		t.Fatalf("empty optional fields are written: %s", raw)
	}
}

func TestNotificationKeyIsPerChainRoot(t *testing.T) {
	if NotificationKey("a") == NotificationKey("b") {
		t.Fatal("two roots share a key")
	}
	if NotificationKey("a") != NotificationKey("a") {
		t.Fatal("the key is not stable")
	}
}
