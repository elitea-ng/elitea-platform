package output

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
)

type RuntimeFailure struct {
	Code        string
	SafeMessage string
	Retryable   bool
}

type RuntimeFailureFrame struct {
	StreamID              string
	TenantID              string
	ResourceProjectID     string
	ProjectionProjectID   string
	WorkloadSessionID     string
	ProducerID            string
	EventID               string
	LogicalOutputID       string
	Sequence              uint64
	ClaimHandoffWatermark uint64
	OccurredAt            time.Time
	Fence                 runtimedomain.Fence
	PayloadDigest         runtimedomain.Digest
	EncodedFailure        []byte
	Settlement            executionapp.SettlementProposal
	EncodedSettlement     []byte
	Failure               RuntimeFailure
}

func (f RuntimeFailureFrame) Validate() error {
	if f.StreamID == "" || f.TenantID == "" || f.ResourceProjectID == "" || f.ProjectionProjectID == "" || f.WorkloadSessionID == "" || f.ProducerID == "" || f.EventID == "" || f.LogicalOutputID == "" || f.Sequence == 0 || f.OccurredAt.IsZero() {
		return ErrInvalidValidationOutput
	}
	if strings.ContainsAny(f.EventID, "\r\n") || strings.ContainsAny(f.LogicalOutputID, "\r\n") || f.WorkloadSessionID != f.Fence.WorkloadSessionID || f.ProducerID != f.Fence.ProducerID {
		return ErrInvalidValidationOutput
	}
	if err := f.Fence.Validate(); err != nil {
		return err
	}
	if !matchesCanonicalTerminalIdentity(
		f.StreamID,
		f.EventID,
		f.LogicalOutputID,
		f.Sequence,
		f.Settlement.ProposalID,
		f.Settlement.IdempotencyKey,
		f.Fence,
		"",
	) {
		return ErrInvalidValidationOutput
	}
	if f.PayloadDigest.IsZero() || len(f.EncodedFailure) == 0 || len(f.EncodedFailure) > MaxConfigurationValidationResultBytes || runtimedomain.SHA256(f.EncodedFailure) != f.PayloadDigest {
		return ErrInvalidValidationOutput
	}
	if f.Failure.Code == "" || f.Failure.SafeMessage == "" || len(f.Failure.Code) > 256 || len(f.Failure.SafeMessage) > 256 {
		return ErrInvalidValidationOutput
	}
	expectedOutcome := executionapp.SettlementFailed
	if f.Failure.Code == "CANCELLED" {
		expectedOutcome = executionapp.SettlementCancelled
	}
	if err := f.Settlement.Validate(); err != nil || f.Settlement.Fence != f.Fence || f.Settlement.Outcome != expectedOutcome || f.Settlement.TerminalLogicalOutputID != f.LogicalOutputID || f.Settlement.TerminalEventID != f.EventID || f.Settlement.TerminalSequence != f.Sequence || f.Settlement.TerminalPayloadDigest != f.PayloadDigest || len(f.EncodedSettlement) == 0 || len(f.EncodedSettlement) > MaxConfigurationValidationResultBytes || runtimedomain.SHA256(f.EncodedSettlement) != f.Settlement.ProposalDigest {
		return ErrInvalidValidationOutput
	}
	return nil
}

type RuntimeFailureProjection struct {
	Frame        RuntimeFailureFrame
	BrowserData  json.RawMessage
	CapabilityID string
}

// ExpectedRuntimeFailure is the capability-specific terminal identity derived
// from the admitted execution. Runtime-error frames deliberately carry no
// caller-selected capability identifier, so this binding must come from the
// durable control plane before a failure can be projected or settled.
type ExpectedRuntimeFailure struct {
	TenantID            string
	ResourceProjectID   string
	ProjectionProjectID string
	CapabilityID        string
	CommandID           string
	ExecutionID         string
	Generation          uint64
	LogicalOutputID     string
}

func (e ExpectedRuntimeFailure) Validate() error {
	if e.TenantID == "" || e.ResourceProjectID == "" || e.ProjectionProjectID == "" || e.CommandID == "" || e.ExecutionID == "" || e.Generation == 0 || e.LogicalOutputID == "" || strings.ContainsAny(e.LogicalOutputID, "\r\n") {
		return ErrInvalidValidationOutput
	}
	switch e.CapabilityID {
	case executiondomain.ConfigurationValidationCapability:
		const prefix = "configuration-validation:"
		if !strings.HasPrefix(e.LogicalOutputID, prefix) || len(e.LogicalOutputID) == len(prefix) {
			return ErrInvalidValidationOutput
		}
	case executiondomain.IndexIngestCapability:
		if e.LogicalOutputID != "index-ingest:"+e.ExecutionID {
			return ErrInvalidValidationOutput
		}
	case executiondomain.AgentApplicationCapability, executiondomain.AgentAdhocCapability:
		if e.LogicalOutputID != "agent-execution:"+e.ExecutionID {
			return ErrInvalidValidationOutput
		}
	case executiondomain.ToolkitCallToolCapability:
		// A tool run CAN fail as a runtime failure rather than as a tool result:
		// a lost claim, a passed deadline or an exhausted SDK budget never
		// reaches the tool. The producer's bounded wait reads this row like any
		// other terminal row, so leaving the capability out here would have left
		// such a run polling until its own timeout for a settlement that was
		// already durable but unprojectable.
		if e.LogicalOutputID != "toolkit-call-tool:"+e.ExecutionID {
			return ErrInvalidValidationOutput
		}
	default:
		return ErrInvalidValidationOutput
	}
	return nil
}

type RuntimeFailureBindingRepository interface {
	ExpectedRuntimeFailure(ctx context.Context, executionID string, generation uint64) (ExpectedRuntimeFailure, error)
}

type RuntimeFailureProjector interface {
	ProjectRuntimeFailure(ctx context.Context, projection RuntimeFailureProjection) (ProjectionOutcome, error)
}

type RuntimeFailureService struct {
	bindings  RuntimeFailureBindingRepository
	fences    FenceVerifier
	projector RuntimeFailureProjector
}

func NewRuntimeFailureService(bindings RuntimeFailureBindingRepository, fences FenceVerifier, projector RuntimeFailureProjector) (*RuntimeFailureService, error) {
	if bindings == nil || fences == nil || projector == nil {
		return nil, errors.New("runtime failure binding, fence and projector dependencies are required")
	}
	return &RuntimeFailureService{bindings: bindings, fences: fences, projector: projector}, nil
}

func (s *RuntimeFailureService) IngestFailure(ctx context.Context, frame RuntimeFailureFrame) (ProjectionOutcome, error) {
	if err := frame.Validate(); err != nil {
		return ProjectionOutcome{}, err
	}
	if err := s.fences.VerifyActive(ctx, frame.Fence); err != nil {
		return ProjectionOutcome{}, fmt.Errorf("verify runtime failure fence: %w", err)
	}
	expected, err := s.bindings.ExpectedRuntimeFailure(ctx, frame.Fence.ExecutionID, frame.Fence.Generation)
	if err != nil {
		return ProjectionOutcome{}, fmt.Errorf("load admitted runtime failure binding: %w", err)
	}
	if err := expected.Validate(); err != nil {
		return ProjectionOutcome{}, fmt.Errorf("invalid admitted runtime failure binding: %w", err)
	}
	if expected.TenantID != frame.TenantID || expected.ResourceProjectID != frame.ResourceProjectID || expected.ProjectionProjectID != frame.ProjectionProjectID || expected.CommandID != frame.Fence.CommandID || expected.ExecutionID != frame.Fence.ExecutionID || expected.Generation != frame.Fence.Generation || expected.LogicalOutputID != frame.LogicalOutputID {
		return ProjectionOutcome{}, ErrValidationOutputConflict
	}
	browserData, err := json.Marshal(struct {
		Code        string `json:"code"`
		SafeMessage string `json:"safe_message"`
		Retryable   bool   `json:"retryable"`
	}{Code: frame.Failure.Code, SafeMessage: frame.Failure.SafeMessage, Retryable: frame.Failure.Retryable})
	if err != nil {
		return ProjectionOutcome{}, err
	}
	frame.EncodedFailure = append([]byte(nil), frame.EncodedFailure...)
	frame.EncodedSettlement = append([]byte(nil), frame.EncodedSettlement...)
	outcome, err := s.projector.ProjectRuntimeFailure(ctx, RuntimeFailureProjection{
		Frame:        frame,
		BrowserData:  browserData,
		CapabilityID: expected.CapabilityID,
	})
	if err != nil {
		return ProjectionOutcome{}, fmt.Errorf("project runtime failure: %w", err)
	}
	if outcome.Cursor == 0 || outcome.CommittedSequence != frame.Sequence {
		return ProjectionOutcome{}, errors.New("runtime failure projector returned an empty durable position")
	}
	return outcome, nil
}
