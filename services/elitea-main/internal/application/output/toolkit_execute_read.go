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

const (
	MaxToolkitExecuteReadResultBytes = 64 * 1024
	MaxToolkitExecuteReadJSONBytes   = 48 * 1024
)

var (
	ErrInvalidToolkitExecuteReadOutput   = errors.New("invalid direct toolkit execution output")
	ErrToolkitExecuteReadBindingMismatch = errors.New("direct toolkit output does not match admitted input")
	ErrToolkitExecuteReadOutputConflict  = errors.New("direct toolkit output conflicts with durable output")
)

type ToolkitExecuteReadResult struct {
	InputBundleID           string
	InputBundleDigest       runtimedomain.Digest
	RequestEntryID          string
	RequestImmutableVersion string
	RequestContentDigest    runtimedomain.Digest
	ResultJSON              json.RawMessage
	ToolkitType             string
	ToolkitName             string
	ToolName                string
}

func (r ToolkitExecuteReadResult) Validate() error {
	if !validIndexMetadata(r.InputBundleID) || r.InputBundleDigest.IsZero() ||
		!validIndexMetadata(r.RequestEntryID) || !validIndexMetadata(r.RequestImmutableVersion) ||
		r.RequestContentDigest.IsZero() || len(r.ResultJSON) == 0 ||
		len(r.ResultJSON) > MaxToolkitExecuteReadJSONBytes || !json.Valid(r.ResultJSON) ||
		!validIndexMetadata(r.ToolkitType) || !validIndexMetadata(r.ToolkitName) ||
		!validIndexMetadata(r.ToolName) {
		return ErrInvalidToolkitExecuteReadOutput
	}
	return nil
}

type ToolkitExecuteReadFrame struct {
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
	EncodedResult         []byte
	Settlement            executionapp.SettlementProposal
	EncodedSettlement     []byte
	Result                ToolkitExecuteReadResult
}

func (f ToolkitExecuteReadFrame) Validate() error {
	if f.StreamID == "" || f.TenantID == "" || f.ResourceProjectID == "" ||
		f.ProjectionProjectID == "" || f.WorkloadSessionID == "" || f.ProducerID == "" ||
		f.EventID == "" || f.LogicalOutputID == "" || f.Sequence == 0 || f.OccurredAt.IsZero() ||
		f.ClaimHandoffWatermark >= f.Sequence || strings.ContainsAny(f.EventID, "\x00\r\n") ||
		strings.ContainsAny(f.LogicalOutputID, "\x00\r\n") ||
		f.WorkloadSessionID != f.Fence.WorkloadSessionID || f.ProducerID != f.Fence.ProducerID {
		return ErrInvalidToolkitExecuteReadOutput
	}
	if err := f.Fence.Validate(); err != nil {
		return err
	}
	if !matchesCanonicalTerminalIdentity(
		f.StreamID, f.EventID, f.LogicalOutputID, f.Sequence,
		f.Settlement.ProposalID, f.Settlement.IdempotencyKey, f.Fence, "",
	) || f.LogicalOutputID != "toolkit-execute-read:"+f.Fence.ExecutionID {
		return ErrInvalidToolkitExecuteReadOutput
	}
	if f.PayloadDigest.IsZero() || len(f.EncodedResult) == 0 ||
		len(f.EncodedResult) > MaxToolkitExecuteReadResultBytes ||
		runtimedomain.SHA256(f.EncodedResult) != f.PayloadDigest || f.Result.Validate() != nil {
		return ErrInvalidToolkitExecuteReadOutput
	}
	if err := f.Settlement.Validate(); err != nil || f.Settlement.Fence != f.Fence ||
		f.Settlement.Outcome != executionapp.SettlementSucceeded ||
		f.Settlement.TerminalLogicalOutputID != f.LogicalOutputID ||
		f.Settlement.TerminalEventID != f.EventID || f.Settlement.TerminalSequence != f.Sequence ||
		f.Settlement.TerminalPayloadDigest != f.PayloadDigest || len(f.EncodedSettlement) == 0 ||
		len(f.EncodedSettlement) > MaxToolkitExecuteReadResultBytes ||
		runtimedomain.SHA256(f.EncodedSettlement) != f.Settlement.ProposalDigest {
		return ErrInvalidToolkitExecuteReadOutput
	}
	return nil
}

type ExpectedToolkitExecuteRead struct {
	TenantID                string
	ResourceProjectID       string
	ProjectionProjectID     string
	CapabilityID            string
	CommandID               string
	ExecutionID             string
	Generation              uint64
	LogicalOutputID         string
	InputBundleID           string
	InputBundleDigest       runtimedomain.Digest
	RequestEntryID          string
	RequestImmutableVersion string
	RequestContentDigest    runtimedomain.Digest
}

func (e ExpectedToolkitExecuteRead) Validate() error {
	if e.TenantID == "" || e.ResourceProjectID == "" || e.ProjectionProjectID == "" ||
		e.CapabilityID != executiondomain.ToolkitExecuteReadCapability || e.CommandID == "" ||
		e.ExecutionID == "" || e.Generation == 0 ||
		e.LogicalOutputID != "toolkit-execute-read:"+e.ExecutionID ||
		!validIndexMetadata(e.InputBundleID) || e.InputBundleDigest.IsZero() ||
		!validIndexMetadata(e.RequestEntryID) || !validIndexMetadata(e.RequestImmutableVersion) ||
		e.RequestContentDigest.IsZero() {
		return ErrInvalidToolkitExecuteReadOutput
	}
	return nil
}

type ToolkitExecuteReadBindingRepository interface {
	ExpectedToolkitExecuteRead(context.Context, string, uint64) (ExpectedToolkitExecuteRead, error)
}

type ToolkitExecuteReadProjection struct {
	Frame    ToolkitExecuteReadFrame
	Expected ExpectedToolkitExecuteRead
}

type ToolkitExecuteReadProjector interface {
	ProjectToolkitExecuteRead(context.Context, ToolkitExecuteReadProjection) (ProjectionOutcome, error)
}

type ToolkitExecuteReadService struct {
	bindings  ToolkitExecuteReadBindingRepository
	fences    FenceVerifier
	projector ToolkitExecuteReadProjector
}

func NewToolkitExecuteReadService(
	bindings ToolkitExecuteReadBindingRepository,
	fences FenceVerifier,
	projector ToolkitExecuteReadProjector,
) (*ToolkitExecuteReadService, error) {
	if bindings == nil || fences == nil || projector == nil {
		return nil, errors.New("direct toolkit binding, fence and projector dependencies are required")
	}
	return &ToolkitExecuteReadService{bindings: bindings, fences: fences, projector: projector}, nil
}

func (s *ToolkitExecuteReadService) IngestToolkitExecuteRead(
	ctx context.Context,
	frame ToolkitExecuteReadFrame,
) (ProjectionOutcome, error) {
	if err := frame.Validate(); err != nil {
		return ProjectionOutcome{}, err
	}
	if err := s.fences.VerifyActive(ctx, frame.Fence); err != nil {
		return ProjectionOutcome{}, fmt.Errorf("verify direct toolkit output fence: %w", err)
	}
	expected, err := s.bindings.ExpectedToolkitExecuteRead(ctx, frame.Fence.ExecutionID, frame.Fence.Generation)
	if err != nil {
		return ProjectionOutcome{}, fmt.Errorf("load admitted direct toolkit binding: %w", err)
	}
	if err := expected.Validate(); err != nil {
		return ProjectionOutcome{}, fmt.Errorf("invalid admitted direct toolkit binding: %w", err)
	}
	result := frame.Result
	if expected.TenantID != frame.TenantID || expected.ResourceProjectID != frame.ResourceProjectID ||
		expected.ProjectionProjectID != frame.ProjectionProjectID || expected.CommandID != frame.Fence.CommandID ||
		expected.ExecutionID != frame.Fence.ExecutionID || expected.Generation != frame.Fence.Generation ||
		expected.LogicalOutputID != frame.LogicalOutputID || expected.InputBundleID != result.InputBundleID ||
		expected.InputBundleDigest != result.InputBundleDigest || expected.RequestEntryID != result.RequestEntryID ||
		expected.RequestImmutableVersion != result.RequestImmutableVersion ||
		expected.RequestContentDigest != result.RequestContentDigest {
		return ProjectionOutcome{}, ErrToolkitExecuteReadBindingMismatch
	}
	projection := ToolkitExecuteReadProjection{Frame: cloneToolkitExecuteReadFrame(frame), Expected: expected}
	outcome, err := s.projector.ProjectToolkitExecuteRead(ctx, projection)
	if err != nil {
		return ProjectionOutcome{}, fmt.Errorf("project direct toolkit output: %w", err)
	}
	if outcome.Cursor == 0 || outcome.CommittedSequence != frame.Sequence {
		return ProjectionOutcome{}, errors.New("direct toolkit projector returned an empty durable position")
	}
	return outcome, nil
}

func cloneToolkitExecuteReadFrame(frame ToolkitExecuteReadFrame) ToolkitExecuteReadFrame {
	frame.EncodedResult = append([]byte(nil), frame.EncodedResult...)
	frame.EncodedSettlement = append([]byte(nil), frame.EncodedSettlement...)
	frame.Result.ResultJSON = append(json.RawMessage(nil), frame.Result.ResultJSON...)
	return frame
}
