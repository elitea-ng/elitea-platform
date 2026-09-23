package toolkitexecution

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
)

var ErrInvalidToolkitExecuteReadDispatch = errors.New("invalid direct toolkit execution dispatch")

// ToolkitExecuteReadDispatch is the complete reference-only command for one
// external MCP read-tool call. Protected settings and invocation arguments
// remain behind InputBundleID/RequestEntryID.
type ToolkitExecuteReadDispatch struct {
	OutboxID              string
	CommandID             string
	ExecutionID           string
	Generation            uint64
	DispatchOrdinal       uint64
	TenantID              string
	ResourceProjectID     string
	ProjectionProjectID   string
	PrincipalRef          string
	InputBundleID         string
	InputBundleVersion    string
	InputBundleMediaType  string
	InputBundleByteLength uint64
	InputBundleDigest     runtimedomain.Digest
	CapabilityID          string
	CapabilityVersion     string
	ResourceClass         string
	IsolationClass        string
	Priority              uint32
	Deadline              time.Time
	LimitsRevision        string
	Traceparent           string
	Tracestate            string
	RequestEntryID        string
}

func (d ToolkitExecuteReadDispatch) Validate() error {
	required := []string{
		d.OutboxID, d.CommandID, d.ExecutionID, d.TenantID,
		d.ResourceProjectID, d.ProjectionProjectID, d.PrincipalRef,
		d.InputBundleID, d.InputBundleVersion, d.InputBundleMediaType,
		d.CapabilityID, d.CapabilityVersion, d.ResourceClass,
		d.IsolationClass, d.LimitsRevision, d.RequestEntryID,
	}
	for _, value := range required {
		if !validDispatchText(value) {
			return ErrInvalidToolkitExecuteReadDispatch
		}
	}
	for _, value := range []string{d.Traceparent, d.Tracestate} {
		if value != "" && !validDispatchText(value) {
			return ErrInvalidToolkitExecuteReadDispatch
		}
	}
	if d.CapabilityID != executiondomain.ToolkitExecuteReadCapability ||
		d.Generation == 0 || d.DispatchOrdinal == 0 || d.InputBundleByteLength == 0 ||
		d.InputBundleDigest.IsZero() || d.Priority == 0 || d.Deadline.IsZero() {
		return ErrInvalidToolkitExecuteReadDispatch
	}
	return nil
}

func validDispatchText(value string) bool {
	return value != "" && len(value) <= executiondomain.MaxIndexMetaCorrelationBytes &&
		!strings.ContainsAny(value, "\x00\r\n")
}

type PendingDispatchStore interface {
	LoadPendingToolkitExecuteRead(context.Context, string) (ToolkitExecuteReadDispatch, error)
	LoadPreparedToolkitExecuteRead(context.Context, string) (*executionapp.StoredPreparedEnvelope, error)
	StorePreparedToolkitExecuteRead(context.Context, string, executionapp.PreparedCommandEnvelope) (executionapp.StoredPreparedEnvelope, error)
	MarkToolkitExecuteReadPublished(context.Context, string, runtimedomain.Digest) error
}

type ReferenceCommandProducer interface {
	PrepareToolkitExecuteRead(context.Context, ToolkitExecuteReadDispatch) (executionapp.PreparedCommandEnvelope, error)
	AppendPrepared(context.Context, string, executionapp.PreparedCommandEnvelope) error
}

type Dispatcher struct {
	store    PendingDispatchStore
	producer ReferenceCommandProducer
}

func NewDispatcher(store PendingDispatchStore, producer ReferenceCommandProducer) (*Dispatcher, error) {
	if store == nil || producer == nil {
		return nil, errors.New("direct toolkit dispatch store and producer are required")
	}
	return &Dispatcher{store: store, producer: producer}, nil
}

func (d *Dispatcher) Dispatch(ctx context.Context, outboxID string) error {
	if outboxID == "" {
		return ErrInvalidToolkitExecuteReadDispatch
	}
	stored, err := d.store.LoadPreparedToolkitExecuteRead(ctx, outboxID)
	if errors.Is(err, executionapp.ErrDispatchRetired) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load prepared direct toolkit envelope: %w", err)
	}
	if stored == nil {
		dispatch, err := d.store.LoadPendingToolkitExecuteRead(ctx, outboxID)
		if err != nil {
			return fmt.Errorf("load pending direct toolkit dispatch: %w", err)
		}
		if dispatch.OutboxID != outboxID {
			return fmt.Errorf("%w: outbox identity mismatch", ErrInvalidToolkitExecuteReadDispatch)
		}
		if err := dispatch.Validate(); err != nil {
			return err
		}
		candidate, err := d.producer.PrepareToolkitExecuteRead(ctx, dispatch)
		if err != nil {
			return fmt.Errorf("prepare direct toolkit reference: %w", err)
		}
		if err := candidate.Validate(); err != nil {
			return err
		}
		selected, err := d.store.StorePreparedToolkitExecuteRead(ctx, outboxID, candidate)
		if errors.Is(err, executionapp.ErrDispatchRetired) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("store prepared direct toolkit envelope: %w", err)
		}
		stored = &selected
	}
	if err := stored.Validate(); err != nil {
		return err
	}
	if err := d.producer.AppendPrepared(ctx, outboxID, stored.Envelope.Clone()); err != nil {
		return fmt.Errorf("append prepared direct toolkit reference: %w", err)
	}
	if err := d.store.MarkToolkitExecuteReadPublished(ctx, outboxID, stored.Envelope.Digest); err != nil {
		if errors.Is(err, executionapp.ErrDispatchRetired) {
			return nil
		}
		return fmt.Errorf("mark direct toolkit reference published: %w", err)
	}
	return nil
}

type PendingOutbox interface {
	RetireNoAuthorityToolkitExecuteRead(context.Context, int) (int, error)
	ListPendingToolkitExecuteReadIDs(context.Context, int, time.Duration) ([]string, error)
}

type pendingOutboxAdapter struct{ outbox PendingOutbox }

func (a pendingOutboxAdapter) RetireNoAuthorityValidation(ctx context.Context, limit int) (int, error) {
	return a.outbox.RetireNoAuthorityToolkitExecuteRead(ctx, limit)
}

func (a pendingOutboxAdapter) ListPendingValidationIDs(
	ctx context.Context,
	limit int,
	visibilityTimeout time.Duration,
) ([]string, error) {
	return a.outbox.ListPendingToolkitExecuteReadIDs(ctx, limit, visibilityTimeout)
}

func NewOutboxPublisher(
	outbox PendingOutbox,
	dispatcher *Dispatcher,
	config executionapp.OutboxPublisherConfig,
) (*executionapp.OutboxPublisher, error) {
	if outbox == nil || dispatcher == nil {
		return nil, errors.New("pending direct toolkit outbox and dispatcher are required")
	}
	return executionapp.NewOutboxPublisher(pendingOutboxAdapter{outbox: outbox}, dispatcher, config)
}
