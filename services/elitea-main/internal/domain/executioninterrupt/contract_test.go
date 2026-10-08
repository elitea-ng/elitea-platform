package executioninterrupt

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const contractDir = "../../../../../libs/jsonschema/runtime/v1"

func fixtures(t *testing.T, stem string) (valid, invalid map[string][]byte) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(contractDir, "fixtures", stem+"-v1*.json"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no %s fixtures: %v", stem, err)
	}
	valid, invalid = map[string][]byte{}, map[string][]byte{}
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		name := filepath.Base(path)
		if strings.Contains(name, ".invalid.") {
			invalid[name] = raw
		} else {
			valid[name] = raw
		}
	}
	if len(valid) == 0 || len(invalid) < 2 {
		t.Fatalf("%s: want valid and at least two invalid fixtures", stem)
	}
	return valid, invalid
}

// The embedded copies are what Main validates with; they must be the contract.
func TestEmbeddedSchemasMatchContract(t *testing.T) {
	entries, err := schemaFiles.ReadDir("schemas")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(schemaStems) {
		t.Fatalf("embedded %d schemas, want %d", len(entries), len(schemaStems))
	}
	for _, stem := range schemaStems {
		embedded, err := schemaFiles.ReadFile("schemas/" + stem + ".schema.json")
		if err != nil {
			t.Fatal(err)
		}
		contract, err := os.ReadFile(filepath.Join(contractDir, stem+".schema.json"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(embedded, contract) {
			t.Errorf("%s: embedded schema differs from libs/jsonschema; copy it again", stem)
		}
	}
}

func TestCardFixtures(t *testing.T) {
	valid, invalid := fixtures(t, "fanout-interrupt-card")
	for name, raw := range valid {
		card, canonical, err := ParseCard(raw)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !bytes.Equal(canonical, raw) {
			t.Errorf("%s: canonical bytes differ from the canonical fixture", name)
		}
		if !ValidDigest(card.InterruptKey) || card.InterruptID == "" || card.Kind == "" || len(card.AvailableActions) == 0 {
			t.Errorf("%s: decoded card is incomplete: %+v", name, card)
		}
	}
	for name, raw := range invalid {
		if _, _, err := ParseCard(raw); !errors.Is(err, ErrInvalidCard) {
			t.Errorf("%s: err=%v, want ErrInvalidCard", name, err)
		}
	}
}

func TestDecisionRequestFixtures(t *testing.T) {
	valid, invalid := fixtures(t, "fanout-interrupt-decision-request")
	for name, raw := range valid {
		decision, canonical, err := ParseDecisionRequest(raw)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !bytes.Equal(canonical, raw) || decision.ExpectedRevision != 1 {
			t.Errorf("%s: decoded %+v canonical %s", name, decision, canonical)
		}
	}
	for name, raw := range invalid {
		if _, _, err := ParseDecisionRequest(raw); !errors.Is(err, ErrInvalidDecision) {
			t.Errorf("%s: err=%v, want ErrInvalidDecision", name, err)
		}
	}
}

func TestAckRequestFixtures(t *testing.T) {
	valid, invalid := fixtures(t, "fanout-interrupt-ack-request")
	for name, raw := range valid {
		ack, canonical, err := ParseAck(raw)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !bytes.Equal(canonical, raw) || ack.Revision < 2 {
			t.Errorf("%s: decoded %+v", name, ack)
		}
		if (ack.Outcome == AckApplied) != (ack.ChildCheckpointID != nil) {
			t.Errorf("%s: outcome %s with checkpoint %v", name, ack.Outcome, ack.ChildCheckpointID)
		}
	}
	for name, raw := range invalid {
		if _, _, err := ParseAck(raw); !errors.Is(err, ErrInvalidAck) {
			t.Errorf("%s: err=%v, want ErrInvalidAck", name, err)
		}
	}
}

// Main recomputes decision_sha256 exactly as the fetch fixture states it.
func TestDecisionSHA256MatchesFetchFixture(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(contractDir, "fixtures", "fanout-interrupt-fetch-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fetch Fetch
	if err := json.Unmarshal(raw, &fetch); err != nil {
		t.Fatal(err)
	}
	if len(fetch.Decisions) != 2 {
		t.Fatalf("fixture decisions = %d", len(fetch.Decisions))
	}
	for _, entry := range fetch.Decisions {
		got, err := DecisionSHA256(entry.InterruptKey, entry.RequestID, entry.Revision, entry.Action, entry.Value, entry.CredentialRef)
		if err != nil || got != entry.DecisionSHA256 {
			t.Errorf("%s: digest %s, fixture %s", entry.InterruptKey, got, entry.DecisionSHA256)
		}
	}
	encoded, err := fetch.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encoded, raw) {
		t.Errorf("Fetch.CanonicalJSON differs from the canonical fixture:\n%s\n%s", encoded, raw)
	}
}

func TestCanonicalJSONForm(t *testing.T) {
	got, err := Canonicalize([]byte("{\"b\":1, \"a\":\"x\\u2028<>&\\u0001\\/\", \"c\":[true,null,{\"z\":-5,\"y\":0}]}"), 1024)
	if err != nil {
		t.Fatal(err)
	}
	want := "{\"a\":\"x <>&\\u0001/\",\"b\":1,\"c\":[true,null,{\"y\":0,\"z\":-5}]}"
	if string(got) != want {
		t.Fatalf("canonical = %s, want %s", got, want)
	}
	deep := strings.Repeat("[", MaxJSONDepth+1) + strings.Repeat("]", MaxJSONDepth+1)
	limitDeep := strings.Repeat("[", MaxJSONDepth) + strings.Repeat("]", MaxJSONDepth)
	if _, err := Canonicalize([]byte(limitDeep), 1024); err != nil {
		t.Errorf("depth %d refused: %v", MaxJSONDepth, err)
	}
	for name, input := range map[string]string{
		"duplicate key":   `{"a":1,"a":2}`,
		"nested dup":      `{"a":{"b":1,"b":1}}`,
		"too deep":        deep,
		"trailing data":   `{"a":1} {}`,
		"fraction":        `{"a":1.0}`,
		"exponent":        `{"a":1e2}`,
		"negative zero":   `{"a":-0}`,
		"over int64":      `{"a":9223372036854775808}`,
		"invalid utf8":    "{\"a\":\"\xff\"}",
		"empty":           ``,
		"over byte limit": `{"a":"` + strings.Repeat("x", 1024) + `"}`,
	} {
		if _, err := Canonicalize([]byte(input), 1024); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func baseEditDecision(value string) []byte {
	return []byte(`{"action":"edit","expected_revision":1,"request_id":"` + strings.Repeat("ab", 32) + `","value":"` + value + `"}`)
}

func TestDecisionBodyAtLimitAndLimitPlusOne(t *testing.T) {
	empty := len(baseEditDecision(""))
	atLimit := baseEditDecision(strings.Repeat("v", MaxDecisionBodyBytes-empty))
	if len(atLimit) != MaxDecisionBodyBytes {
		t.Fatalf("fixture length %d", len(atLimit))
	}
	if _, canonical, err := ParseDecisionRequest(atLimit); err != nil || len(canonical) != MaxDecisionBodyBytes {
		t.Fatalf("body at %d bytes: len=%d err=%v", MaxDecisionBodyBytes, len(canonical), err)
	}
	over := baseEditDecision(strings.Repeat("v", MaxDecisionBodyBytes-empty+1))
	if _, _, err := ParseDecisionRequest(over); !errors.Is(err, ErrInvalidDecision) {
		t.Fatalf("body at %d bytes: err=%v, want ErrInvalidDecision", len(over), err)
	}
	// The schema bounds characters; the byte bound is tighter for multi-byte
	// text and is the one that refuses it.
	if _, _, err := ParseDecisionRequest(baseEditDecision(strings.Repeat("é", 4000))); err != nil {
		t.Fatalf("4000-character two-byte value under the byte limit: err=%v", err)
	}
	if _, _, err := ParseDecisionRequest(baseEditDecision(strings.Repeat("é", 4100))); !errors.Is(err, ErrInvalidDecision) {
		t.Fatalf("4100-character two-byte value over the byte limit: err=%v", err)
	}
	// Tab, LF and CR are allowed in a value; other controls are not.
	if _, _, err := ParseDecisionRequest(baseEditDecision(`a\tb\nc\r`)); err != nil {
		t.Fatalf("tab/LF/CR refused: %v", err)
	}
	if _, _, err := ParseDecisionRequest(baseEditDecision(`a\u0007b`)); !errors.Is(err, ErrInvalidDecision) {
		t.Fatalf("BEL accepted: %v", err)
	}
}

// The raw-token fixture is the schema's proof; a raw token in any extra field
// is refused before anything is stored.
func TestDecisionRejectsUnknownFieldsAndRawTokens(t *testing.T) {
	for _, body := range []string{
		`{"action":"approve","expected_revision":1,"request_id":"` + strings.Repeat("ab", 32) + `","value":"","token":"eyJ"}`,
		`{"action":"approve","expected_revision":1,"request_id":"` + strings.Repeat("ab", 32) + `","value":"","credential_ref":"tsr_4f9a1c2b7d3e8f60"}`,
		`{"action":"approve","expected_revision":0,"request_id":"` + strings.Repeat("ab", 32) + `","value":""}`,
		`{"action":"approve","expected_revision":1,"request_id":"` + strings.Repeat("0", 64) + `","value":""}`,
		`{"action":"approve","expected_revision":1,"request_id":"` + strings.Repeat("AB", 32) + `","value":""}`,
		`{"action":"approve","expected_revision":1,"request_id":"` + strings.Repeat("ab", 32) + `"}`,
		`{"action":"approve","expected_revision":1,"request_id":"` + strings.Repeat("ab", 32) + `","value":null}`,
		`{"action":"approve","action":"reject","expected_revision":1,"request_id":"` + strings.Repeat("ab", 32) + `","value":""}`,
		`[]`,
	} {
		if _, _, err := ParseDecisionRequest([]byte(body)); !errors.Is(err, ErrInvalidDecision) {
			t.Errorf("accepted %s: %v", body, err)
		}
	}
}

func cardWith(t *testing.T, mutate func(map[string]any)) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(contractDir, "fixtures", "fanout-interrupt-card-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var card map[string]any
	if err := json.Unmarshal(raw, &card); err != nil {
		t.Fatal(err)
	}
	mutate(card)
	encoded, err := json.Marshal(card)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := Canonicalize(encoded, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}

func TestRaiseCardAtLimitAndLimitPlusOne(t *testing.T) {
	// "€" is one character and three bytes, so a schema-valid card can pass
	// the byte bound that only Main enforces.
	sized := func(extra int) []byte {
		return cardWith(t, func(card map[string]any) {
			display := card["display"].(map[string]any)
			display["message"] = strings.Repeat("€", 8192)
			display["tool_args_json"] = strings.Repeat("a", 1+extra)
		})
	}
	base := len(sized(0))
	if base >= MaxCardBytes {
		t.Fatalf("base card is %d bytes", base)
	}
	atLimit := sized(MaxCardBytes - base)
	if len(atLimit) != MaxCardBytes {
		t.Fatalf("card length %d", len(atLimit))
	}
	if _, _, err := ParseCard(atLimit); err != nil {
		t.Fatalf("card at %d bytes: %v", MaxCardBytes, err)
	}
	if _, _, err := ParseCard(sized(MaxCardBytes - base + 1)); !errors.Is(err, ErrInvalidCard) {
		t.Fatalf("card at %d bytes: err=%v", MaxCardBytes+1, err)
	}

	// The same card with every message character escaped as \uXXXX is valid
	// although its raw form is far over the canonical bound.
	args := MaxCardBytes - base + 1
	escaped := bytes.Replace(atLimit, []byte(strings.Repeat("€", 8192)), []byte(strings.Repeat(`\u20ac`, 8192)), 1)
	escaped = bytes.Replace(escaped, []byte(`"`+strings.Repeat("a", args)+`"`), []byte(`"`+strings.Repeat(`\u0061`, args)+`"`), 1)
	if len(escaped) <= 2*MaxCardBytes {
		t.Fatalf("escaped fixture is only %d bytes", len(escaped))
	}
	if _, canonical, err := ParseCard(escaped); err != nil || !bytes.Equal(canonical, atLimit) {
		t.Fatalf("escaped card of %d raw bytes: err=%v", len(escaped), err)
	}

	id := func(n int) []byte {
		return cardWith(t, func(card map[string]any) { card["interrupt_id"] = strings.Repeat("i", n) })
	}
	if _, _, err := ParseCard(id(MaxInterruptIDBytes)); err != nil {
		t.Fatalf("interrupt_id at %d: %v", MaxInterruptIDBytes, err)
	}
	if _, _, err := ParseCard(id(MaxInterruptIDBytes + 1)); !errors.Is(err, ErrInvalidCard) {
		t.Fatalf("interrupt_id at %d: %v", MaxInterruptIDBytes+1, err)
	}
}

// JSON Schema cannot tie the member tier to fanout_v1.ordinal; Main does.
func TestCardMemberTierMustCarryTheFanoutOrdinal(t *testing.T) {
	mismatch := cardWith(t, func(card map[string]any) {
		card["parent_agent_path"].([]any)[0].(map[string]any)["sibling_ordinal"] = 2
	})
	if _, _, err := ParseCard(mismatch); !errors.Is(err, ErrInvalidCard) {
		t.Fatalf("sibling_ordinal 2 with fanout ordinal 1: err=%v", err)
	}
}

func TestFrontierBounds(t *testing.T) {
	ok := Frontier{ChildThread: "thread-1", FanoutNode: "research", Ordinal: 3}
	if _, err := ok.CanonicalJSON(); err != nil {
		t.Fatal(err)
	}
	coordinator := Frontier{ChildThread: "root-thread"}
	if _, err := coordinator.CanonicalJSON(); err != nil {
		t.Fatal(err)
	}
	for name, frontier := range map[string]Frontier{
		"empty thread":         {FanoutNode: "n", Ordinal: 1},
		"thread over limit":    {ChildThread: strings.Repeat("t", MaxChildThreadBytes+1), FanoutNode: "n", Ordinal: 1},
		"control in thread":    {ChildThread: "a\nb", FanoutNode: "n", Ordinal: 1},
		"ordinal over 64":      {ChildThread: "t", FanoutNode: "n", Ordinal: MaxFanoutOrdinal + 1},
		"member without node":  {ChildThread: "t", Ordinal: 1},
		"node without ordinal": {ChildThread: "t", FanoutNode: "n"},
		"bad node":             {ChildThread: "t", FanoutNode: "a b", Ordinal: 1},
	} {
		if _, err := frontier.CanonicalJSON(); !errors.Is(err, ErrInvalidFrontier) {
			t.Errorf("%s: err=%v", name, err)
		}
	}
}

func TestActionsAndStates(t *testing.T) {
	if !ActionApprove.Valid() || Action("approve ").Valid() || Action("").Valid() {
		t.Fatal("action vocabulary")
	}
	if ActionApprove.TakesValue() || !ActionEdit.TakesValue() || !ActionContinue.TakesValue() || ActionSkip.TakesValue() {
		t.Fatal("value rule")
	}
	if !StatePending.Open() || !StateDecided.Open() || StateConsumed.Open() || StateCancelled.Open() || StateSuperseded.Open() {
		t.Fatal("open states")
	}
}
