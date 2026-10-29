package commandbus

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"google.golang.org/protobuf/proto"

	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
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

type refusalStub struct{ message string }

func (r refusalStub) Error() string                     { return "refused: " + r.message }
func (r refusalStub) DispatchRefusal() (string, string) { return "EGRESS_POLICY_INVALID", r.message }

type refusalRecorderStub struct {
	calls []string
	err   error
}

func (r *refusalRecorderStub) RefuseDispatch(_ context.Context, executionID string, generation uint64, code, message string) error {
	r.calls = append(r.calls, fmt.Sprintf("%s/%d/%s/%s", executionID, generation, code, message))
	return r.err
}

func prepareWithRefusals(t *testing.T, resolver EgressPolicyResolver, recorder DispatchRefusalRecorder) (*signerStub, error) {
	t.Helper()
	signer := &signerStub{}
	producer, err := NewProducer(validProducerConfig(), signer, &appenderStub{})
	if err != nil {
		t.Fatal(err)
	}
	producer.WithEgressPolicy(resolver).WithDispatchRefusals(recorder)
	_, err = producer.PrepareValidation(context.Background(), validTransportDispatch())
	return signer, err
}

// A policy that can never be assembled is recorded on the run and answered
// as an executionapp.DispatchRefusal: nothing is signed, and the dispatcher
// stops retrying. A transient error is recorded nowhere and retried.
func TestProducerRecordsARefusedPolicy(t *testing.T) {
	dispatch := validTransportDispatch()
	recorder := &refusalRecorderStub{}
	signer, err := prepareWithRefusals(t, &egressResolverStub{err: fmt.Errorf("wrapped: %w", refusalStub{message: "fix it"})}, recorder)
	refusal, refused := executionapp.AsDispatchRefusal(err)
	if !refused || refusal.Code != "EGRESS_POLICY_INVALID" || refusal.Message != "fix it" || !errors.Is(err, executionapp.ErrDispatchRefused) {
		t.Fatalf("err = %v", err)
	}
	want := fmt.Sprintf("%s/%d/EGRESS_POLICY_INVALID/fix it", dispatch.ExecutionID, dispatch.Generation)
	if !slices.Equal(recorder.calls, []string{want}) || signer.exact != nil {
		t.Fatalf("calls=%v signed=%v", recorder.calls, signer.exact != nil)
	}

	// The refusal could not be recorded: an ordinary error, retried.
	failing := &refusalRecorderStub{err: errors.New("database down")}
	_, err = prepareWithRefusals(t, &egressResolverStub{err: refusalStub{message: "fix it"}}, failing)
	if err == nil || errors.Is(err, executionapp.ErrDispatchRefused) {
		t.Fatalf("an unrecorded refusal: %v", err)
	}

	transient := &refusalRecorderStub{}
	_, err = prepareWithRefusals(t, &egressResolverStub{err: errors.New("database down")}, transient)
	if err == nil || errors.Is(err, executionapp.ErrDispatchRefused) || len(transient.calls) != 0 {
		t.Fatalf("a transient error: %v calls=%v", err, transient.calls)
	}
}

// A refused policy refuses the command: nothing is signed without it.
func TestProducerRefusesACommandWhosePolicyIsRefused(t *testing.T) {
	refused := errors.New("refused")
	if _, _, err := preparedWorkerCommand(t, &egressResolverStub{err: refused}); !errors.Is(err, refused) {
		t.Fatalf("err = %v", err)
	}
}
