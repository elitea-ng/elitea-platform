package toolkitcalltool

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

var ErrInvalidToolRunDispatch = errors.New("invalid tool-run dispatch")

// Dispatch is the complete reference-only control-plane command for one tool
// run. The redeemed settings, the caller's arguments and the tool's answer stay
// behind InputBundleID and the two entry ids and cannot be represented here.
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
	ToolName              string
	ToolkitID             string
	ToolkitVersion        string
	SettingsEntryID       string
	ArgumentsEntryID      string
}

func (d Dispatch) Validate() error {
	required := []string{
		d.OutboxID, d.CommandID, d.ExecutionID, d.TenantID,
		d.ResourceProjectID, d.ProjectionProjectID, d.PrincipalRef,
		d.InputBundleID, d.InputBundleVersion, d.InputBundleMediaType,
		d.CapabilityID, d.CapabilityVersion, d.ResourceClass,
		d.IsolationClass, d.LimitsRevision,
		d.ToolkitType, d.ToolName, d.ToolkitID,
		d.SettingsEntryID, d.ArgumentsEntryID,
	}
	for _, value := range required {
		if !validDispatchText(value) {
			return ErrInvalidToolRunDispatch
		}
	}
	for _, value := range []string{d.Traceparent, d.Tracestate, d.ToolkitVersion} {
		if value != "" && !validDispatchText(value) {
			return ErrInvalidToolRunDispatch
		}
	}
	if d.CapabilityID != executiondomain.ToolkitCallToolCapability ||
		d.SettingsEntryID == d.ArgumentsEntryID ||
		d.Generation == 0 || d.DispatchOrdinal == 0 ||
		d.InputBundleByteLength == 0 || d.InputBundleDigest.IsZero() ||
		d.Priority == 0 || d.Deadline.IsZero() {
		return ErrInvalidToolRunDispatch
	}
	return nil
}

func validDispatchText(value string) bool {
	return value != "" && len(value) <= executiondomain.MaxIndexMetaCorrelationBytes &&
		!strings.ContainsAny(value, "\x00\r\n")
}

// PendingDispatchStore is the durable half of the one-shot publish. There is no
// LoadPending here on purpose: the dispatch is handed in from admission, which
// is the same request.
type PendingDispatchStore interface {
	LoadPreparedToolkitCallTool(context.Context, string) (*executionapp.StoredPreparedEnvelope, error)
	StorePreparedToolkitCallTool(context.Context, string, executionapp.PreparedCommandEnvelope) (executionapp.StoredPreparedEnvelope, error)
	MarkToolkitCallToolPublished(context.Context, string, runtimedomain.Digest) error
}

type ReferenceCommandProducer interface {
	PrepareToolkitCallTool(context.Context, Dispatch) (executionapp.PreparedCommandEnvelope, error)
	AppendPrepared(context.Context, string, executionapp.PreparedCommandEnvelope) error
}

// Dispatcher durably selects one signed byte sequence before the Redis append,
// exactly as the agent and index publishers do. Redis or acknowledgement
// uncertainty therefore retries the exact command and never re-signs a possibly
// different tool run.
type Dispatcher struct {
	store    PendingDispatchStore
	producer ReferenceCommandProducer
}

func NewDispatcher(store PendingDispatchStore, producer ReferenceCommandProducer) (*Dispatcher, error) {
	if store == nil || producer == nil {
		return nil, errors.New("tool-run dispatch store and producer are required")
	}
	return &Dispatcher{store: store, producer: producer}, nil
}

func (d *Dispatcher) Dispatch(ctx context.Context, dispatch Dispatch) error {
	if err := dispatch.Validate(); err != nil {
		return err
	}
	stored, err := d.store.LoadPreparedToolkitCallTool(ctx, dispatch.OutboxID)
	if errors.Is(err, executionapp.ErrDispatchRetired) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load prepared tool-run envelope: %w", err)
	}
	if stored == nil {
		candidate, err := d.producer.PrepareToolkitCallTool(ctx, dispatch)
		if err != nil {
			return fmt.Errorf("prepare tool-run reference: %w", err)
		}
		if err := candidate.Validate(); err != nil {
			return err
		}
		selected, err := d.store.StorePreparedToolkitCallTool(ctx, dispatch.OutboxID, candidate)
		if errors.Is(err, executionapp.ErrDispatchRetired) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("store prepared tool-run envelope: %w", err)
		}
		stored = &selected
	}
	if err := stored.Validate(); err != nil {
		return err
	}
	if err := d.producer.AppendPrepared(ctx, dispatch.OutboxID, stored.Envelope.Clone()); err != nil {
		return fmt.Errorf("append prepared tool-run reference: %w", err)
	}
	if err := d.store.MarkToolkitCallToolPublished(ctx, dispatch.OutboxID, stored.Envelope.Digest); err != nil {
		if errors.Is(err, executionapp.ErrDispatchRetired) {
			return nil
		}
		return fmt.Errorf("mark tool-run reference published: %w", err)
	}
	return nil
}
