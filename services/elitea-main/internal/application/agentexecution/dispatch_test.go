package agentexecution

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

type agentDispatchStoreStub struct {
	dispatch   AgentExecutionDispatch
	prepared   *executionapp.StoredPreparedEnvelope
	storeCalls int
	markCalls  int
}

func (s *agentDispatchStoreStub) LoadPendingAgentExecution(context.Context, string) (AgentExecutionDispatch, error) {
	return s.dispatch, nil
}

func (s *agentDispatchStoreStub) LoadPreparedAgentExecution(context.Context, string) (*executionapp.StoredPreparedEnvelope, error) {
	if s.prepared == nil {
		return nil, nil
	}
	copy := *s.prepared
	copy.Envelope = copy.Envelope.Clone()
	return &copy, nil
}

func (s *agentDispatchStoreStub) StorePreparedAgentExecution(_ context.Context, _ string, candidate executionapp.PreparedCommandEnvelope) (executionapp.StoredPreparedEnvelope, error) {
	s.storeCalls++
	if s.prepared == nil {
		s.prepared = &executionapp.StoredPreparedEnvelope{Envelope: candidate.Clone()}
	}
	copy := *s.prepared
	copy.Envelope = copy.Envelope.Clone()
	return copy, nil
}

func (s *agentDispatchStoreStub) MarkAgentExecutionPublished(_ context.Context, _ string, digest runtimedomain.Digest) error {
	s.markCalls++
	if s.prepared == nil || s.prepared.Envelope.Digest != digest {
		return executionapp.ErrInvalidPreparedEnvelope
	}
	s.prepared.Published = true
	return nil
}

type agentProducerStub struct {
	prepared     executionapp.PreparedCommandEnvelope
	prepareCalls int
	appendCalls  int
	appended     [][]byte
	appendErrors []error
}

func (p *agentProducerStub) PrepareAgentExecution(_ context.Context, _ AgentExecutionDispatch) (executionapp.PreparedCommandEnvelope, error) {
	p.prepareCalls++
	return p.prepared.Clone(), nil
}

func (p *agentProducerStub) AppendPrepared(_ context.Context, _ string, envelope executionapp.PreparedCommandEnvelope) error {
	p.appendCalls++
	p.appended = append(p.appended, bytes.Clone(envelope.Bytes))
	if len(p.appendErrors) == 0 {
		return nil
	}
	err := p.appendErrors[0]
	p.appendErrors = p.appendErrors[1:]
	return err
}

func TestAgentDispatcherRetriesTheExactDurableEnvelopeAfterBusFailure(t *testing.T) {
	dispatch := validAgentDispatch()
	store := &agentDispatchStoreStub{dispatch: dispatch}
	producer := &agentProducerStub{
		prepared: agentPreparedEnvelope("before-rotation"),
		appendErrors: []error{
			executionapp.ErrDispatchBackpressured,
		},
	}
	dispatcher, err := NewDispatcher(store, producer)
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatcher.Dispatch(context.Background(), dispatch.OutboxID); !errors.Is(err, executionapp.ErrDispatchBackpressured) {
		t.Fatalf("first dispatch error = %v", err)
	}
	if store.prepared == nil || store.prepared.Published || store.markCalls != 0 {
		t.Fatal("failed append lost or published the durable agent intent")
	}
	producer.prepared = agentPreparedEnvelope("after-rotation")
	if err := dispatcher.Dispatch(context.Background(), dispatch.OutboxID); err != nil {
		t.Fatal(err)
	}
	if producer.prepareCalls != 1 || producer.appendCalls != 2 ||
		!bytes.Equal(producer.appended[0], producer.appended[1]) ||
		!store.prepared.Published || store.storeCalls != 1 || store.markCalls != 1 {
		t.Fatal("agent dispatch retry changed or re-signed the durable envelope")
	}
}

func TestAgentDispatchAcceptsBothCurrentSemanticsAndRejectsUnknownSIOEvents(t *testing.T) {
	for _, capabilityID := range []string{
		executiondomain.AgentApplicationCapability,
		executiondomain.AgentAdhocCapability,
	} {
		dispatch := validAgentDispatch()
		dispatch.CapabilityID = capabilityID
		if err := dispatch.Validate(); err != nil {
			t.Fatalf("capability %q rejected: %v", capabilityID, err)
		}
	}
	dispatch := validAgentDispatch()
	dispatch.SIOEvent = "application_predict"
	if !errors.Is(dispatch.Validate(), ErrInvalidAgentExecutionDispatch) {
		t.Fatal("unadmitted browser event was accepted")
	}
}

type agentPendingOutboxStub struct {
	ids         []string
	retireCalls int
	listCalls   int
}

func (s *agentPendingOutboxStub) RetireNoAuthorityAgentExecution(context.Context, int) (int, error) {
	s.retireCalls++
	return 0, nil
}

func (s *agentPendingOutboxStub) ListPendingAgentExecutionIDs(context.Context, int, time.Duration) ([]string, error) {
	s.listCalls++
	return append([]string(nil), s.ids...), nil
}

func TestAgentOutboxPublisherUsesAgentScopedRecoveryLifecycle(t *testing.T) {
	dispatch := validAgentDispatch()
	store := &agentDispatchStoreStub{dispatch: dispatch}
	producer := &agentProducerStub{prepared: agentPreparedEnvelope("publisher")}
	dispatcher, err := NewDispatcher(store, producer)
	if err != nil {
		t.Fatal(err)
	}
	outbox := &agentPendingOutboxStub{ids: []string{dispatch.OutboxID}}
	publisher, err := NewOutboxPublisher(outbox, dispatcher, executionapp.OutboxPublisherConfig{
		PollInterval:      time.Second,
		VisibilityTimeout: time.Minute,
		BatchSize:         1,
		MaxConcurrent:     1,
		ReportFailure:     func(error) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := publisher.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if outbox.retireCalls != 1 || outbox.listCalls != 1 ||
		producer.appendCalls != 1 || !store.prepared.Published {
		t.Fatal("agent publisher escaped its capability-scoped lifecycle")
	}
}

func agentPreparedEnvelope(keyID string) executionapp.PreparedCommandEnvelope {
	encoded := []byte("signed-agent-envelope:" + keyID)
	return executionapp.PreparedCommandEnvelope{
		Bytes:            encoded,
		Digest:           runtimedomain.SHA256(encoded),
		SignatureProfile: 1,
		KeyID:            keyID,
	}
}

func validAgentDispatch() AgentExecutionDispatch {
	return AgentExecutionDispatch{
		OutboxID:              "agent-outbox-1",
		CommandID:             "agent-command-1",
		ExecutionID:           "agent-execution-1",
		Generation:            1,
		DispatchOrdinal:       1,
		TenantID:              "tenant-1",
		ResourceProjectID:     "2",
		ProjectionProjectID:   "2",
		PrincipalRef:          "actor-1",
		InputBundleID:         "agent-input-bundle-1",
		InputBundleVersion:    "admission:agent-input-bundle-1",
		InputBundleMediaType:  executiondomain.InputBundleManifestMediaType,
		InputBundleByteLength: 512,
		InputBundleDigest:     runtimedomain.SHA256([]byte("agent manifest")),
		CapabilityID:          executiondomain.AgentApplicationCapability,
		CapabilityVersion:     "1.0.0",
		ResourceClass:         "agent",
		IsolationClass:        "project",
		Priority:              1,
		Deadline:              time.Now().UTC().Add(time.Minute),
		LimitsRevision:        "limits-v1",
		RequestEntryID:        "agent-request",
		ClientStreamID:        "conversation-1",
		ClientMessageID:       "response-1",
		SIOEvent:              "chat_predict",
	}
}
