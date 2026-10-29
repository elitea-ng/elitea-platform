package commandbus

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"google.golang.org/protobuf/proto"
)

type egressResolverStub struct {
	policy *runtimev1.WorkerEgressPolicyV1
	err    error
	seen   *runtimev1.WorkerCommandV1
}

func (s *egressResolverStub) WorkerEgressPolicy(_ context.Context, command *runtimev1.WorkerCommandV1) (*runtimev1.WorkerEgressPolicyV1, error) {
	s.seen = command
	return s.policy, s.err
}

func preparedWorkerCommand(t *testing.T, resolver EgressPolicyResolver) (*runtimev1.WorkerCommandV1, []byte, error) {
	t.Helper()
	signer := &signerStub{}
	producer, err := NewProducer(validProducerConfig(), signer, &appenderStub{})
	if err != nil {
		t.Fatal(err)
	}
	if resolver != nil {
		producer.WithEgressPolicy(resolver)
	}
	if _, err := producer.PrepareValidation(context.Background(), validTransportDispatch()); err != nil {
		return nil, nil, err
	}
	command := &runtimev1.WorkerCommandV1{}
	if err := proto.Unmarshal(signer.exact, command); err != nil {
		t.Fatal(err)
	}
	return command, signer.exact, nil
}

// The signed command carries the resolver's policy; an open answer (nil)
// leaves the bytes exactly as a producer without a resolver signs them.
func TestProducerSignsTheEgressPolicy(t *testing.T) {
	_, legacy, err := preparedWorkerCommand(t, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, open, err := preparedWorkerCommand(t, &egressResolverStub{})
	if err != nil || !bytes.Equal(legacy, open) {
		t.Fatalf("an open policy changed the command bytes: %v", err)
	}
	resolver := &egressResolverStub{policy: &runtimev1.WorkerEgressPolicyV1{
		Mode:  runtimev1.WorkerEgressModeV1_WORKER_EGRESS_MODE_V1_ALLOWLIST,
		Allow: []string{"api.github.com", "example.com"},
	}}
	command, _, err := preparedWorkerCommand(t, resolver)
	if err != nil {
		t.Fatal(err)
	}
	if resolver.seen.GetConfigurationValidation() == nil {
		t.Fatal("the resolver did not see the command")
	}
	if command.GetEgressPolicy().GetMode() != runtimev1.WorkerEgressModeV1_WORKER_EGRESS_MODE_V1_ALLOWLIST ||
		!slices.Equal(command.GetEgressPolicy().GetAllow(), []string{"api.github.com", "example.com"}) {
		t.Fatalf("signed policy = %v", command.GetEgressPolicy())
	}
}

// A refused policy refuses the command: nothing is signed without it.
func TestProducerRefusesACommandWhosePolicyIsRefused(t *testing.T) {
	refused := errors.New("refused")
	if _, _, err := preparedWorkerCommand(t, &egressResolverStub{err: refused}); !errors.Is(err, refused) {
		t.Fatalf("err = %v", err)
	}
}
