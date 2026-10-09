package executioninterrupt

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func schemaNode(t *testing.T, stem string, path ...string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(contractDir, stem+".schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var node any
	if err := json.Unmarshal(raw, &node); err != nil {
		t.Fatal(err)
	}
	for _, key := range path {
		object, ok := node.(map[string]any)
		if !ok {
			t.Fatalf("%s: %s is not an object", stem, strings.Join(path, "."))
		}
		node = object[key]
	}
	object, ok := node.(map[string]any)
	if !ok {
		t.Fatalf("%s: %s missing", stem, strings.Join(path, "."))
	}
	return object
}

func schemaInt(t *testing.T, node map[string]any, key string) int {
	t.Helper()
	value, ok := node[key].(float64)
	if !ok {
		t.Fatalf("schema bound %s missing", key)
	}
	return int(value)
}

// TestExecutionInterruptLimitsMatchContractSchemas is the drift guard named in
// the contract §12: every Main constant a schema can express equals it.
func TestExecutionInterruptLimitsMatchContractSchemas(t *testing.T) {
	for _, check := range []struct {
		name string
		got  int
		node map[string]any
		key  string
	}{
		{"MaxOpenInterrupts", MaxOpenInterrupts, schemaNode(t, "fanout-interrupt-list", "properties", "interrupts"), "maxItems"},
		{"MaxDecisionValueChars", MaxDecisionValueChars, schemaNode(t, "fanout-interrupt-decision-request", "properties", "value"), "maxLength"},
		{"MaxInterruptIDBytes", MaxInterruptIDBytes, schemaNode(t, "fanout-interrupt-card", "properties", "interrupt_id"), "maxLength"},
		{"MaxAvailableActions", MaxAvailableActions, schemaNode(t, "fanout-interrupt-card", "properties", "available_actions"), "maxItems"},
		{"MaxHierarchyTiers", MaxHierarchyTiers, schemaNode(t, "fanout-hierarchy", "properties", "parent_agent_path"), "maxItems"},
		{"MaxFanoutOrdinal", MaxFanoutOrdinal, schemaNode(t, "fanout-member", "properties", "ordinal"), "maximum"},
		{"MaxFetchDecisions", MaxFetchDecisions, schemaNode(t, "fanout-interrupt-fetch", "properties", "decisions"), "maxItems"},
		{"MinCredentialRefBytes", MinCredentialRefBytes, schemaNode(t, "fanout-interrupt-decision-request", "properties", "credential_ref"), "minLength"},
		{"MaxCredentialRefBytes", MaxCredentialRefBytes, schemaNode(t, "fanout-interrupt-decision-request", "properties", "credential_ref"), "maxLength"},
	} {
		if want := schemaInt(t, check.node, check.key); check.got != want {
			t.Errorf("%s = %d, schema %s = %d", check.name, check.got, check.key, want)
		}
	}

	checkpoint := schemaNode(t, "fanout-interrupt-ack-request", "properties", "child_checkpoint_id")["oneOf"].([]any)[0].(map[string]any)
	if want := schemaInt(t, checkpoint, "maxLength"); MaxCheckpointIDBytes != want {
		t.Errorf("MaxCheckpointIDBytes = %d, schema %d", MaxCheckpointIDBytes, want)
	}
	// Byte bounds live in schema descriptions and the contract text.
	for stem, phrase := range map[string]string{
		"fanout-interrupt-decision-request": "at most 8192 bytes",
		"fanout-interrupt-fetch":            "at most 65536 bytes",
		"fanout-interrupt-list":             "at most 16 are open",
	} {
		if !strings.Contains(schemaNode(t, stem)["description"].(string), phrase) {
			t.Errorf("%s description no longer says %q; update limits.go with the contract", stem, phrase)
		}
	}
	contract, err := os.ReadFile("../../../../../libs/proto/contracts/fanout-interrupt-decisions-v1.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, phrase := range []string{"card is at most 32 KiB", "card_json (canonical card bytes, ≤32768)", "decision_json (canonical decision body, ≤8192"} {
		if !strings.Contains(string(contract), phrase) {
			t.Errorf("contract no longer says %q; update limits.go with it", phrase)
		}
	}
	if MaxDecisionBodyBytes != 8192 || MaxCardBytes != 32768 || MaxFetchEntryBytes != 65536 {
		t.Error("byte bounds differ from the contract text checked above")
	}
}
