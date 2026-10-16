package agentexecution

import (
	"bytes"
	"os"
	"testing"
)

func TestFanoutWakeMetaIsTheContractFixture(t *testing.T) {
	fixture, err := os.ReadFile("../../../../../libs/jsonschema/runtime/v1/fixtures/fanout-wake-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	got, err := FanoutWakeMeta("10000000-0000-4000-8000-000000000043", 2)
	if err != nil || !bytes.Equal(got, fixture) {
		t.Fatalf("wake meta:\n%s\n%s", got, fixture)
	}
	for _, bad := range []struct {
		response string
		revision int64
	}{{"10000000-0000-4000-8000-000000000043", 0}, {"00000000-0000-0000-0000-000000000000", 1}, {"not-a-uuid", 1}} {
		if _, err := FanoutWakeMeta(bad.response, bad.revision); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
	if key := FanoutWakeIdempotencyKey("10000000-0000-4000-8000-000000000043", 2); key != "continue-interrupt/10000000-0000-4000-8000-000000000043/2" {
		t.Fatal(key)
	}
}

// A browser can never request a fanout_wake: it carries no decision.
func TestBrowserContinuationCannotSelectFanoutWake(t *testing.T) {
	request := CurrentContinuationResolveRequest{ProjectID: 1, ActorUserID: 7,
		ConversationUUID: "10000000-0000-4000-8000-000000000001", ResponseMessageID: "10000000-0000-4000-8000-000000000002",
		Kind: CurrentContinuationFanoutWake}
	if request.Validate() == nil {
		t.Fatal("fanout_wake accepted from a browser continuation request")
	}
}
