package workeroutputcontract

import (
	"context"
	"testing"

	"google.golang.org/protobuf/proto"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
)

type inputs map[string][]byte

func (i inputs) InputEntries(_ context.Context, _ string, ids []string) (map[string][]byte, error) {
	out := map[string][]byte{}
	for _, id := range ids {
		out[id] = i[id]
	}
	return out, nil
}

func entry(t *testing.T, application string) []byte {
	t.Helper()
	encoded, err := proto.Marshal(&runtimev1.AgentExecutionInputV1{Application: []byte(application)})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func command() *runtimev1.WorkerCommandV1 {
	return &runtimev1.WorkerCommandV1{
		InputBundleRef: &runtimev1.ExecutionInputBundleReferenceV1{InputBundleId: "b"},
		CapabilityCommand: &runtimev1.WorkerCommandV1_AgentExecution{
			AgentExecution: &runtimev1.AgentExecutionCommandV1{RequestEntryId: "e"},
		},
	}
}

// ADR-0043 decision 2: only a command whose frozen definition holds a
// contract carries field 70, so an older worker refuses exactly those.
func TestFieldSeventyOnlyForAContract(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		application string
		want        bool
	}{
		{"no contract", `{"version_details":{"meta":{"step_limit":5},"instructions":"Be brief."}}`, false},
		{"null contract", `{"version_details":{"meta":{"output_contract":null}}}`, false},
		{"text that names the key", `{"version_details":{"instructions":"Never print output_contract."}}`, false},
		{"agent", `{"version_details":{"meta":{"output_contract":{"type":"format","format":"json"}}}}`, true},
		{"nested agent", `{"version_details":{"tools":[{"settings":{"version_details":{"meta":{"output_contract":{"type":"format","format":"csv"}}}}}]}}`, true},
		{"pipeline node", `{"version_details":{"instructions":"nodes:\n  - id: A\n    type: llm\n    output_contract: {type: format, format: json}\n"}}`, true},
	} {
		resolver, err := NewResolver(inputs{"e": entry(t, tc.application)})
		if err != nil {
			t.Fatal(err)
		}
		got, err := resolver.WorkerOutputContracts(context.Background(), command())
		if err != nil {
			t.Fatal(err)
		}
		if (got != nil) != tc.want || (got != nil && got.GetRevision() != Revision) {
			t.Fatalf("%s: field 70 = %v, want present %t", tc.name, got, tc.want)
		}
	}
	resolver, _ := NewResolver(inputs{})
	if got, _ := resolver.WorkerOutputContracts(context.Background(), &runtimev1.WorkerCommandV1{}); got != nil {
		t.Fatal("a command that is not an agent execution got field 70")
	}
}
