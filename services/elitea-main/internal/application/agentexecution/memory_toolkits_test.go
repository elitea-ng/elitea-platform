package agentexecution

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
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

type nestedTools map[[2]int64]string

func (n nestedTools) NestedApplicationTools(_ context.Context, project, application, version int64) (json.RawMessage, error) {
	if project != 7 {
		return nil, errors.New("wrong project")
	}
	tools, ok := n[[2]int64{application, version}]
	if !ok {
		return nil, errors.New("no such version")
	}
	return json.RawMessage(tools), nil
}

// The sub-agent walk: A -> B -> C, each level's memory toolkits count, a
// cycle (C -> B) is read once, and a sub-agent that cannot be read fails the
// admission.
func TestRunMemoryToolkitsIncludeSubAgentsRecursively(t *testing.T) {
	t.Parallel()
	service := (&AdmissionService{}).WithNestedMemoryToolkits(nestedTools{
		{31, 41}: `[{"id":5,"type":"memory","settings":{}},{"type":"application","settings":{"application_id":32,"application_version_id":42}}]`,
		{32, 42}: `[{"id":6,"type":"memory","settings":{}},{"type":"application","settings":{"application_id":31,"application_version_id":41}}]`,
	})
	request := SubmitRequest{
		Identity:     executionapp.AdmissionIdentity{ResourceProjectID: "7"},
		CapabilityID: executiondomain.AgentApplicationCapability,
		Input: &runtimev1.AgentExecutionInputV1{Application: []byte(`{"version_details":{"tools":[
		    {"id":9,"type":"memory","settings":{}},
		    {"id":40,"type":"application","settings":{"application_id":31,"application_version_id":41}}]}}`)},
	}
	got, err := service.runMemoryToolkits(context.Background(), request)
	if err != nil || !slices.Equal(got, []int64{5, 6, 9}) {
		t.Fatalf("run memory toolkits = %v, %v; want [5 6 9]", got, err)
	}
	request.Input = &runtimev1.AgentExecutionInputV1{Application: []byte(`{"version_details":{"tools":[
	    {"type":"application","settings":{"application_id":99,"application_version_id":99}}]}}`)}
	if _, err := service.runMemoryToolkits(context.Background(), request); err == nil {
		t.Fatal("an unreadable sub-agent did not fail the admission")
	}
	// Without the reader only the run's own toolkits count.
	request.Input = &runtimev1.AgentExecutionInputV1{Application: []byte(`{"version_details":{"tools":[
	    {"id":9,"type":"memory","settings":{}},
	    {"type":"application","settings":{"application_id":31,"application_version_id":41}}]}}`)}
	if got, err := (&AdmissionService{}).runMemoryToolkits(context.Background(), request); err != nil || !slices.Equal(got, []int64{9}) {
		t.Fatalf("without a reader = %v, %v", got, err)
	}
}
