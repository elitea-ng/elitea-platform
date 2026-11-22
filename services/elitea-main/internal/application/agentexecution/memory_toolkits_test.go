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
	got, _, err := service.runToolkitSet(context.Background(), request)
	if err != nil || !slices.Equal(got, []int64{5, 6, 9}) {
		t.Fatalf("run memory toolkits = %v, %v; want [5 6 9]", got, err)
	}
	request.Input = &runtimev1.AgentExecutionInputV1{Application: []byte(`{"version_details":{"tools":[
	    {"type":"application","settings":{"application_id":99,"application_version_id":99}}]}}`)}
	if _, _, err := service.runToolkitSet(context.Background(), request); err == nil {
		t.Fatal("an unreadable sub-agent did not fail the admission")
	}
	// Without the reader only the run's own toolkits count.
	request.Input = &runtimev1.AgentExecutionInputV1{Application: []byte(`{"version_details":{"tools":[
	    {"id":9,"type":"memory","settings":{}},
	    {"type":"application","settings":{"application_id":31,"application_version_id":41}}]}}`)}
	if got, _, err := (&AdmissionService{}).runToolkitSet(context.Background(), request); err != nil || !slices.Equal(got, []int64{9}) {
		t.Fatalf("without a reader = %v, %v", got, err)
	}
}

// The run's toolkit types are recorded for the kill switch: the canonical
// type of every frozen entry, its sub-agents' included, sorted and unique.
// FrozenToolkitTypes reads only the run's own set (the legacy backfill).
func TestRunToolkitTypesIncludeSubAgents(t *testing.T) {
	t.Parallel()
	service := (&AdmissionService{}).WithNestedMemoryToolkits(nestedTools{
		{31, 41}: `[{"id":5,"type":"Jira","settings":{}},{"type":"application","settings":{"application_id":31,"application_version_id":41}}]`,
	})
	request := SubmitRequest{
		Identity:     executionapp.AdmissionIdentity{ResourceProjectID: "7"},
		CapabilityID: executiondomain.AgentApplicationCapability,
		Input: &runtimev1.AgentExecutionInputV1{Application: []byte(`{"version_details":{"tools":[
		    {"id":9,"type":"github","settings":{}},
		    {"id":10,"type":"github","settings":{}},
		    {"id":40,"type":"application","settings":{"application_id":31,"application_version_id":41}}]}}`)},
	}
	_, types, err := service.runToolkitSet(context.Background(), request)
	if err != nil || !slices.Equal(types, []string{"application", "github", "jira"}) {
		t.Fatalf("run toolkit types = %v, %v", types, err)
	}
	own, err := FrozenToolkitTypes(request.CapabilityID, request.Input)
	if err != nil || !slices.Equal(own, []string{"application", "github"}) {
		t.Fatalf("own toolkit types = %v, %v", own, err)
	}
	empty, err := FrozenToolkitTypes(executiondomain.AgentAdhocCapability, &runtimev1.AgentExecutionInputV1{Tools: []byte(`[]`)})
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("no toolkits = %v, %v; want an empty, non-nil list", empty, err)
	}
}

type stubHiddenMemoryToolkits struct {
	hidden     []int64
	hiddenApps []int64
	err        error
	appErr     error
}

func (s stubHiddenMemoryToolkits) HiddenToolkits(_ context.Context, _, _ int64, _ []int64) ([]int64, error) {
	return s.hidden, s.err
}

func (s stubHiddenMemoryToolkits) HiddenApplications(_ context.Context, _, _ int64, _ []int64) ([]int64, error) {
	return s.hiddenApps, s.appErr
}

// FU-353 defense-in-depth + FU-353-07: memory toolkits hidden from the acting
// user and memory toolkits reachable only through folder-hidden intermediate
// sub-agents are excluded from execution_jobs.memory_toolkit_ids; filter read
// failures and malformed non-numeric actor IDs fail admission closed.
func TestRunMemoryToolkitsExcludePrincipalHiddenToolkits(t *testing.T) {
	t.Parallel()
	nested := nestedTools{
		{31, 41}: `[{"id":5,"type":"memory","settings":{}},{"id":6,"type":"memory","settings":{}},{"type":"application","settings":{"application_id":32,"application_version_id":42}}]`,
		{32, 42}: `[{"id":7,"type":"memory","settings":{}}]`,
	}
	request := SubmitRequest{
		Identity:     executionapp.AdmissionIdentity{ResourceProjectID: "7", ActorID: "42"},
		CapabilityID: executiondomain.AgentApplicationCapability,
		Input: &runtimev1.AgentExecutionInputV1{Application: []byte(`{"version_details":{"tools":[
		    {"id":9,"type":"memory","settings":{}},
		    {"id":40,"type":"application","settings":{"application_id":31,"application_version_id":41}}]}}`)},
	}

	// Hidden nested toolkit 5 and hidden root toolkit 9 are stripped; visible 6 and 7 remain.
	service := (&AdmissionService{}).
		WithNestedMemoryToolkits(nested).
		WithHiddenMemoryToolkits(stubHiddenMemoryToolkits{hidden: []int64{5, 9}})
	got, _, err := service.runToolkitSet(context.Background(), request)
	if err != nil || !slices.Equal(got, []int64{6, 7}) {
		t.Fatalf("filtered nested = %v, %v; want [6 7]", got, err)
	}

	// FU353-07: hidden intermediate sub-agent 31 skips both 31's own toolkits
	// (5, 6) and its grandchild 32's toolkit (7); only root toolkit 9 remains.
	hiddenMid := (&AdmissionService{}).
		WithNestedMemoryToolkits(nested).
		WithHiddenMemoryToolkits(stubHiddenMemoryToolkits{hiddenApps: []int64{31}})
	got, _, err = hiddenMid.runToolkitSet(context.Background(), request)
	if err != nil || !slices.Equal(got, []int64{9}) {
		t.Fatalf("FU353-07 hidden intermediate sub-agent = %v, %v; want [9]", got, err)
	}

	// FU353-07: malformed non-numeric ActorID fails admission closed.
	badActor := request
	badActor.Identity.ActorID = "svc-non-numeric"
	if _, _, err := service.runToolkitSet(context.Background(), badActor); !errors.Is(err, ErrInvalidAgentAdmission) {
		t.Fatalf("FU353-07 malformed actor: want ErrInvalidAgentAdmission, got %v", err)
	}

	// Root-only path also strips hidden toolkits and keeps non-nil empty slice.
	rootOnly := SubmitRequest{
		Identity:     executionapp.AdmissionIdentity{ResourceProjectID: "7", ActorID: "42"},
		CapabilityID: executiondomain.AgentApplicationCapability,
		Input:        &runtimev1.AgentExecutionInputV1{Application: []byte(`{"version_details":{"tools":[{"id":9,"type":"memory","settings":{}}]}}`)},
	}
	got, _, err = (&AdmissionService{}).WithHiddenMemoryToolkits(stubHiddenMemoryToolkits{hidden: []int64{9}}).runToolkitSet(context.Background(), rootOnly)
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("all hidden root = %v, %v; want non-nil empty slice", got, err)
	}

	// Filter error fails admission closed (both toolkit filter and hidden-app filter).
	failing := (&AdmissionService{}).
		WithNestedMemoryToolkits(nested).
		WithHiddenMemoryToolkits(stubHiddenMemoryToolkits{err: errors.New("folder access unavailable")})
	if _, _, err := failing.runToolkitSet(context.Background(), request); err == nil {
		t.Fatal("toolkit filter error did not fail admission closed")
	}
	failingApps := (&AdmissionService{}).
		WithNestedMemoryToolkits(nested).
		WithHiddenMemoryToolkits(stubHiddenMemoryToolkits{appErr: errors.New("folder access unavailable")})
	if _, _, err := failingApps.runToolkitSet(context.Background(), request); err == nil {
		t.Fatal("hidden-app filter error did not fail admission closed")
	}
}
