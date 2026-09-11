package toolkitexecution

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
)

type toolkitReadDispatchStoreStub struct {
	dispatch   ToolkitExecuteReadDispatch
	prepared   *executionapp.StoredPreparedEnvelope
	storeCalls int
	markCalls  int
}

func (s *toolkitReadDispatchStoreStub) LoadPendingToolkitExecuteRead(context.Context, string) (ToolkitExecuteReadDispatch, error) {
	return s.dispatch, nil
}

func (s *toolkitReadDispatchStoreStub) LoadPreparedToolkitExecuteRead(context.Context, string) (*executionapp.StoredPreparedEnvelope, error) {
	if s.prepared == nil {
		return nil, nil
	}
	copy := *s.prepared
	copy.Envelope = copy.Envelope.Clone()
	return &copy, nil
}

func (s *toolkitReadDispatchStoreStub) StorePreparedToolkitExecuteRead(
	_ context.Context,
	_ string,
	candidate executionapp.PreparedCommandEnvelope,
) (executionapp.StoredPreparedEnvelope, error) {
	s.storeCalls++
	if s.prepared == nil {
		s.prepared = &executionapp.StoredPreparedEnvelope{Envelope: candidate.Clone()}
	}
	copy := *s.prepared
	copy.Envelope = copy.Envelope.Clone()
	return copy, nil
}

func (s *toolkitReadDispatchStoreStub) MarkToolkitExecuteReadPublished(
	_ context.Context,
	_ string,
	digest runtimedomain.Digest,
) error {
	s.markCalls++
	if s.prepared == nil || s.prepared.Envelope.Digest != digest {
		return executionapp.ErrInvalidPreparedEnvelope
	}
	s.prepared.Published = true
	return nil
}

type toolkitReadProducerStub struct {
	prepared     executionapp.PreparedCommandEnvelope
	prepareCalls int
	appendCalls  int
	appended     [][]byte
	appendErrors []error
}

func (p *toolkitReadProducerStub) PrepareToolkitExecuteRead(
	context.Context,
	ToolkitExecuteReadDispatch,
) (executionapp.PreparedCommandEnvelope, error) {
	p.prepareCalls++
	return p.prepared.Clone(), nil
}

func (p *toolkitReadProducerStub) AppendPrepared(
	_ context.Context,
	_ string,
	envelope executionapp.PreparedCommandEnvelope,
) error {
	p.appendCalls++
	p.appended = append(p.appended, bytes.Clone(envelope.Bytes))
	if len(p.appendErrors) == 0 {
		return nil
	}
	err := p.appendErrors[0]
	p.appendErrors = p.appendErrors[1:]
	return err
}

func TestToolkitReadDispatcherRetriesExactDurableEnvelopeAfterRedisFailure(t *testing.T) {
	dispatch := validToolkitReadDispatch()
	store := &toolkitReadDispatchStoreStub{dispatch: dispatch}
	producer := &toolkitReadProducerStub{
		prepared:     toolkitReadPreparedEnvelope("before-rotation"),
		appendErrors: []error{executionapp.ErrDispatchBackpressured},
	}
	dispatcher, err := NewDispatcher(store, producer)
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatcher.Dispatch(context.Background(), dispatch.OutboxID); !errors.Is(err, executionapp.ErrDispatchBackpressured) {
		t.Fatalf("first dispatch error = %v", err)
	}
	producer.prepared = toolkitReadPreparedEnvelope("after-rotation")
	if err := dispatcher.Dispatch(context.Background(), dispatch.OutboxID); err != nil {
		t.Fatal(err)
	}
	if producer.prepareCalls != 1 || producer.appendCalls != 2 ||
		!bytes.Equal(producer.appended[0], producer.appended[1]) ||
		store.storeCalls != 1 || store.markCalls != 1 || !store.prepared.Published {
		t.Fatal("direct toolkit dispatch retry changed or re-signed the durable envelope")
	}
}

func TestToolkitReadDispatchRejectsOtherCapabilitiesAndUnsafeIdentity(t *testing.T) {
	dispatch := validToolkitReadDispatch()
	if err := dispatch.Validate(); err != nil {
		t.Fatal(err)
	}
	dispatch.CapabilityID = executiondomain.AgentAdhocCapability
	if !errors.Is(dispatch.Validate(), ErrInvalidToolkitExecuteReadDispatch) {
		t.Fatal("agent capability was accepted")
	}
	dispatch = validToolkitReadDispatch()
	dispatch.RequestEntryID = "request\nforged"
	if !errors.Is(dispatch.Validate(), ErrInvalidToolkitExecuteReadDispatch) {
		t.Fatal("unsafe request entry identity was accepted")
	}
}

func toolkitReadPreparedEnvelope(keyID string) executionapp.PreparedCommandEnvelope {
	encoded := []byte("signed-toolkit-read-envelope:" + keyID)
	return executionapp.PreparedCommandEnvelope{
		Bytes: encoded, Digest: runtimedomain.SHA256(encoded), SignatureProfile: 1, KeyID: keyID,
	}
}

func validToolkitReadDispatch() ToolkitExecuteReadDispatch {
	return ToolkitExecuteReadDispatch{
		OutboxID: "outbox-1", CommandID: "command-1", ExecutionID: "execution-1",
		Generation: 1, DispatchOrdinal: 1, TenantID: "tenant-1",
		ResourceProjectID: "2", ProjectionProjectID: "2", PrincipalRef: "actor-1",
		InputBundleID: "bundle-1", InputBundleVersion: "admission:bundle-1",
		InputBundleMediaType: executiondomain.InputBundleManifestMediaType, InputBundleByteLength: 512,
		InputBundleDigest: runtimedomain.SHA256([]byte("manifest")),
		CapabilityID:      executiondomain.ToolkitExecuteReadCapability, CapabilityVersion: "1",
		ResourceClass: "toolkit-read", IsolationClass: "project", Priority: 1,
		Deadline: time.Now().UTC().Add(time.Minute), LimitsRevision: "limits-v1",
		RequestEntryID: toolkitReadRequestEntryID,
	}
}
