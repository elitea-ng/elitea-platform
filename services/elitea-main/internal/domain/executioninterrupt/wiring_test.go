package executioninterrupt

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func contractSchema(t *testing.T, stems ...string) *jsonschema.Schema {
	t.Helper()
	compiler := jsonschema.NewCompiler()
	for _, stem := range append([]string{"fanout-member", "fanout-hierarchy"}, stems...) {
		raw, err := os.ReadFile(filepath.Join(contractDir, stem+".schema.json"))
		if err != nil {
			t.Fatal(err)
		}
		document, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		if err := compiler.AddResource(schemaBaseURL+"elitea.pipeline."+stem+".v1", document); err != nil {
			t.Fatal(err)
		}
	}
	schema, err := compiler.Compile(schemaBaseURL + "elitea.pipeline." + stems[0] + ".v1")
	if err != nil {
		t.Fatal(err)
	}
	return schema
}

func TestCardMemberFrontierIsTheMemberTier(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(contractDir, "fixtures", "fanout-interrupt-card-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	card, _, err := ParseCard(raw)
	if err != nil {
		t.Fatal(err)
	}
	frontier, err := card.MemberFrontier()
	if err != nil || frontier.ChildThread != "fo1_fd47d5a520dc10ff429e8c38e13656305daf31781c78c477db942ad604db4031" ||
		frontier.FanoutNode != "research" || frontier.Ordinal != 1 {
		t.Fatalf("frontier = %+v, %v", frontier, err)
	}
	if _, err := frontier.CanonicalJSON(); err != nil {
		t.Fatalf("derived frontier does not fit the stored bounds: %v", err)
	}
	coordinator, err := os.ReadFile(filepath.Join(contractDir, "fixtures", "fanout-interrupt-card-v1.coordinator.json"))
	if err != nil {
		t.Fatal(err)
	}
	parsed, _, err := ParseCard(coordinator)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parsed.MemberFrontier(); err == nil {
		t.Fatal("a coordinator card produced a member frontier")
	}
	// A member tier whose call id is not fo1_<64 hex> is refused.
	bad := bytes.Replace(raw, []byte(`"call_id":"fo1_fd47`), []byte(`"call_id":"fo1_XX47`), 1)
	if _, _, err := ParseCard(bad); err == nil {
		t.Fatal("malformed member call id accepted")
	}
}

func TestResolvedMetadataMatchesTheContract(t *testing.T) {
	schema := contractSchema(t, "fanout-interrupt-resolved")
	answer := ActionAnswer
	for name, resolved := range map[string]Resolved{
		"decided":    {InterruptKey: strings.Repeat("ab", 32), InterruptID: "ask:1", State: StateDecided, Revision: 2, Action: &answer},
		"cancelled":  {InterruptKey: strings.Repeat("ab", 32), InterruptID: "ask:1", State: StateCancelled, Revision: 3},
		"superseded": {InterruptKey: strings.Repeat("ab", 32), InterruptID: "ask:1", State: StateSuperseded, Revision: 2},
	} {
		encoded, err := resolved.ResolvedMetadataJSON()
		if err != nil {
			t.Fatal(name, err)
		}
		value, err := jsonschema.UnmarshalJSON(bytes.NewReader(encoded))
		if err != nil || schema.Validate(value) != nil {
			t.Fatalf("%s: %s does not validate: %v", name, encoded, schema.Validate(value))
		}
	}
	fixture, err := os.ReadFile(filepath.Join(contractDir, "fixtures", "fanout-interrupt-resolved-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var want struct {
		InterruptKey string `json:"interrupt_key"`
		InterruptID  string `json:"interrupt_id"`
		Action       Action `json:"action"`
		Revision     int64  `json:"revision"`
	}
	if err := json.Unmarshal(fixture, &want); err != nil {
		t.Fatal(err)
	}
	got, err := Resolved{InterruptKey: want.InterruptKey, InterruptID: want.InterruptID, State: StateDecided, Revision: want.Revision, Action: &want.Action}.ResolvedMetadataJSON()
	if err != nil || !bytes.Equal(got, fixture) {
		t.Fatalf("canonical resolved metadata differs from the fixture:\n%s\n%s", got, fixture)
	}
	for name, invalid := range map[string]Resolved{
		"decided without action":  {InterruptKey: strings.Repeat("ab", 32), InterruptID: "x", State: StateDecided, Revision: 2},
		"cancel with action":      {InterruptKey: strings.Repeat("ab", 32), InterruptID: "x", State: StateCancelled, Revision: 2, Action: &answer},
		"consumed is not emitted": {InterruptKey: strings.Repeat("ab", 32), InterruptID: "x", State: StateConsumed, Revision: 3},
	} {
		if _, err := invalid.ResolvedMetadataJSON(); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestAckResultIsTheCanonicalFixture(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join(contractDir, "fixtures", "fanout-interrupt-ack-result-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := AckResult{InterruptKey: "93ba421dbbbcc4f07e5b3366f081d1ef3ac983c422a3b9e48ed1fbe05b2d6a87", State: StateConsumed, Revision: 3}.CanonicalJSON()
	if err != nil || !bytes.Equal(got, fixture) {
		t.Fatalf("ack result:\n%s\n%s", got, fixture)
	}
	if _, err := (AckResult{InterruptKey: strings.Repeat("ab", 32), State: StateDecided, Revision: 2}).CanonicalJSON(); err == nil {
		t.Fatal("an ACK result in state DECIDED was written")
	}
}

// TestFetchEntryAtLimitAndLimitPlusOne: one canonical fetch entry is bounded
// at 65536 bytes. A valid decision body (<= 8192 bytes) can never reach it;
// the bound is a defensive check on the stored row, proven here exactly.
func TestFetchEntryAtLimitAndLimitPlusOne(t *testing.T) {
	entry := FetchedDecision{InterruptKey: strings.Repeat("ab", 32), InterruptID: "ask:1", Revision: 2,
		RequestID: strings.Repeat("cd", 32), Action: ActionAnswer, DecisionSHA256: strings.Repeat("ef", 32)}
	size, err := entry.CanonicalSize()
	if err != nil {
		t.Fatal(err)
	}
	entry.Value = strings.Repeat("v", MaxFetchEntryBytes-size)
	if size, _ := entry.CanonicalSize(); size != MaxFetchEntryBytes || !entry.WithinFetchLimit() {
		t.Fatalf("at the limit: %d", size)
	}
	entry.Value += "v"
	if entry.WithinFetchLimit() {
		t.Fatal("limit+1 entry accepted")
	}
}
