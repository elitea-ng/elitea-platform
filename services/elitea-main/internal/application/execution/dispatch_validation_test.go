package execution

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	configurationdomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/configurations"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
)

type dispatchStoreStub struct {
	dispatch      ValidationDispatch
	prepared      *StoredPreparedEnvelope
	loadPrepared  int
	storePrepared int
	markCalls     int
	markErr       error
}

func (s *dispatchStoreStub) LoadPendingValidation(_ context.Context, _ string) (ValidationDispatch, error) {
	return s.dispatch, nil
}

func (s *dispatchStoreStub) LoadPreparedValidation(_ context.Context, _ string) (*StoredPreparedEnvelope, error) {
	s.loadPrepared++
	if s.prepared == nil {
		return nil, nil
	}
	copy := *s.prepared
	copy.Envelope = copy.Envelope.Clone()
	return &copy, nil
}

func (s *dispatchStoreStub) StorePreparedValidation(_ context.Context, _ string, candidate PreparedCommandEnvelope) (StoredPreparedEnvelope, error) {
	s.storePrepared++
	if s.prepared == nil {
		s.prepared = &StoredPreparedEnvelope{Envelope: candidate.Clone()}
	}
	copy := *s.prepared
	copy.Envelope = copy.Envelope.Clone()
	return copy, nil
}

func (s *dispatchStoreStub) MarkValidationPublished(_ context.Context, _ string, digest runtimedomain.Digest) error {
	s.markCalls++
	if s.prepared == nil || s.prepared.Envelope.Digest != digest {
		return ErrInvalidPreparedEnvelope
	}
	if s.markErr == nil {
		s.prepared.Published = true
	}
	return s.markErr
}

type producerStub struct {
	received     ValidationDispatch
	prepared     PreparedCommandEnvelope
	prepareCalls int
	appendCalls  int
	appended     [][]byte
	deliveryIDs  []string
	appendErrors []error
}

func (p *producerStub) PrepareValidation(_ context.Context, dispatch ValidationDispatch) (PreparedCommandEnvelope, error) {
	p.prepareCalls++
	p.received = dispatch
	return p.prepared.Clone(), nil
}

func (p *producerStub) AppendPrepared(_ context.Context, deliveryID string, prepared PreparedCommandEnvelope) error {
	p.appendCalls++
	p.deliveryIDs = append(p.deliveryIDs, deliveryID)
	p.appended = append(p.appended, append([]byte(nil), prepared.Bytes...))
	if len(p.appendErrors) == 0 {
		return nil
	}
	err := p.appendErrors[0]
	p.appendErrors = p.appendErrors[1:]
	return err
}

func TestValidationDispatcherPublishesOnlyReferenceShapeAndLeavesFailedMarkReconcilable(t *testing.T) {
	dispatch := validValidationDispatch()
	store := &dispatchStoreStub{dispatch: dispatch, markErr: errors.New("commit unavailable")}
	producer := &producerStub{prepared: validPreparedEnvelope("key-a")}
	service, err := NewValidationDispatcher(store, producer)
	if err != nil {
		t.Fatal(err)
	}

	err = service.Dispatch(context.Background(), dispatch.OutboxID)
	if err == nil || store.markCalls != 1 || producer.prepareCalls != 1 || producer.appendCalls != 1 {
		t.Fatalf("expected prepare, append and failed durable mark, err=%v marks=%d prepares=%d appends=%d", err, store.markCalls, producer.prepareCalls, producer.appendCalls)
	}
	if producer.received != dispatch {
		t.Fatal("producer did not receive the immutable reference dispatch")
	}

	store.markErr = nil
	if err := service.Dispatch(context.Background(), dispatch.OutboxID); err != nil {
		t.Fatal(err)
	}
	if producer.prepareCalls != 1 || producer.appendCalls != 2 || !bytes.Equal(producer.appended[0], producer.appended[1]) {
		t.Fatalf("reconciliation did not reuse exact durable envelope: prepares=%d appends=%d", producer.prepareCalls, producer.appendCalls)
	}
}

func TestValidationDispatcherRetriesUnknownAppendWithDurableExactBytes(t *testing.T) {
	dispatch := validValidationDispatch()
	store := &dispatchStoreStub{dispatch: dispatch}
	producer := &producerStub{
		prepared:     validPreparedEnvelope("key-before-rotation"),
		appendErrors: []error{errors.New("bus response lost after publish")},
	}
	service, err := NewValidationDispatcher(store, producer)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Dispatch(context.Background(), dispatch.OutboxID); err == nil {
		t.Fatal("expected unknown append outcome")
	}
	// A retry after rotation must not invoke the signer or select new bytes.
	producer.prepared = validPreparedEnvelope("key-after-rotation")
	if err := service.Dispatch(context.Background(), dispatch.OutboxID); err != nil {
		t.Fatal(err)
	}
	if producer.prepareCalls != 1 || producer.appendCalls != 2 || !bytes.Equal(producer.appended[0], producer.appended[1]) || !store.prepared.Published {
		t.Fatalf("unknown-success retry changed its durable envelope: prepares=%d appends=%d published=%v", producer.prepareCalls, producer.appendCalls, store.prepared.Published)
	}
	if len(producer.deliveryIDs) != 2 || producer.deliveryIDs[0] != dispatch.OutboxID || producer.deliveryIDs[1] != dispatch.OutboxID {
		t.Fatalf("reconciliation changed stable delivery identity: %v", producer.deliveryIDs)
	}
}

func TestValidationDispatcherLeavesBackpressuredOutboxUnpublishedForRetry(t *testing.T) {
	dispatch := validValidationDispatch()
	store := &dispatchStoreStub{dispatch: dispatch}
	producer := &producerStub{
		prepared:     validPreparedEnvelope("key-a"),
		appendErrors: []error{ErrDispatchBackpressured},
	}
	service, err := NewValidationDispatcher(store, producer)
	if err != nil {
		t.Fatal(err)
	}

	if err := service.Dispatch(context.Background(), dispatch.OutboxID); !errors.Is(err, ErrDispatchBackpressured) {
		t.Fatalf("expected dispatch backpressure, got %v", err)
	}
	if store.prepared == nil || store.prepared.Published || store.markCalls != 0 {
		t.Fatalf("backpressured outbox was lost or marked published: prepared=%v marks=%d", store.prepared != nil, store.markCalls)
	}
	if err := service.Dispatch(context.Background(), dispatch.OutboxID); err != nil {
		t.Fatalf("retry prepared envelope after capacity recovery: %v", err)
	}
	if producer.prepareCalls != 1 || producer.appendCalls != 2 || store.markCalls != 1 || !store.prepared.Published || !bytes.Equal(producer.appended[0], producer.appended[1]) {
		t.Fatalf("capacity recovery did not retry exact durable bytes: prepares=%d appends=%d marks=%d published=%v", producer.prepareCalls, producer.appendCalls, store.markCalls, store.prepared.Published)
	}
}

func TestValidationDispatcherRejectsCorruptDurableEnvelopeBeforeAppend(t *testing.T) {
	dispatch := validValidationDispatch()
	corrupt := validPreparedEnvelope("key-a")
	corrupt.Bytes[0] ^= 0xff
	store := &dispatchStoreStub{dispatch: dispatch, prepared: &StoredPreparedEnvelope{Envelope: corrupt}}
	producer := &producerStub{prepared: validPreparedEnvelope("unused")}
	service, err := NewValidationDispatcher(store, producer)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Dispatch(context.Background(), dispatch.OutboxID); !errors.Is(err, ErrInvalidPreparedEnvelope) {
		t.Fatalf("expected corrupt durable envelope rejection, got %v", err)
	}
	if producer.prepareCalls != 0 || producer.appendCalls != 0 {
		t.Fatal("corrupt durable bytes reached preparation or the command-bus publish")
	}
}

func validPreparedEnvelope(keyID string) PreparedCommandEnvelope {
	encoded := []byte("signed-envelope:" + keyID)
	return PreparedCommandEnvelope{
		Bytes:            encoded,
		Digest:           runtimedomain.SHA256(encoded),
		SignatureProfile: 1,
		KeyID:            keyID,
	}
}

func validValidationDispatch() ValidationDispatch {
	return ValidationDispatch{
		OutboxID:              "outbox-1",
		CommandID:             "command-1",
		ExecutionID:           "execution-1",
		Generation:            1,
		DispatchOrdinal:       1,
		TenantID:              "tenant-1",
		ResourceProjectID:     "project-1",
		ProjectionProjectID:   "project-1",
		PrincipalRef:          "actor-1",
		InputBundleID:         "bundle-1",
		InputBundleVersion:    "bundle-v1",
		InputBundleMediaType:  "application/x-protobuf",
		InputBundleByteLength: 128,
		InputBundleDigest:     runtimedomain.SHA256([]byte("manifest")),
		CapabilityVersion:     "v1",
		ResourceClass:         "cpu-small",
		IsolationClass:        "credential-free",
		Priority:              1,
		Deadline:              time.Date(2026, time.July, 16, 12, 1, 0, 0, time.UTC),
		LimitsRevision:        "limits-v1",
		Command: configurationdomain.ValidationCommand{
			ConfigurationRevisionID: "revision-1",
			ConfigurationType:       "openapi",
			CatalogRevision:         "catalog-v1",
			CatalogDigest:           runtimedomain.SHA256([]byte("catalog")),
			SchemaID:                "openapi",
			SchemaRevision:          "schema-v1",
			SchemaDigest:            runtimedomain.SHA256([]byte("schema")),
			SettingsEntryID:         "settings",
		},
	}
}
