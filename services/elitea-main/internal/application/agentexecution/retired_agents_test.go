package agentexecution

import (
	"context"
	"errors"
	"testing"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentworkforce"
	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
)

type retiredSet map[int64]bool

func (r retiredSet) RetiredAgents(_ context.Context, project int64, ids []int64) (map[int64]bool, error) {
	if project != 7 {
		return nil, errors.New("wrong project")
	}
	out := map[int64]bool{}
	for _, id := range ids {
		if r[id] {
			out[id] = true
		}
	}
	return out, nil
}

// A run of a retired agent is refused with agent_retired; a run whose
// sub-agents (at any depth the walk reads) include a retired one with
// nested_agent_retired (ADR-0050 decision 5g).
func TestAdmissionRefusesRetiredAndNestedRetiredAgents(t *testing.T) {
	t.Parallel()
	request := func(own int64) SubmitRequest {
		return SubmitRequest{
			Identity:     executionapp.AdmissionIdentity{ResourceProjectID: "7"},
			CapabilityID: executiondomain.AgentApplicationCapability,
			CurrentTurn:  &CurrentApplicationTurn{ApplicationID: own},
			Input: &runtimev1.AgentExecutionInputV1{Application: []byte(`{"version_details":{"tools":[
			    {"id":40,"type":"application","settings":{"application_id":31,"application_version_id":41}}]}}`)},
		}
	}
	nested := nestedTools{
		{31, 41}: `[{"type":"application","settings":{"application_id":32,"application_version_id":42}}]`,
		{32, 42}: `[]`,
	}

	service := (&AdmissionService{}).WithNestedMemoryToolkits(nested).WithRetiredAgents(retiredSet{30: true})
	var retired *agentworkforce.RetiredError
	if _, _, err := service.runToolkitSet(context.Background(), request(30)); !errors.As(err, &retired) || retired.Nested || retired.ApplicationID != 30 {
		t.Fatalf("own retired: %v", err)
	}

	service = (&AdmissionService{}).WithNestedMemoryToolkits(nested).WithRetiredAgents(retiredSet{32: true})
	if _, _, err := service.runToolkitSet(context.Background(), request(30)); !errors.As(err, &retired) || !retired.Nested || retired.ApplicationID != 32 {
		t.Fatalf("grandchild retired: %v", err)
	}

	// Without the nested reader the direct children are still checked.
	service = (&AdmissionService{}).WithRetiredAgents(retiredSet{31: true})
	if _, _, err := service.runToolkitSet(context.Background(), request(30)); !errors.As(err, &retired) || !retired.Nested || retired.ApplicationID != 31 {
		t.Fatalf("child retired: %v", err)
	}

	service = (&AdmissionService{}).WithNestedMemoryToolkits(nested).WithRetiredAgents(retiredSet{})
	if _, _, err := service.runToolkitSet(context.Background(), request(30)); err != nil {
		t.Fatalf("no retired agent refused: %v", err)
	}
}

// A parked fan-out wake carries no application id; the frozen application
// names the agent, and a retired one is refused (and the admitter ends the
// wake typed instead of retrying it).
func TestAdmissionRefusesTheFanoutWakeOfARetiredAgent(t *testing.T) {
	t.Parallel()
	request := SubmitRequest{
		Identity:              executionapp.AdmissionIdentity{ResourceProjectID: "7"},
		CapabilityID:          executiondomain.AgentApplicationCapability,
		CurrentFanoutWakeTurn: &CurrentFanoutWakeTurn{},
		Input:                 &runtimev1.AgentExecutionInputV1{Application: []byte(`{"id":30,"version_details":{"tools":[]}}`)},
	}
	var retired *agentworkforce.RetiredError
	service := (&AdmissionService{}).WithRetiredAgents(retiredSet{30: true})
	_, _, err := service.runToolkitSet(context.Background(), request)
	if !errors.As(err, &retired) || retired.Nested || retired.ApplicationID != 30 {
		t.Fatalf("fan-out wake of a retired agent: %v", err)
	}
	if !admissionRefusal(err) {
		t.Fatal("the fan-out wake admitter would retry a retired agent instead of ending the wake")
	}
}
