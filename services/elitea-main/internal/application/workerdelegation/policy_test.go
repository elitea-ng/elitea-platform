package workerdelegation

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/protobuf/proto"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

type settingsStub struct {
	settings platformconfig.DelegationSettings
	err      error
}

func (s settingsStub) Get(context.Context) (platformconfig.DelegationSettings, error) {
	return s.settings, s.err
}

type inputStub map[string][]byte

func (s inputStub) InputEntries(_ context.Context, _ string, ids []string) (map[string][]byte, error) {
	out := map[string][]byte{}
	for _, id := range ids {
		if content, ok := s[id]; ok {
			out[id] = content
		}
	}
	return out, nil
}

type classesStub struct {
	classes map[[2]int64][]string
	reads   int
}

func (s *classesStub) AgentDataClasses(_ context.Context, projectID, applicationID int64) ([]string, error) {
	s.reads++
	return s.classes[[2]int64{projectID, applicationID}], nil
}

func agentInput(t *testing.T, application string) []byte {
	t.Helper()
	content, err := proto.Marshal(&runtimev1.AgentExecutionInputV1{Application: []byte(application)})
	if err != nil {
		t.Fatal(err)
	}
	return content
}

func agentCommand() *runtimev1.WorkerCommandV1 {
	return &runtimev1.WorkerCommandV1{
		ResourceProjectId: "17",
		InputBundleRef:    &runtimev1.ExecutionInputBundleReferenceV1{InputBundleId: "bundle-1"},
		CapabilityCommand: &runtimev1.WorkerCommandV1_AgentExecution{
			AgentExecution: &runtimev1.AgentExecutionCommandV1{RequestEntryId: "request"},
		},
	}
}

func resolver(t *testing.T, settings platformconfig.DelegationSettings, input []byte, classes map[[2]int64][]string) (*Resolver, *classesStub) {
	t.Helper()
	reader := &classesStub{classes: classes}
	resolved, err := NewResolver(settingsStub{settings: settings}, inputStub{"request": input}, reader)
	if err != nil {
		t.Fatal(err)
	}
	return resolved, reader
}

func TestDefaultsLeaveTheCommandUnchanged(t *testing.T) {
	t.Parallel()
	r, _ := resolver(t, platformconfig.DefaultDelegationSettings(), agentInput(t, `{"id":5}`), nil)
	policy, err := r.WorkerDelegationPolicy(context.Background(), agentCommand())
	if err != nil || policy != nil {
		t.Fatalf("policy = %v, err = %v; want nil, nil", policy, err)
	}
}

func TestTheSettingAndTheRootDataClassesTravelOnTheCommand(t *testing.T) {
	t.Parallel()
	r, _ := resolver(t, platformconfig.DelegationSettings{MaxDepth: 2, Mode: platformconfig.DelegationReport},
		agentInput(t, `{"id":5,"version_id":9}`), map[[2]int64][]string{{17, 5}: {"pii", "finance.restricted"}})
	policy, err := r.WorkerDelegationPolicy(context.Background(), agentCommand())
	if err != nil {
		t.Fatal(err)
	}
	if policy.GetMaxDepth() != 2 ||
		policy.GetMode() != runtimev1.WorkerDelegationModeV1_WORKER_DELEGATION_MODE_V1_REPORT ||
		len(policy.GetRootDataClasses()) != 2 || policy.GetRootDataClasses()[0] != "pii" {
		t.Fatalf("policy = %v", policy)
	}
	// The field is 67 on the command and survives a round trip.
	command := agentCommand()
	command.DelegationPolicy = policy
	encoded, err := proto.Marshal(command)
	if err != nil {
		t.Fatal(err)
	}
	decoded := &runtimev1.WorkerCommandV1{}
	if err := proto.Unmarshal(encoded, decoded); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(decoded.GetDelegationPolicy(), policy) {
		t.Fatalf("round trip = %v", decoded.GetDelegationPolicy())
	}
}

func TestRootDataClassesAloneMakeAPolicy(t *testing.T) {
	t.Parallel()
	r, _ := resolver(t, platformconfig.DefaultDelegationSettings(), agentInput(t, `{"id":5}`),
		map[[2]int64][]string{{17, 5}: {"pii"}})
	policy, err := r.WorkerDelegationPolicy(context.Background(), agentCommand())
	if err != nil {
		t.Fatal(err)
	}
	if policy.GetMaxDepth() != 4 ||
		policy.GetMode() != runtimev1.WorkerDelegationModeV1_WORKER_DELEGATION_MODE_V1_ENFORCE ||
		len(policy.GetRootDataClasses()) != 1 {
		t.Fatalf("policy = %v", policy)
	}
}

func TestAGovernedRunIsAlwaysEnforced(t *testing.T) {
	t.Parallel()
	r, _ := resolver(t, platformconfig.DelegationSettings{MaxDepth: 4, Mode: platformconfig.DelegationReport},
		agentInput(t, `{"instructions":"ad hoc"}`), nil)
	command := agentCommand()
	command.SecurityContextRef = &runtimev1.ExecutionSecurityContextReferenceV1{}
	policy, err := r.WorkerDelegationPolicy(context.Background(), command)
	if err != nil || policy != nil {
		t.Fatalf("policy = %v, err = %v; a governed run under report is the default (enforce)", policy, err)
	}
}

func TestAnAdhocConversationAndOtherCommandsReadNoRegistry(t *testing.T) {
	t.Parallel()
	r, classes := resolver(t, platformconfig.DefaultDelegationSettings(), agentInput(t, `{"instructions":"x"}`), nil)
	if policy, err := r.WorkerDelegationPolicy(context.Background(), agentCommand()); err != nil || policy != nil {
		t.Fatalf("ad hoc: policy = %v, err = %v", policy, err)
	}
	toolkit := &runtimev1.WorkerCommandV1{CapabilityCommand: &runtimev1.WorkerCommandV1_ToolkitCallTool{
		ToolkitCallTool: &runtimev1.ToolkitCallToolCommandV1{},
	}}
	if policy, err := r.WorkerDelegationPolicy(context.Background(), toolkit); err != nil || policy != nil {
		t.Fatalf("toolkit call: policy = %v, err = %v", policy, err)
	}
	if classes.reads != 0 {
		t.Fatalf("registry reads = %d", classes.reads)
	}
}

func TestAnUnreadableSettingIsRetried(t *testing.T) {
	t.Parallel()
	r, err := NewResolver(settingsStub{err: errors.New("down")}, inputStub{}, &classesStub{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.WorkerDelegationPolicy(context.Background(), agentCommand()); err == nil {
		t.Fatal("an unreadable setting must not sign a command without its policy")
	}
}

func TestAMalformedRegistryRowIsNeverNoClass(t *testing.T) {
	t.Parallel()
	r, _ := resolver(t, platformconfig.DefaultDelegationSettings(), agentInput(t, `{"id":5}`),
		map[[2]int64][]string{{17, 5}: {"Not A Class"}})
	if _, err := r.WorkerDelegationPolicy(context.Background(), agentCommand()); err == nil {
		t.Fatal("a malformed class must not lift the constraint")
	}
}

func TestPolicyClampsTheDepth(t *testing.T) {
	t.Parallel()
	policy := Policy(platformconfig.DelegationSettings{MaxDepth: 99, Mode: platformconfig.DelegationEnforce}, nil)
	if policy.GetMaxDepth() != 4 {
		t.Fatalf("policy = %v", policy)
	}
}
