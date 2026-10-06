package noderecovery

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNodeRecoveryProductionExecutionRequestFixture(t *testing.T) {
	raw, err := os.ReadFile("../../../../../libs/jsonschema/runtime/v1/fixtures/node-recovery-request-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	request, err := DecodeRequest(raw)
	if err != nil || request.ExecutionID != "0123456789abcdef0123456789abcdef" || request.Generation != 1 || request.ExpectedRevision != 3 || request.Action != "retry" {
		t.Fatal(request, err)
	}
}

func TestNodeRecoveryExecutionSchemasFollowProductionGrammar(t *testing.T) {
	paths, err := filepath.Glob("../../../../../libs/jsonschema/runtime/v1/node-recovery*.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var schema struct {
			Properties map[string]map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatal(path, err)
		}
		execution, exists := schema.Properties["execution_id"]
		if !exists {
			continue
		}
		checked++
		if string(execution["type"]) != `"string"` || string(execution["pattern"]) != `"^[0-9a-f]{32}$"` || string(execution["minLength"]) != "32" || string(execution["maxLength"]) != "32" || execution["format"] != nil || execution["not"] != nil {
			t.Fatal("execution schema differs from production identifier", path, execution)
		}
	}
	if checked != 8 {
		t.Fatal("missing execution schema coverage", checked)
	}
}

func TestNodeRecoveryExecutionIdentifiersMatchMainRuntimeAndStaySeparateFromResponses(t *testing.T) {
	for _, value := range []string{"0123456789abcdef0123456789abcdef", strings.Repeat("0", 32)} {
		if !ValidExecutionID(value) || ValidResponseMessageID(value) {
			t.Fatal(value)
		}
	}
	for _, value := range []string{"10000000-0000-4000-8000-000000000041", "0123456789abcdef0123456789abcde", "0123456789abcdef0123456789abcdef0", "0123456789abcdeF0123456789abcdef", "0123456789abcdeg0123456789abcdef", " 0123456789abcdef0123456789abcdef", "0123456789abcdef0123456789abcdef\n"} {
		if ValidExecutionID(value) {
			t.Fatal("accepted malformed execution", value)
		}
	}
	if !ValidResponseMessageID("10000000-0000-4000-8000-000000000043") || ValidResponseMessageID("00000000-0000-0000-0000-000000000000") {
		t.Fatal("responseUUID boundary")
	}
}
