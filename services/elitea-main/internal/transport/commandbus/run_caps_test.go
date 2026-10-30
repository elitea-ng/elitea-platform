package commandbus

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/protobuf/proto"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
)

type runCapsResolverStub struct {
	caps  *runtimev1.WorkerRunCapsV1
	err   error
	calls int
}

func (s *runCapsResolverStub) WorkerRunCaps(context.Context, *runtimev1.WorkerCommandV1) (*runtimev1.WorkerRunCapsV1, error) {
	s.calls++
	return s.caps, s.err
}

func preparedRunCapsCommand(t *testing.T, resolver RunCapsResolver) (*runtimev1.WorkerCommandV1, error) {
	t.Helper()
	signer := &signerStub{}
	producer, err := NewAgentExecutionProducer(validAgentProducerConfig(), signer, &appenderStub{})
	if err != nil {
		t.Fatal(err)
	}
	if resolver != nil {
		producer.WithRunCaps(resolver)
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

// ADR-0041 decision 3 rollout gate: while no administrator saved a per-tier
// run cap the resolver answers nil and the signed command has no field 69,
// so a worker of an earlier release still accepts it. After the save the
// command carries the caps and nothing else changes. A resolver error signs
// nothing.
func TestProducerSignsTheRunCapsOnlyOnceSaved(t *testing.T) {
	legacy, err := preparedRunCapsCommand(t, nil)
	if err != nil {
		t.Fatal(err)
	}
	unsaved, err := preparedRunCapsCommand(t, &runCapsResolverStub{})
	if err != nil {
		t.Fatal(err)
	}
	legacy.DeadlineUnixMillis, unsaved.DeadlineUnixMillis = 0, 0
	if !proto.Equal(legacy, unsaved) || unsaved.GetRunCaps() != nil {
		t.Fatal("unsaved run caps changed the command")
	}
	resolver := &runCapsResolverStub{caps: &runtimev1.WorkerRunCapsV1{MaxToolCalls: 50, MaxModelTurns: 20, MaxRecordsRead: 1000}}
	saved, err := preparedRunCapsCommand(t, resolver)
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(saved.GetRunCaps(), resolver.caps) || resolver.calls != 1 {
		t.Fatalf("signed caps = %v, calls = %d", saved.GetRunCaps(), resolver.calls)
	}
	saved.DeadlineUnixMillis, saved.RunCaps = 0, nil
	if !proto.Equal(legacy, saved) {
		t.Fatal("the saved caps changed more than field 69")
	}
	if _, err := preparedRunCapsCommand(t, &runCapsResolverStub{err: errors.New("down")}); err == nil {
		t.Fatal("a command was signed without the caps it should carry")
	}
}

// Only agent execution commands carry run caps.
func TestTheRunCapsAreOnlyAskedForAgentCommands(t *testing.T) {
	signer := &signerStub{}
	producer, err := NewProducer(validProducerConfig(), signer, &appenderStub{})
	if err != nil {
		t.Fatal(err)
	}
	resolver := &runCapsResolverStub{caps: &runtimev1.WorkerRunCapsV1{MaxToolCalls: 1}}
	producer.WithRunCaps(resolver)
	if _, err := producer.PrepareValidation(context.Background(), validTransportDispatch()); err != nil {
		t.Fatal(err)
	}
	if resolver.calls != 0 {
		t.Fatalf("calls = %d", resolver.calls)
	}
}
