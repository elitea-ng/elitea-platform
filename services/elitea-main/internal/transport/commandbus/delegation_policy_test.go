package commandbus

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"google.golang.org/protobuf/proto"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
)

type delegationResolverStub struct {
	policy *runtimev1.WorkerDelegationPolicyV1
	err    error
	calls  int
}

func (s *delegationResolverStub) WorkerDelegationPolicy(context.Context, *runtimev1.WorkerCommandV1) (*runtimev1.WorkerDelegationPolicyV1, error) {
	s.calls++
	return s.policy, s.err
}

func preparedAgentCommand(t *testing.T, resolver DelegationPolicyResolver) (*runtimev1.WorkerCommandV1, []byte, error) {
	t.Helper()
	signer := &signerStub{}
	producer, err := NewAgentExecutionProducer(validAgentProducerConfig(), signer, &appenderStub{})
	if err != nil {
		t.Fatal(err)
	}
	if resolver != nil {
		producer.WithDelegationPolicy(resolver)
	}
	if _, err := producer.PrepareAgentExecution(context.Background(), validAgentTransportDispatch()); err != nil {
		return nil, nil, err
	}
	command := &runtimev1.WorkerCommandV1{}
	if err := proto.Unmarshal(signer.exact, command); err != nil {
		t.Fatal(err)
	}
	return command, signer.exact, nil
}

// ADR-0044 decision 3: the signed agent command carries the delegation
// policy (field 67); the defaults (nil) leave the bytes exactly as a producer
// without a resolver signs them, and a resolver error signs nothing.
func TestProducerSignsTheDelegationPolicy(t *testing.T) {
	_, legacy, err := preparedAgentCommand(t, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, defaults, err := preparedAgentCommand(t, &delegationResolverStub{})
	if err != nil || !bytes.Equal(legacy, defaults) {
		t.Fatalf("the default policy changed the command bytes: %v", err)
	}
	resolver := &delegationResolverStub{policy: &runtimev1.WorkerDelegationPolicyV1{
		MaxDepth:        2,
		Mode:            runtimev1.WorkerDelegationModeV1_WORKER_DELEGATION_MODE_V1_REPORT,
		RootDataClasses: []string{"pii"},
	}}
	command, _, err := preparedAgentCommand(t, resolver)
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(command.GetDelegationPolicy(), resolver.policy) || resolver.calls != 1 {
		t.Fatalf("signed policy = %v, calls = %d", command.GetDelegationPolicy(), resolver.calls)
	}
	if _, _, err := preparedAgentCommand(t, &delegationResolverStub{err: errors.New("down")}); err == nil {
		t.Fatal("a command was signed without the policy it should carry")
	}
}

// Only agent execution commands carry a delegation policy.
func TestTheDelegationPolicyIsOnlyAskedForAgentCommands(t *testing.T) {
	signer := &signerStub{}
	producer, err := NewProducer(validProducerConfig(), signer, &appenderStub{})
	if err != nil {
		t.Fatal(err)
	}
	resolver := &delegationResolverStub{policy: &runtimev1.WorkerDelegationPolicyV1{MaxDepth: 2}}
	producer.WithDelegationPolicy(resolver)
	if _, err := producer.PrepareValidation(context.Background(), validTransportDispatch()); err != nil {
		t.Fatal(err)
	}
	if resolver.calls != 0 {
		t.Fatalf("calls = %d", resolver.calls)
	}
}
