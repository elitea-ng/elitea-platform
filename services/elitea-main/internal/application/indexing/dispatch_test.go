package indexing

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

type indexDispatchStoreStub struct {
	dispatch     IndexIngestDispatch
	prepared     *executionapp.StoredPreparedEnvelope
	prepareLoads int
	storeCalls   int
	markCalls    int
	markErrors   []error
}

func (s *indexDispatchStoreStub) LoadPendingIndexIngest(context.Context, string) (IndexIngestDispatch, error) {
	return s.dispatch, nil
}

func (s *indexDispatchStoreStub) LoadPreparedIndexIngest(context.Context, string) (*executionapp.StoredPreparedEnvelope, error) {
	s.prepareLoads++
	if s.prepared == nil {
		return nil, nil
	}
	copy := *s.prepared
	copy.Envelope = copy.Envelope.Clone()
	return &copy, nil
}

func (s *indexDispatchStoreStub) StorePreparedIndexIngest(_ context.Context, _ string, candidate executionapp.PreparedCommandEnvelope) (executionapp.StoredPreparedEnvelope, error) {
	s.storeCalls++
	if s.prepared == nil {
		s.prepared = &executionapp.StoredPreparedEnvelope{Envelope: candidate.Clone()}
	}
	copy := *s.prepared
	copy.Envelope = copy.Envelope.Clone()
	return copy, nil
}

func (s *indexDispatchStoreStub) MarkIndexIngestPublished(_ context.Context, _ string, digest runtimedomain.Digest) error {
	s.markCalls++
	if s.prepared == nil || s.prepared.Envelope.Digest != digest {
		return executionapp.ErrInvalidPreparedEnvelope
	}
	if len(s.markErrors) != 0 {
		err := s.markErrors[0]
		s.markErrors = s.markErrors[1:]
		return err
	}
	s.prepared.Published = true
	return nil
}

type indexProducerStub struct {
	prepared     executionapp.PreparedCommandEnvelope
	prepareCalls int
	dispatches   []IndexIngestDispatch
	appendCalls  int
	appended     [][]byte
	appendErrors []error
}

func (p *indexProducerStub) PrepareIndexIngest(_ context.Context, dispatch IndexIngestDispatch) (executionapp.PreparedCommandEnvelope, error) {
	p.prepareCalls++
	p.dispatches = append(p.dispatches, dispatch)
	return p.prepared.Clone(), nil
}

func (p *indexProducerStub) AppendPrepared(_ context.Context, _ string, envelope executionapp.PreparedCommandEnvelope) error {
	p.appendCalls++
	p.appended = append(p.appended, bytes.Clone(envelope.Bytes))
	if len(p.appendErrors) == 0 {
		return nil
	}
	err := p.appendErrors[0]
	p.appendErrors = p.appendErrors[1:]
	return err
}

func TestIndexIngestDispatcherRetainsAndRetriesExactEnvelopeAfterTransportFailure(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
	}{
		{name: "command-bus outage", err: errors.New("command bus unavailable")},
		{name: "capacity", err: executionapp.ErrDispatchBackpressured},
	} {
		t.Run(test.name, func(t *testing.T) {
			dispatch := validIndexIngestDispatch()
			dispatch.Initiator = executiondomain.IndexIngestInitiatorSchedule
			store := &indexDispatchStoreStub{dispatch: dispatch}
			producer := &indexProducerStub{
				prepared:     indexPreparedEnvelope("before-rotation"),
				appendErrors: []error{test.err},
			}
			dispatcher, err := NewIndexIngestDispatcher(store, producer)
			if err != nil {
				t.Fatal(err)
			}
			if err := dispatcher.Dispatch(context.Background(), dispatch.OutboxID); !errors.Is(err, test.err) {
				t.Fatalf("first dispatch error = %v", err)
			}
			if store.prepared == nil || store.prepared.Published || store.markCalls != 0 {
				t.Fatalf("failed append lost or published durable intent: prepared=%v published=%v marks=%d", store.prepared != nil, store.prepared != nil && store.prepared.Published, store.markCalls)
			}

			producer.prepared = indexPreparedEnvelope("after-rotation")
			if err := dispatcher.Dispatch(context.Background(), dispatch.OutboxID); err != nil {
				t.Fatal(err)
			}
			if producer.prepareCalls != 1 || len(producer.dispatches) != 1 ||
				producer.dispatches[0].Initiator != executiondomain.IndexIngestInitiatorSchedule ||
				producer.appendCalls != 2 || !bytes.Equal(producer.appended[0], producer.appended[1]) || !store.prepared.Published {
				t.Fatalf("retry changed durable bytes: prepares=%d appends=%d published=%v", producer.prepareCalls, producer.appendCalls, store.prepared.Published)
			}
		})
	}
}

func TestIndexIngestDispatcherRetriesExactEnvelopeAfterUnknownMarkOutcome(t *testing.T) {
	dispatch := validIndexIngestDispatch()
	markFailure := errors.New("PostgreSQL response lost after the command-bus publish")
	store := &indexDispatchStoreStub{dispatch: dispatch, markErrors: []error{markFailure}}
	producer := &indexProducerStub{prepared: indexPreparedEnvelope("before-mark-retry")}
	dispatcher, err := NewIndexIngestDispatcher(store, producer)
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatcher.Dispatch(context.Background(), dispatch.OutboxID); !errors.Is(err, markFailure) {
		t.Fatalf("unknown mark outcome = %v", err)
	}
	if store.prepared == nil || store.prepared.Published || producer.appendCalls != 1 || store.markCalls != 1 {
		t.Fatal("unknown mark outcome did not retain reconcilable exact intent")
	}
	producer.prepared = indexPreparedEnvelope("after-mark-retry")
	if err := dispatcher.Dispatch(context.Background(), dispatch.OutboxID); err != nil {
		t.Fatal(err)
	}
	if producer.prepareCalls != 1 || producer.appendCalls != 2 || store.markCalls != 2 || !store.prepared.Published || !bytes.Equal(producer.appended[0], producer.appended[1]) {
		t.Fatal("mark reconciliation changed or re-signed the durable envelope")
	}
}

type indexPendingOutboxStub struct {
	ids         []string
	retireCalls int
	listCalls   int
}

func (s *indexPendingOutboxStub) RetireNoAuthorityIndexIngest(context.Context, int) (int, error) {
	s.retireCalls++
	return 0, nil
}

func (s *indexPendingOutboxStub) ListPendingIndexIngestIDs(context.Context, int, time.Duration) ([]string, error) {
	s.listCalls++
	return append([]string(nil), s.ids...), nil
}

func TestIndexIngestOutboxPublisherUsesIndexScopedLifecycle(t *testing.T) {
	dispatch := validIndexIngestDispatch()
	store := &indexDispatchStoreStub{dispatch: dispatch}
	producer := &indexProducerStub{prepared: indexPreparedEnvelope("publisher")}
	dispatcher, err := NewIndexIngestDispatcher(store, producer)
	if err != nil {
		t.Fatal(err)
	}
	outbox := &indexPendingOutboxStub{ids: []string{dispatch.OutboxID}}
	publisher, err := NewIndexIngestOutboxPublisher(outbox, dispatcher, executionapp.OutboxPublisherConfig{
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
	if outbox.retireCalls != 1 || outbox.listCalls != 1 || producer.appendCalls != 1 || !store.prepared.Published {
		t.Fatalf("index publisher escaped scoped lifecycle: retire=%d list=%d append=%d published=%v", outbox.retireCalls, outbox.listCalls, producer.appendCalls, store.prepared.Published)
	}
}

func TestIndexIngestDispatchRejectsDuplicateEntryReferences(t *testing.T) {
	dispatch := validIndexIngestDispatch()
	dispatch.LLMModelEntryID = dispatch.ToolParametersEntryID
	if !errors.Is(dispatch.Validate(), ErrInvalidIndexIngestDispatch) {
		t.Fatal("duplicate immutable entry reference was accepted")
	}
}

func TestIndexIngestDispatchRejectsUnknownInitiator(t *testing.T) {
	dispatch := validIndexIngestDispatch()
	dispatch.Initiator = executiondomain.IndexIngestInitiator("automation")
	if !errors.Is(dispatch.Validate(), ErrInvalidIndexIngestDispatch) {
		t.Fatal("unknown durable initiator was accepted")
	}
}

func TestIndexIngestDispatchAcceptsClosedInitiatorSet(t *testing.T) {
	for _, initiator := range []executiondomain.IndexIngestInitiator{
		executiondomain.IndexIngestInitiatorUser,
		executiondomain.IndexIngestInitiatorLLM,
		executiondomain.IndexIngestInitiatorSchedule,
	} {
		t.Run(string(initiator), func(t *testing.T) {
			dispatch := validIndexIngestDispatch()
			dispatch.Initiator = initiator
			if err := dispatch.Validate(); err != nil {
				t.Fatalf("valid initiator %q rejected: %v", initiator, err)
			}
		})
	}
}

func indexPreparedEnvelope(keyID string) executionapp.PreparedCommandEnvelope {
	encoded := []byte("signed-index-envelope:" + keyID)
	return executionapp.PreparedCommandEnvelope{
		Bytes:            encoded,
		Digest:           runtimedomain.SHA256(encoded),
		SignatureProfile: 1,
		KeyID:            keyID,
	}
}

func validIndexIngestDispatch() IndexIngestDispatch {
	return IndexIngestDispatch{
		OutboxID:                    "index-outbox-1",
		CommandID:                   "index-command-1",
		ExecutionID:                 "index-execution-1",
		Generation:                  1,
		DispatchOrdinal:             1,
		TenantID:                    "tenant-1",
		ResourceProjectID:           "1",
		ProjectionProjectID:         "1",
		PrincipalRef:                "actor-1",
		InputBundleID:               "index-bundle-1",
		InputBundleVersion:          "admission:index-bundle-1",
		InputBundleMediaType:        "application/x-protobuf",
		InputBundleByteLength:       256,
		InputBundleDigest:           runtimedomain.SHA256([]byte("index-manifest")),
		CapabilityVersion:           "1",
		ResourceClass:               "indexing",
		IsolationClass:              "project",
		Priority:                    1,
		Deadline:                    time.Date(2026, time.July, 22, 12, 0, 0, 0, time.UTC),
		LimitsRevision:              "index-limits-v1",
		ToolkitConfigurationEntryID: "toolkit-configuration",
		ToolParametersEntryID:       "tool-parameters",
		LLMModelEntryID:             "llm-model",
		LLMConfigurationEntryID:     "llm-configuration",
		MCPTokensEntryID:            "mcp-credential-references",
		Initiator:                   executiondomain.IndexIngestInitiatorUser,
	}
}
