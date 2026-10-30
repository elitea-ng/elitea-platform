package commandbus

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/protobuf/proto"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
)

type approvalResolverStub struct {
	policy *runtimev1.WorkerApprovalPolicyV1
	err    error
	calls  int
}

func (s *approvalResolverStub) WorkerApprovalPolicy(context.Context, *runtimev1.WorkerCommandV1) (*runtimev1.WorkerApprovalPolicyV1, error) {
	s.calls++
	return s.policy, s.err
}

func preparedApprovalCommand(t *testing.T, resolver ApprovalPolicyResolver) (*runtimev1.WorkerCommandV1, error) {
	t.Helper()
	signer := &signerStub{}
	producer, err := NewAgentExecutionProducer(validAgentProducerConfig(), signer, &appenderStub{})
	if err != nil {
		t.Fatal(err)
	}
	if resolver != nil {
		producer.WithApprovalPolicy(resolver)
	}
	if _, err := producer.PrepareAgentExecution(context.Background(), validAgentTransportDispatch()); err != nil {
		return nil, err
	}
	command := &runtimev1.WorkerCommandV1{}
	if err := proto.Unmarshal(signer.exact, command); err != nil {
		t.Fatal(err)
	}
	return command, nil
}

// ADR-0041 decision 5 rollout gate: while no administrator saved the
// approval_policy section the resolver answers nil, and the signed command is
// the command a producer without a resolver signs (no field 68), so a worker
// from an earlier release still accepts it. After the save the command
// carries the policy. A resolver error signs nothing.
//
// The commands are compared decoded, never as bytes: map entries encode in a
// random order and the deadline comes from the wall clock. The deadline is
// cleared on both.
func TestProducerSignsTheApprovalPolicyOnlyOnceSaved(t *testing.T) {
	legacy, err := preparedApprovalCommand(t, nil)
	if err != nil {
		t.Fatal(err)
	}
	unsaved, err := preparedApprovalCommand(t, &approvalResolverStub{})
	if err != nil {
		t.Fatal(err)
	}
	legacy.DeadlineUnixMillis, unsaved.DeadlineUnixMillis = 0, 0
	if !proto.Equal(legacy, unsaved) {
		t.Fatal("an unsaved approval policy changed the command")
	}
	if unsaved.GetApprovalPolicy() != nil || len(unsaved.ProtoReflect().GetUnknown()) != 0 {
		t.Fatal("an unsaved approval policy put field 68 on the command")
	}
	resolver := &approvalResolverStub{policy: &runtimev1.WorkerApprovalPolicyV1{
		RiskTier:           "high",
		ApprovalCategories: []string{"delete", "external_send", "payment"},
	}}
	saved, err := preparedApprovalCommand(t, resolver)
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(saved.GetApprovalPolicy(), resolver.policy) || resolver.calls != 1 {
		t.Fatalf("signed policy = %v, calls = %d", saved.GetApprovalPolicy(), resolver.calls)
	}
	saved.DeadlineUnixMillis, saved.ApprovalPolicy = 0, nil
	if !proto.Equal(legacy, saved) {
		t.Fatal("the saved policy changed more than field 68")
	}
	if _, err := preparedApprovalCommand(t, &approvalResolverStub{err: errors.New("down")}); err == nil {
		t.Fatal("a command was signed without the policy it should carry")
	}
}

// Only agent execution commands carry an approval policy.
func TestTheApprovalPolicyIsOnlyAskedForAgentCommands(t *testing.T) {
	signer := &signerStub{}
	producer, err := NewProducer(validProducerConfig(), signer, &appenderStub{})
	if err != nil {
		t.Fatal(err)
	}
	resolver := &approvalResolverStub{policy: &runtimev1.WorkerApprovalPolicyV1{RiskTier: "high"}}
	producer.WithApprovalPolicy(resolver)
	if _, err := producer.PrepareValidation(context.Background(), validTransportDispatch()); err != nil {
		t.Fatal(err)
	}
	if resolver.calls != 0 {
		t.Fatalf("calls = %d", resolver.calls)
	}
}
