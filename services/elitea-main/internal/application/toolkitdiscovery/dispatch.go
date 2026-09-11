package toolkitdiscovery

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

var ErrInvalidDiscoveryDispatch = errors.New("invalid toolkit discovery dispatch")

type Dispatch struct {
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
	ToolkitType           string
	SettingsEntryID       string
}

func (d Dispatch) Validate() error {
	required := []string{
		d.OutboxID, d.CommandID, d.ExecutionID, d.TenantID,
		d.ResourceProjectID, d.ProjectionProjectID, d.PrincipalRef,
		d.InputBundleID, d.InputBundleVersion, d.InputBundleMediaType,
		d.CapabilityID, d.CapabilityVersion, d.ResourceClass,
		d.IsolationClass, d.LimitsRevision,
		d.ToolkitType,
		d.SettingsEntryID,
	}
	for _, value := range required {
		if !validDispatchText(value) {
			return ErrInvalidDiscoveryDispatch
		}
	}
	for _, value := range []string{d.Traceparent, d.Tracestate} {
		if value != "" && !validDispatchText(value) {
			return ErrInvalidDiscoveryDispatch
		}
	}
	if d.CapabilityID != executiondomain.ToolkitAvailableToolsCapability ||
		d.Generation == 0 || d.DispatchOrdinal == 0 ||
		d.InputBundleByteLength == 0 || d.InputBundleDigest.IsZero() ||
		d.Priority == 0 || d.Deadline.IsZero() {
		return ErrInvalidDiscoveryDispatch
	}
	return nil
}

func validDispatchText(value string) bool {
	return value != "" && len(value) <= executiondomain.MaxIndexMetaCorrelationBytes &&
		!strings.ContainsAny(value, "\x00\r\n")
}

type PendingDispatchStore interface {
	LoadPreparedToolkitAvailableTools(context.Context, string) (*executionapp.StoredPreparedEnvelope, error)
	StorePreparedToolkitAvailableTools(context.Context, string, executionapp.PreparedCommandEnvelope) (executionapp.StoredPreparedEnvelope, error)
	MarkToolkitAvailableToolsPublished(context.Context, string, runtimedomain.Digest) error
}

type ReferenceCommandProducer interface {
	PrepareToolkitAvailableTools(context.Context, Dispatch) (executionapp.PreparedCommandEnvelope, error)
	AppendPrepared(context.Context, string, executionapp.PreparedCommandEnvelope) error
}

type Dispatcher struct {
	store    PendingDispatchStore
	producer ReferenceCommandProducer
}

func NewDispatcher(store PendingDispatchStore, producer ReferenceCommandProducer) (*Dispatcher, error) {
	if store == nil || producer == nil {
		return nil, errors.New("toolkit discovery dispatch store and producer are required")
	}
	return &Dispatcher{store: store, producer: producer}, nil
}

func (d *Dispatcher) Dispatch(ctx context.Context, dispatch Dispatch) error {
	if err := dispatch.Validate(); err != nil {
		return err
	}
	stored, err := d.store.LoadPreparedToolkitAvailableTools(ctx, dispatch.OutboxID)
	if errors.Is(err, executionapp.ErrDispatchRetired) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load prepared toolkit discovery envelope: %w", err)
	}
	if stored == nil {
		candidate, err := d.producer.PrepareToolkitAvailableTools(ctx, dispatch)
		if err != nil {
			return fmt.Errorf("prepare toolkit discovery reference: %w", err)
		}
		if err := candidate.Validate(); err != nil {
			return err
		}
		selected, err := d.store.StorePreparedToolkitAvailableTools(ctx, dispatch.OutboxID, candidate)
		if errors.Is(err, executionapp.ErrDispatchRetired) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("store prepared toolkit discovery envelope: %w", err)
		}
		stored = &selected
	}
	if err := stored.Validate(); err != nil {
		return err
	}
	if err := d.producer.AppendPrepared(ctx, dispatch.OutboxID, stored.Envelope.Clone()); err != nil {
		return fmt.Errorf("append prepared toolkit discovery reference: %w", err)
	}
	if err := d.store.MarkToolkitAvailableToolsPublished(ctx, dispatch.OutboxID, stored.Envelope.Digest); err != nil {
		if errors.Is(err, executionapp.ErrDispatchRetired) {
			return nil
		}
		return fmt.Errorf("mark toolkit discovery reference published: %w", err)
	}
	return nil
}
