package execution

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	configurationdomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/configurations"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
)

var (
	ErrInvalidDispatch         = errors.New("invalid configuration validation dispatch")
	ErrInvalidPreparedEnvelope = errors.New("invalid prepared worker-command envelope")
	ErrDispatchRetired         = errors.New("validation dispatch was durably retired")
	ErrDispatchDeadlineExpired = errors.New("validation dispatch deadline expired")
	ErrDispatchBackpressured   = errors.New("validation dispatch is temporarily backpressured")
)

// ValidationDispatch is deliberately reference-only. The admitted manifest
// and settings bytes remain in durable input storage and are never exposed by
// this type or its producer interface.
type ValidationDispatch struct {
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
	CapabilityVersion     string
	ResourceClass         string
	IsolationClass        string
	Priority              uint32
	Deadline              time.Time
	LimitsRevision        string
	Traceparent           string
	Tracestate            string
	Command               configurationdomain.ValidationCommand
}

func (d ValidationDispatch) Validate() error {
	if d.OutboxID == "" || d.CommandID == "" || d.ExecutionID == "" || d.Generation == 0 || d.DispatchOrdinal == 0 || d.TenantID == "" || d.ResourceProjectID == "" || d.ProjectionProjectID == "" || d.PrincipalRef == "" {
		return ErrInvalidDispatch
	}
	if d.InputBundleID == "" || d.InputBundleVersion == "" || d.InputBundleMediaType == "" || d.InputBundleByteLength == 0 || d.InputBundleDigest.IsZero() {
		return ErrInvalidDispatch
	}
	if d.CapabilityVersion == "" || d.ResourceClass == "" || d.IsolationClass == "" || d.Priority == 0 || d.Deadline.IsZero() || d.LimitsRevision == "" {
		return ErrInvalidDispatch
	}
	if err := d.Command.Validate(); err != nil {
		return ErrInvalidDispatch
	}
	return nil
}

type PendingDispatchStore interface {
	LoadPendingValidation(ctx context.Context, outboxID string) (ValidationDispatch, error)
	LoadPreparedValidation(ctx context.Context, outboxID string) (*StoredPreparedEnvelope, error)
	StorePreparedValidation(ctx context.Context, outboxID string, candidate PreparedCommandEnvelope) (StoredPreparedEnvelope, error)
	MarkValidationPublished(ctx context.Context, outboxID string, encodedDigest runtimedomain.Digest) error
}

// PreparedCommandEnvelope is the exact bounded byte sequence selected for one
// outbox identity before any command bus publish. Bytes are immutable once stored;
// retries and competing publisher instances must append the durable winner
// rather than invoking the signer again.
type PreparedCommandEnvelope struct {
	Bytes            []byte
	Digest           runtimedomain.Digest
	SignatureProfile int32
	KeyID            string
}

func (e PreparedCommandEnvelope) Validate() error {
	if len(e.Bytes) == 0 || e.Digest.IsZero() || e.SignatureProfile <= 0 || e.KeyID == "" {
		return ErrInvalidPreparedEnvelope
	}
	if runtimedomain.SHA256(e.Bytes) != e.Digest {
		return ErrInvalidPreparedEnvelope
	}
	return nil
}

func (e PreparedCommandEnvelope) Clone() PreparedCommandEnvelope {
	e.Bytes = bytes.Clone(e.Bytes)
	return e
}

// StoredPreparedEnvelope adds the latest PostgreSQL publication observation to
// a validated durable envelope. Published is not a permanent worker receipt;
// a visibility-expired row is intentionally offered to the command bus again.
type StoredPreparedEnvelope struct {
	Envelope  PreparedCommandEnvelope
	Published bool
}

func (e StoredPreparedEnvelope) Validate() error {
	return e.Envelope.Validate()
}

type ReferenceCommandProducer interface {
	PrepareValidation(ctx context.Context, dispatch ValidationDispatch) (PreparedCommandEnvelope, error)
	AppendPrepared(ctx context.Context, deliveryID string, envelope PreparedCommandEnvelope) error
}

type ValidationDispatcher struct {
	store    PendingDispatchStore
	producer ReferenceCommandProducer
}

func NewValidationDispatcher(store PendingDispatchStore, producer ReferenceCommandProducer) (*ValidationDispatcher, error) {
	if store == nil || producer == nil {
		return nil, errors.New("dispatch store and producer are required")
	}
	return &ValidationDispatcher{store: store, producer: producer}, nil
}

func (d *ValidationDispatcher) Dispatch(ctx context.Context, outboxID string) error {
	if outboxID == "" {
		return ErrInvalidDispatch
	}
	stored, err := d.store.LoadPreparedValidation(ctx, outboxID)
	if errors.Is(err, ErrDispatchRetired) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load prepared validation envelope: %w", err)
	}
	if stored == nil {
		dispatch, err := d.store.LoadPendingValidation(ctx, outboxID)
		if err != nil {
			return fmt.Errorf("load pending validation dispatch: %w", err)
		}
		if dispatch.OutboxID != outboxID {
			return fmt.Errorf("%w: outbox identity mismatch", ErrInvalidDispatch)
		}
		if err := dispatch.Validate(); err != nil {
			return err
		}

		candidate, err := d.producer.PrepareValidation(ctx, dispatch)
		if err != nil {
			return fmt.Errorf("prepare validation reference: %w", err)
		}
		if err := candidate.Validate(); err != nil {
			return err
		}
		selected, err := d.store.StorePreparedValidation(ctx, outboxID, candidate)
		if errors.Is(err, ErrDispatchRetired) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("store prepared validation envelope: %w", err)
		}
		stored = &selected
	}
	if err := stored.Validate(); err != nil {
		return err
	}
	// Published is only the latest PostgreSQL visibility observation, not a
	// permanent delivery acknowledgement. The publisher may deliberately call
	// Dispatch again after the bounded visibility interval. The bus suppresses a
	// duplicate for the same stable outbox identity and exact envelope bytes.
	if err := d.producer.AppendPrepared(ctx, outboxID, stored.Envelope.Clone()); err != nil {
		return fmt.Errorf("append prepared validation reference: %w", err)
	}
	if err := d.store.MarkValidationPublished(ctx, outboxID, stored.Envelope.Digest); err != nil {
		if errors.Is(err, ErrDispatchRetired) {
			return nil
		}
		// An unknown publish success remains safely reconcilable because the next
		// attempt reloads and appends these exact durable bytes.
		return fmt.Errorf("mark validation reference published: %w", err)
	}
	return nil
}
