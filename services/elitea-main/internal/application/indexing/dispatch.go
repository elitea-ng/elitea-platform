package indexing

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
)

var ErrInvalidIndexIngestDispatch = errors.New("invalid index ingest dispatch")

// IndexIngestDispatch is the complete reference-only control-plane command.
// Input entry content, source files, credentials and results remain behind the
// immutable input-bundle reference and cannot be represented by this type.
type IndexIngestDispatch struct {
	OutboxID                    string
	CommandID                   string
	ExecutionID                 string
	Generation                  uint64
	DispatchOrdinal             uint64
	TenantID                    string
	ResourceProjectID           string
	ProjectionProjectID         string
	PrincipalRef                string
	InputBundleID               string
	InputBundleVersion          string
	InputBundleMediaType        string
	InputBundleByteLength       uint64
	InputBundleDigest           runtimedomain.Digest
	CapabilityVersion           string
	ResourceClass               string
	IsolationClass              string
	Priority                    uint32
	Deadline                    time.Time
	LimitsRevision              string
	Traceparent                 string
	Tracestate                  string
	ToolkitConfigurationEntryID string
	ToolParametersEntryID       string
	LLMModelEntryID             string
	LLMConfigurationEntryID     string
	MCPTokensEntryID            string
	EmbeddingBindingEntryID     string
	EmbeddingBindingDigest      runtimedomain.Digest
	ClientStreamID              string
	ClientMessageID             string
	SIOEvent                    string
	Initiator                   executiondomain.IndexIngestInitiator
	// ExpectedEmbeddingModel and ExpectedEmbeddingDimension are the embedding
	// space the index registry has stamped on the index (rust indexing runtime
	// only; empty when the index is not stamped yet, and always on the Python
	// path). The worker must refuse to write vectors of another space.
	ExpectedEmbeddingModel     string
	ExpectedEmbeddingDimension uint32
}

// MaxExpectedEmbeddingModelBytes bounds the expected model name: the limit the
// output plane applies to the stamp a result reports.
const MaxExpectedEmbeddingModelBytes = 256

func (d IndexIngestDispatch) Validate() error {
	required := []string{
		d.OutboxID, d.CommandID, d.ExecutionID, d.TenantID,
		d.ResourceProjectID, d.ProjectionProjectID, d.PrincipalRef,
		d.InputBundleID, d.InputBundleVersion, d.InputBundleMediaType,
		d.CapabilityVersion, d.ResourceClass, d.IsolationClass,
		d.LimitsRevision, d.ToolkitConfigurationEntryID,
		d.ToolParametersEntryID,
	}
	for _, value := range required {
		if !validDispatchText(value) {
			return ErrInvalidIndexIngestDispatch
		}
	}
	for _, value := range []string{
		d.Traceparent,
		d.Tracestate,
		d.LLMModelEntryID,
		d.LLMConfigurationEntryID,
		d.MCPTokensEntryID,
		d.EmbeddingBindingEntryID,
	} {
		if value != "" && !validDispatchText(value) {
			return ErrInvalidIndexIngestDispatch
		}
	}
	for _, value := range []string{d.ClientStreamID, d.ClientMessageID} {
		if value != "" && !validDispatchTextWithLimit(value, MaxClientCorrelationBytes) {
			return ErrInvalidIndexIngestDispatch
		}
	}
	if !validIndexSIOEvent(d.SIOEvent) || !d.Initiator.Valid() {
		return ErrInvalidIndexIngestDispatch
	}
	if d.Generation == 0 || d.DispatchOrdinal == 0 || d.InputBundleByteLength == 0 || d.InputBundleDigest.IsZero() || d.Priority == 0 || d.Deadline.IsZero() {
		return ErrInvalidIndexIngestDispatch
	}
	entryIDs := []string{
		d.ToolkitConfigurationEntryID,
		d.ToolParametersEntryID,
		d.LLMModelEntryID,
		d.LLMConfigurationEntryID,
		d.MCPTokensEntryID,
		d.EmbeddingBindingEntryID,
	}
	for position, entryID := range entryIDs {
		if entryID == "" {
			continue
		}
		for previous := 0; previous < position; previous++ {
			if entryID == entryIDs[previous] {
				return ErrInvalidIndexIngestDispatch
			}
		}
	}
	if (d.EmbeddingBindingEntryID == "") != d.EmbeddingBindingDigest.IsZero() {
		return ErrInvalidIndexIngestDispatch
	}
	if (d.ExpectedEmbeddingModel == "") != (d.ExpectedEmbeddingDimension == 0) ||
		(d.ExpectedEmbeddingModel != "" && !validDispatchTextWithLimit(d.ExpectedEmbeddingModel, MaxExpectedEmbeddingModelBytes)) ||
		d.ExpectedEmbeddingDimension > math.MaxUint16 {
		return ErrInvalidIndexIngestDispatch
	}
	return nil
}

func validDispatchText(value string) bool {
	return validDispatchTextWithLimit(value, maxIndexAdmissionStringBytes)
}

func validDispatchTextWithLimit(value string, maximum int) bool {
	return value != "" && len(value) <= maximum && !strings.ContainsAny(value, "\x00\r\n")
}

type PendingDispatchStore interface {
	LoadPendingIndexIngest(ctx context.Context, outboxID string) (IndexIngestDispatch, error)
	LoadPreparedIndexIngest(ctx context.Context, outboxID string) (*executionapp.StoredPreparedEnvelope, error)
	StorePreparedIndexIngest(ctx context.Context, outboxID string, candidate executionapp.PreparedCommandEnvelope) (executionapp.StoredPreparedEnvelope, error)
	MarkIndexIngestPublished(ctx context.Context, outboxID string, encodedDigest runtimedomain.Digest) error
}

type ReferenceCommandProducer interface {
	PrepareIndexIngest(ctx context.Context, dispatch IndexIngestDispatch) (executionapp.PreparedCommandEnvelope, error)
	AppendPrepared(ctx context.Context, deliveryID string, envelope executionapp.PreparedCommandEnvelope) error
}

// IndexIngestDispatcher selects one exact signed envelope durably before a
// command-bus publish. Every retry and competing publisher reloads that selected byte
// sequence; an unknown append result never causes re-signing.
type IndexIngestDispatcher struct {
	store    PendingDispatchStore
	producer ReferenceCommandProducer
}

func NewIndexIngestDispatcher(store PendingDispatchStore, producer ReferenceCommandProducer) (*IndexIngestDispatcher, error) {
	if store == nil || producer == nil {
		return nil, errors.New("index dispatch store and producer are required")
	}
	return &IndexIngestDispatcher{store: store, producer: producer}, nil
}

func (d *IndexIngestDispatcher) Dispatch(ctx context.Context, outboxID string) error {
	if outboxID == "" {
		return ErrInvalidIndexIngestDispatch
	}
	stored, err := d.store.LoadPreparedIndexIngest(ctx, outboxID)
	if errors.Is(err, executionapp.ErrDispatchRetired) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load prepared index ingest envelope: %w", err)
	}
	if stored == nil {
		dispatch, err := d.store.LoadPendingIndexIngest(ctx, outboxID)
		if err != nil {
			return fmt.Errorf("load pending index ingest dispatch: %w", err)
		}
		if dispatch.OutboxID != outboxID {
			return fmt.Errorf("%w: outbox identity mismatch", ErrInvalidIndexIngestDispatch)
		}
		if err := dispatch.Validate(); err != nil {
			return err
		}

		candidate, err := d.producer.PrepareIndexIngest(ctx, dispatch)
		if err != nil {
			return fmt.Errorf("prepare index ingest reference: %w", err)
		}
		if err := candidate.Validate(); err != nil {
			return err
		}
		selected, err := d.store.StorePreparedIndexIngest(ctx, outboxID, candidate)
		if errors.Is(err, executionapp.ErrDispatchRetired) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("store prepared index ingest envelope: %w", err)
		}
		stored = &selected
	}
	if err := stored.Validate(); err != nil {
		return err
	}
	if err := d.producer.AppendPrepared(ctx, outboxID, stored.Envelope.Clone()); err != nil {
		return fmt.Errorf("append prepared index ingest reference: %w", err)
	}
	if err := d.store.MarkIndexIngestPublished(ctx, outboxID, stored.Envelope.Digest); err != nil {
		if errors.Is(err, executionapp.ErrDispatchRetired) {
			return nil
		}
		return fmt.Errorf("mark index ingest reference published: %w", err)
	}
	return nil
}

type PendingIndexIngestOutbox interface {
	RetireNoAuthorityIndexIngest(ctx context.Context, limit int) (int, error)
	ListPendingIndexIngestIDs(ctx context.Context, limit int, visibilityTimeout time.Duration) ([]string, error)
}

type pendingOutboxAdapter struct {
	outbox PendingIndexIngestOutbox
}

func (a pendingOutboxAdapter) RetireNoAuthorityValidation(ctx context.Context, limit int) (int, error) {
	return a.outbox.RetireNoAuthorityIndexIngest(ctx, limit)
}

func (a pendingOutboxAdapter) ListPendingValidationIDs(ctx context.Context, limit int, visibilityTimeout time.Duration) ([]string, error) {
	return a.outbox.ListPendingIndexIngestIDs(ctx, limit, visibilityTimeout)
}

// NewIndexIngestOutboxPublisher reuses the established bounded polling,
// concurrency, cancellation and retry/backoff implementation. The private
// adapter keeps the existing validation-specific repository API unchanged.
func NewIndexIngestOutboxPublisher(outbox PendingIndexIngestOutbox, dispatcher *IndexIngestDispatcher, config executionapp.OutboxPublisherConfig) (*executionapp.OutboxPublisher, error) {
	if outbox == nil || dispatcher == nil {
		return nil, errors.New("pending index outbox and dispatcher are required")
	}
	return executionapp.NewOutboxPublisher(pendingOutboxAdapter{outbox: outbox}, dispatcher, config)
}
