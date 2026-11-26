package auditexport

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

)

// signalFixtureNames is the set of golden fixtures for the signal stream.
var signalFixtureNames = []string{
	"security_signal_full.golden.json",
	"security_signal_minimal.golden.json",
	"platform_sink_test.golden.json",
	"chain_checkpoint.golden.json",
}

const signalFixturesDir = "../../../../libs/jsonschema/audit/v1/fixtures"

func loadSignalFixture(t *testing.T, name string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(signalFixturesDir, name))
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestSignalFixturesValidate(t *testing.T) {
	schema := compileSchema(t)
	for _, name := range signalFixtureNames {
		t.Run(name, func(t *testing.T) {
			v := loadSignalFixture(t, name)
			if err := validate(t, schema, v); err != nil {
				t.Fatalf("golden fixture does not validate: %v", err)
			}
		})
	}
}

func TestSignalFixtureRoundTrip(t *testing.T) {
	for _, name := range signalFixtureNames {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(signalFixturesDir, name))
			if err != nil {
				t.Fatal(err)
			}
			var v map[string]any
			if err := json.Unmarshal(raw, &v); err != nil {
				t.Fatal(err)
			}
			var event Event
			if err := json.Unmarshal(raw, &event); err != nil {
				t.Fatal(err)
			}
			// Re-marshal through the Go types: the fields that the schema
			// defines survive the round-trip.
			marshaled, err := json.Marshal(event)
			if err != nil {
				t.Fatal(err)
			}
			var roundTrip map[string]any
			if err := json.Unmarshal(marshaled, &roundTrip); err != nil {
				t.Fatal(err)
			}
			schema := compileSchema(t)
			if err := validate(t, schema, roundTrip); err != nil {
				t.Fatalf("event does not validate after a Go round-trip: %v", err)
			}
			if reflect.TypeOf(v) == nil || reflect.TypeOf(roundTrip) == nil {
				t.Fatal("fixture and round trip did not decode as maps")
			}
			if !jsonSemanticEqual(v, roundTrip) {
				t.Fatalf("round trip differs from the fixture:\n got %s\nwant %s", mustJSON(t, roundTrip), mustJSON(t, v))
			}
		})
	}
}

func TestSignalTypePattern(t *testing.T) {
	schema := compileSchema(t)
	full := loadSignalFixture(t, "security_signal_full.golden.json")
	// A new, unregistered type that matches the pattern validates: the
	// signal.type changes, as a real forward-compatible rollout would.
	signal := full["signal"].(map[string]any)
	signal["type"] = "derived.my_new_type"
	signal["category"] = "derived"
	full["event_type"] = "security_signal.derived.my_new_type"
	full["action"] = "derived.my_new_type"
	if err := validate(t, schema, full); err != nil {
		t.Fatalf("an unknown but well-formed signal type was refused: %v", err)
	}
	// A signal type that does not match the pattern is refused.
	bad := loadSignalFixture(t, "security_signal_full.golden.json")
	signalBad := bad["signal"].(map[string]any)
	signalBad["type"] = "Not-A_dotted_name"
	if err := validate(t, schema, bad); err == nil {
		t.Fatal("a signal type that does not match the pattern was accepted")
	}
}

func TestSignalBoundsEnforced(t *testing.T) {
	schema := compileSchema(t)
	cases := map[string]func(map[string]any){
		"17 labels": func(v map[string]any) {
			signal := v["signal"].(map[string]any)
			labels := make([]string, 17)
			for i := range labels {
				labels[i] = "label"
			}
			signal["labels"] = labels
		},
		"21 folded ids": func(v map[string]any) {
			signal := v["signal"].(map[string]any)
			folded := make([]string, 21)
			for i := range folded {
				folded[i] = "0192f0c1-7b6e-7c3a-8f00-00000000000a"
			}
			signal["folded"] = folded
		},
		"unknown disposition": func(v map[string]any) {
			v["signal"].(map[string]any)["disposition"] = "frobnicate"
		},
		"unknown severity": func(v map[string]any) {
			v["signal"].(map[string]any)["severity"] = "urgent"
		},
		"unknown provenance kind": func(v map[string]any) {
			v["signal"].(map[string]any)["provenance"] = map[string]any{"kind": "the_future"}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			v := loadSignalFixture(t, "security_signal_full.golden.json")
			mutate(v)
			if err := validate(t, schema, v); err == nil {
				t.Fatal("schema accepted a signal that violates a bound")
			}
		})
	}
}

func TestSignalChainRules(t *testing.T) {
	schema := compileSchema(t)
	// A platform.sink_test signal carries no chain.
	test := loadSignalFixture(t, "platform_sink_test.golden.json")
	if err := validate(t, schema, test); err != nil {
		t.Fatalf("platform.sink_test with no chain was refused: %v", err)
	}
	// Any other signal with no chain is refused.
	minimal := loadSignalFixture(t, "security_signal_minimal.golden.json")
	delete(minimal, "chain")
	if err := validate(t, schema, minimal); err == nil {
		t.Fatal("a signal event without a chain was accepted")
	}
}

func TestSignalOnNonSignalSourceRefused(t *testing.T) {
	schema := compileSchema(t)
	for _, name := range []string{"admin_audit_role_use", "admin_audit_no_roles"} {
		t.Run(name, func(t *testing.T) {
			v := map[string]any{}
			raw, err := os.ReadFile(filepath.Join("../../../../libs/jsonschema/audit/v1/testdata", name+".golden.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(raw, &v); err != nil {
				t.Fatal(err)
			}
			v["signal"] = map[string]any{"type": "access.authn_failed", "taxonomy_version": "1.0.0", "category": "access", "severity": "medium"}
			if err := validate(t, schema, v); err == nil {
				t.Fatal("a non-security_signal event carrying a signal object was accepted")
			}
		})
	}
}

func TestSignalFixtureRoundTripMarshalStable(t *testing.T) {
	// Guard: the Go round trip of a full signal fixture is deterministic.
	full := loadSignalFixture(t, "security_signal_full.golden.json")
	var event Event
	raw, _ := json.Marshal(full)
	if err := json.Unmarshal(raw, &event); err != nil {
		t.Fatal(err)
	}
	first, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	var again Event
	if err := json.Unmarshal(first, &again); err != nil {
		t.Fatal(err)
	}
	second, err := json.Marshal(again)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("round trip is not stable:\n%s\n%s", first, second)
	}
}

func TestSignalCheckpointEventRule(t *testing.T) {
	schema := compileSchema(t)
	// A chain.checkpoint event carries no signal and no chain.
	v := loadSignalFixture(t, "chain_checkpoint.golden.json")
	if err := validate(t, schema, v); err != nil {
		t.Fatalf("chain.checkpoint was refused: %v", err)
	}
	// A checkpoint with a signal is refused.
	vWithSignal := loadSignalFixture(t, "chain_checkpoint.golden.json")
	vWithSignal["signal"] = map[string]any{"type": "access.authn_failed", "taxonomy_version": "1.0.0", "category": "access", "severity": "medium"}
	if err := validate(t, schema, vWithSignal); err == nil {
		t.Fatal("a chain.checkpoint event carrying a signal was accepted")
	}
	// A checkpoint with a chain is refused.
	vWithChain := loadSignalFixture(t, "chain_checkpoint.golden.json")
	vWithChain["chain"] = map[string]any{"partition": "3", "sequence": 1, "prev_hash": strings.Repeat("0", 64), "hash": strings.Repeat("2", 64)}
	if err := validate(t, schema, vWithChain); err == nil {
		t.Fatal("a chain.checkpoint event carrying a chain was accepted")
	}
}

func TestSignalEventTypeIsFreeForm(t *testing.T) {
	// The event_type field is free-form; the signal.type pattern is what bounds
	// the dotted name.
	schema := compileSchema(t)
	v := loadSignalFixture(t, "security_signal_full.golden.json")
	v["event_type"] = "security_signal.content.prompt_injection"
	if err := validate(t, schema, v); err != nil {
		t.Fatalf("a checkpoint-like event_type was refused: %v", err)
	}
}

func TestSignalUnmappedSourcesRejectSignal(t *testing.T) {
	// security_audit and run_step events must not carry the signal object.
	schema := compileSchema(t)
	v := map[string]any{
		"schema": "elitea.audit.v1", "event_id": "0192f0c1-7b6e-7c3a-8f00-000000000050",
		"time": "2026-11-25T09:30:00Z", "source": "run_step", "event_type": "run_step.model_call",
		"tenant": map[string]any{"project_id": 3},
		"actor":  map[string]any{"type": "agent", "id": "agent-1"},
		"action": "run", "outcome": "success",
		"chain":   map[string]any{"partition": "run:run-1", "sequence": 1, "prev_hash": strings.Repeat("0", 64), "hash": strings.Repeat("1", 64)},
		"signal":  map[string]any{"type": "access.authn_failed", "taxonomy_version": "1.0.0", "category": "access", "severity": "medium"},
		"details": map[string]any{},
	}
	if err := validate(t, schema, v); err == nil {
		t.Fatal("a run_step event carrying a signal object was accepted")
	}
}


// jsonSemanticEqual compares two decoded JSON values semantically: encoding/json
// sorts map keys, so re-marshalled documents are canonically comparable while
// key order is free. It is not a strict type check: the schema validation is the
// strict check, this is the dropped-field check.
func jsonSemanticEqual(a, b any) bool {
	aj, err := json.Marshal(a)
	if err != nil {
		return false
	}
	bj, err := json.Marshal(b)
	if err != nil {
		return false
	}
	return string(aj) == string(bj)
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
