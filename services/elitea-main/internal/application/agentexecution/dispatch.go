package agentexecution

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

var ErrInvalidAgentExecutionDispatch = errors.New("invalid agent execution dispatch")

// AgentExecutionDispatch is the complete reference-only control-plane command
// for both current agent entry points. The resolved request, model and tool
// settings, credentials, conversation history, resume values and results stay
// behind InputBundleID/RequestEntryID and cannot be represented by this type.
type AgentExecutionDispatch struct {
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
	ClientStreamID        string
	ClientMessageID       string
	SIOEvent              string
}

func (d AgentExecutionDispatch) Validate() error {
	required := []string{
		d.OutboxID, d.CommandID, d.ExecutionID, d.TenantID,
		d.ResourceProjectID, d.ProjectionProjectID, d.PrincipalRef,
		d.InputBundleID, d.InputBundleVersion, d.InputBundleMediaType,
		d.CapabilityID, d.CapabilityVersion, d.ResourceClass,
		d.IsolationClass, d.LimitsRevision, d.RequestEntryID,
		d.ClientStreamID, d.ClientMessageID, d.SIOEvent,
	}
	for _, value := range required {
		if !validAgentDispatchText(value) {
			return ErrInvalidAgentExecutionDispatch
		}
	}
	for _, value := range []string{d.Traceparent, d.Tracestate} {
		if value != "" && !validAgentDispatchText(value) {
			return ErrInvalidAgentExecutionDispatch
		}
	}
	if !agentCapability(d.CapabilityID) ||
		(d.SIOEvent != "chat_predict" && d.SIOEvent != "chat_continue_predict") ||
		d.Generation == 0 || d.DispatchOrdinal == 0 ||
		d.InputBundleByteLength == 0 || d.InputBundleDigest.IsZero() ||
		d.Priority == 0 || d.Deadline.IsZero() {
		return ErrInvalidAgentExecutionDispatch
	}
	return nil
}

func validAgentDispatchText(value string) bool {
	return value != "" && len(value) <= executiondomain.MaxIndexMetaCorrelationBytes &&
		!strings.ContainsAny(value, "\x00\r\n")
}

type PendingDispatchStore interface {
	LoadPendingAgentExecution(context.Context, string) (AgentExecutionDispatch, error)
	LoadPreparedAgentExecution(context.Context, string) (*executionapp.StoredPreparedEnvelope, error)
	StorePreparedAgentExecution(context.Context, string, executionapp.PreparedCommandEnvelope) (executionapp.StoredPreparedEnvelope, error)
	MarkAgentExecutionPublished(context.Context, string, runtimedomain.Digest) error
}

type ReferenceCommandProducer interface {
	PrepareAgentExecution(context.Context, AgentExecutionDispatch) (executionapp.PreparedCommandEnvelope, error)
	AppendPrepared(context.Context, string, executionapp.PreparedCommandEnvelope) error
}

// Dispatcher durably selects one signed byte sequence before the command-bus publish.
// Publish or acknowledgement uncertainty therefore retries the exact command
// and never re-signs a possibly different agent request.
type Dispatcher struct {
	store    PendingDispatchStore
	producer ReferenceCommandProducer
}

func NewDispatcher(store PendingDispatchStore, producer ReferenceCommandProducer) (*Dispatcher, error) {
	if store == nil || producer == nil {
		return nil, errors.New("agent dispatch store and producer are required")
	}
	return &Dispatcher{store: store, producer: producer}, nil
}

func (d *Dispatcher) Dispatch(ctx context.Context, outboxID string) error {
	if outboxID == "" {
		return ErrInvalidAgentExecutionDispatch
	}
	stored, err := d.store.LoadPreparedAgentExecution(ctx, outboxID)
	if errors.Is(err, executionapp.ErrDispatchRetired) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load prepared agent execution envelope: %w", err)
	}
	if stored == nil {
		dispatch, err := d.store.LoadPendingAgentExecution(ctx, outboxID)
		if err != nil {
			return fmt.Errorf("load pending agent execution dispatch: %w", err)
		}
		if dispatch.OutboxID != outboxID {
			return fmt.Errorf("%w: outbox identity mismatch", ErrInvalidAgentExecutionDispatch)
		}
		if err := dispatch.Validate(); err != nil {
			return err
		}
		candidate, err := d.producer.PrepareAgentExecution(ctx, dispatch)
		if err != nil {
			return fmt.Errorf("prepare agent execution reference: %w", err)
		}
		if err := candidate.Validate(); err != nil {
			return err
		}
		selected, err := d.store.StorePreparedAgentExecution(ctx, outboxID, candidate)
		if errors.Is(err, executionapp.ErrDispatchRetired) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("store prepared agent execution envelope: %w", err)
		}
		stored = &selected
	}
	if err := stored.Validate(); err != nil {
		return err
	}
	if err := d.producer.AppendPrepared(ctx, outboxID, stored.Envelope.Clone()); err != nil {
		return fmt.Errorf("append prepared agent execution reference: %w", err)
	}
	if err := d.store.MarkAgentExecutionPublished(ctx, outboxID, stored.Envelope.Digest); err != nil {
		if errors.Is(err, executionapp.ErrDispatchRetired) {
			return nil
		}
		return fmt.Errorf("mark agent execution reference published: %w", err)
	}
	return nil
}

type PendingOutbox interface {
	RetireNoAuthorityAgentExecution(context.Context, int) (int, error)
	ListPendingAgentExecutionIDs(context.Context, int, time.Duration) ([]string, error)
}

type pendingOutboxAdapter struct {
	outbox PendingOutbox
}

func (a pendingOutboxAdapter) RetireNoAuthorityValidation(ctx context.Context, limit int) (int, error) {
	return a.outbox.RetireNoAuthorityAgentExecution(ctx, limit)
}

func (a pendingOutboxAdapter) ListPendingValidationIDs(ctx context.Context, limit int, visibilityTimeout time.Duration) ([]string, error) {
	return a.outbox.ListPendingAgentExecutionIDs(ctx, limit, visibilityTimeout)
}

// NewOutboxPublisher reuses the bounded polling, concurrency, cancellation and
// retry implementation while keeping repository selection capability-scoped.
func NewOutboxPublisher(outbox PendingOutbox, dispatcher *Dispatcher, config executionapp.OutboxPublisherConfig) (*executionapp.OutboxPublisher, error) {
	if outbox == nil || dispatcher == nil {
		return nil, errors.New("pending agent outbox and dispatcher are required")
	}
	return executionapp.NewOutboxPublisher(pendingOutboxAdapter{outbox: outbox}, dispatcher, config)
}
