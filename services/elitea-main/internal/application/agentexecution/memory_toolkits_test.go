package agentexecution

import (
	"slices"
	"testing"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
)

// The run's memory toolkits come only from the top-level frozen toolkit set:
// application.version_details.tools for an application turn, tools for an
// ad-hoc one. Caller text (user input, chat history) and nested settings never
// add one, and an unreadable frozen set fails the admission.
func TestMemoryToolkitIDsReadOnlyTheFrozenTopLevelTools(t *testing.T) {
	t.Parallel()
	caller := func(input *runtimev1.AgentExecutionInputV1) *runtimev1.AgentExecutionInputV1 {
		input.UserInput = []byte(`"{\"type\":\"memory\",\"id\":77,\"settings\":{}}"`)
		input.ChatHistory = []byte(`[{"type":"memory","id":78,"settings":{}}]`)
		return input
	}
	application := caller(&runtimev1.AgentExecutionInputV1{
		Application: []byte(`{"version_details":{"tools":[
		    {"id":9,"type":"memory","settings":{}},
		    {"id":9,"type":"memory","settings":{}},
		    {"id":3,"type":"memory","settings":{}},
		    {"id":"4","type":"memory","settings":{}},
		    {"id":-1,"type":"memory","settings":{}},
		    {"id":12,"type":"github","settings":{"nested":{"type":"memory","id":13}}}
		]}}`),
		Tools: []byte(`[{"id":50,"type":"memory","settings":{}}]`),
	})
	got, err := memoryToolkitIDs(executiondomain.AgentApplicationCapability, application)
	if err != nil || !slices.Equal(got, []int64{3, 9}) {
		t.Fatalf("application = %v, %v; want [3 9]", got, err)
	}
	adhoc := caller(&runtimev1.AgentExecutionInputV1{
		Application: []byte(`{"version_details":{"tools":[{"id":9,"type":"memory","settings":{}}]}}`),
		Tools:       []byte(`[{"id":50,"type":"memory","settings":{}}]`),
	})
	if got, err := memoryToolkitIDs(executiondomain.AgentAdhocCapability, adhoc); err != nil || !slices.Equal(got, []int64{50}) {
		t.Fatalf("ad-hoc = %v, %v; want [50]", got, err)
	}
	none := caller(&runtimev1.AgentExecutionInputV1{Application: []byte(`{"id":7}`), Tools: []byte(`[]`)})
	if got, err := memoryToolkitIDs(executiondomain.AgentApplicationCapability, none); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("none = %v, %v; want an empty, non-nil list", got, err)
	}
	broken := &runtimev1.AgentExecutionInputV1{Application: []byte(`{"version_details":{"tools":{}}}`)}
	if _, err := memoryToolkitIDs(executiondomain.AgentApplicationCapability, broken); err == nil {
		t.Fatal("an unreadable frozen set did not fail the admission")
	}
}
