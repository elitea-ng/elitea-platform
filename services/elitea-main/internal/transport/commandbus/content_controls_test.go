package commandbus

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/protobuf/proto"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
)

type contentControlsResolverStub struct {
	controls *runtimev1.WorkerContentControlsV1
	err      error
	calls    int
}

func (s *contentControlsResolverStub) WorkerContentControls(context.Context, *runtimev1.WorkerCommandV1) (*runtimev1.WorkerContentControlsV1, error) {
	s.calls++
	return s.controls, s.err
}

func preparedContentControlsCommand(t *testing.T, resolver ContentControlsResolver) (*runtimev1.WorkerCommandV1, error) {
	t.Helper()
	signer := &signerStub{}
	producer, err := NewAgentExecutionProducer(validAgentProducerConfig(), signer, &appenderStub{})
	if err != nil {
		t.Fatal(err)
	}
	if resolver != nil {
		producer.WithContentControls(resolver)
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

// ADR-0043 decisions 3 and 4 rollout gate: while no administrator saved the
// content controls the resolver answers nil and the signed command has no
// field 71, so a worker of an earlier release still accepts it. After the
// save the command carries the controls and nothing else changes. A
// resolver error signs nothing.
func TestProducerSignsTheContentControlsOnlyOnceSaved(t *testing.T) {
	legacy, err := preparedContentControlsCommand(t, nil)
	if err != nil {
		t.Fatal(err)
	}
	unsaved, err := preparedContentControlsCommand(t, &contentControlsResolverStub{})
	if err != nil {
		t.Fatal(err)
	}
	legacy.DeadlineUnixMillis, unsaved.DeadlineUnixMillis = 0, 0
	if !proto.Equal(legacy, unsaved) || unsaved.GetContentControls() != nil {
		t.Fatal("unsaved content controls changed the command")
	}
	resolver := &contentControlsResolverStub{controls: &runtimev1.WorkerContentControlsV1{
		DisclosureText: "Generated with Elitea AI", PlatformPreamble: "Rules.", PlatformPreambleRevision: 2, UntrustedReadThenAct: true,
	}}
	saved, err := preparedContentControlsCommand(t, resolver)
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(saved.GetContentControls(), resolver.controls) || resolver.calls != 1 {
		t.Fatalf("signed controls = %v, calls = %d", saved.GetContentControls(), resolver.calls)
	}
	saved.DeadlineUnixMillis, saved.ContentControls = 0, nil
	if !proto.Equal(legacy, saved) {
		t.Fatal("the saved controls changed more than field 71")
	}
	if _, err := preparedContentControlsCommand(t, &contentControlsResolverStub{err: errors.New("down")}); err == nil {
		t.Fatal("a command was signed without the controls it should carry")
	}
}

// Only agent execution commands carry content controls.
func TestTheContentControlsAreOnlyAskedForAgentCommands(t *testing.T) {
	signer := &signerStub{}
	producer, err := NewProducer(validProducerConfig(), signer, &appenderStub{})
	if err != nil {
		t.Fatal(err)
	}
	resolver := &contentControlsResolverStub{controls: &runtimev1.WorkerContentControlsV1{}}
	producer.WithContentControls(resolver)
	if _, err := producer.PrepareValidation(context.Background(), validTransportDispatch()); err != nil {
		t.Fatal(err)
	}
	if resolver.calls != 0 {
		t.Fatalf("calls = %d", resolver.calls)
	}
}
