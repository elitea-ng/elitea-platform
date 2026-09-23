package output

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

const (
	MaxToolkitAvailableToolsResultBytes   = 64 * 1024
	MaxToolkitAvailableToolsArtifactBytes = 1024 * 1024
	ToolkitAvailableToolsMediaType        = "application/vnd.elitea.toolkit-available-tools.v1+json"
	ToolkitAvailableToolsClassification   = "tenant-confidential"
)

var (
	ErrInvalidToolkitAvailableToolsOutput   = errors.New("invalid discovery output")
	ErrToolkitAvailableToolsBindingMismatch = errors.New("discovery output does not match admitted inputs")
	ErrToolkitAvailableToolsOutputConflict  = errors.New("discovery output conflicts with a durable output")
)

type ToolkitAvailableToolsInputBinding struct {
	EntryID          string
	ImmutableVersion string
	ContentDigest    runtimedomain.Digest
}

func (b ToolkitAvailableToolsInputBinding) Validate() error {
	if !validIndexMetadata(b.EntryID) || !validIndexMetadata(b.ImmutableVersion) || b.ContentDigest.IsZero() {
		return ErrInvalidToolkitAvailableToolsOutput
	}
	return nil
}

type ToolkitAvailableToolsArtifact struct {
	ArtifactID       string
	ImmutableVersion string
	MediaType        string
	ByteLength       uint64
	Digest           runtimedomain.Digest
	Classification   string
}

func (a ToolkitAvailableToolsArtifact) Validate() error {
	if !validIndexMetadata(a.ArtifactID) || !validIndexMetadata(a.ImmutableVersion) ||
		a.MediaType != ToolkitAvailableToolsMediaType || a.Classification != ToolkitAvailableToolsClassification ||
		a.ByteLength == 0 || a.ByteLength > MaxToolkitAvailableToolsArtifactBytes || a.Digest.IsZero() {
		return ErrInvalidToolkitAvailableToolsOutput
	}
	return nil
}

type ToolkitAvailableToolsResult struct {
	ToolkitType       string
	InputBundleID     string
	InputBundleDigest runtimedomain.Digest
	Settings          ToolkitAvailableToolsInputBinding
	ResultArtifact    ToolkitAvailableToolsArtifact
}

func (r ToolkitAvailableToolsResult) Validate() error {
	if !validIndexMetadata(r.ToolkitType) || !validIndexMetadata(r.InputBundleID) || r.InputBundleDigest.IsZero() {
		return ErrInvalidToolkitAvailableToolsOutput
	}
	if err := r.Settings.Validate(); err != nil {
		return err
	}
	return r.ResultArtifact.Validate()
}

type ToolkitAvailableToolsFrame struct {
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
	Result                ToolkitAvailableToolsResult
}

func (f ToolkitAvailableToolsFrame) Validate() error {
	if f.StreamID == "" || f.TenantID == "" || f.ResourceProjectID == "" ||
		f.ProjectionProjectID == "" || f.WorkloadSessionID == "" || f.ProducerID == "" ||
		f.EventID == "" || f.LogicalOutputID == "" || f.Sequence == 0 || f.OccurredAt.IsZero() {
		return ErrInvalidToolkitAvailableToolsOutput
	}
	if strings.ContainsAny(f.EventID, "\r\n") || strings.ContainsAny(f.LogicalOutputID, "\r\n") ||
		f.WorkloadSessionID != f.Fence.WorkloadSessionID || f.ProducerID != f.Fence.ProducerID {
		return ErrInvalidToolkitAvailableToolsOutput
	}
	if err := f.Fence.Validate(); err != nil {
		return err
	}
	if !matchesCanonicalTerminalIdentity(
		f.StreamID, f.EventID, f.LogicalOutputID, f.Sequence,
		f.Settlement.ProposalID, f.Settlement.IdempotencyKey, f.Fence, "",
	) {
		return ErrInvalidToolkitAvailableToolsOutput
	}
	if f.PayloadDigest.IsZero() || len(f.EncodedResult) == 0 ||
		len(f.EncodedResult) > MaxToolkitAvailableToolsResultBytes ||
		runtimedomain.SHA256(f.EncodedResult) != f.PayloadDigest {
		return ErrInvalidToolkitAvailableToolsOutput
	}
	if err := f.Result.Validate(); err != nil {
		return err
	}
	if err := f.Settlement.Validate(); err != nil ||
		f.Settlement.Fence != f.Fence ||
		f.Settlement.Outcome != executionapp.SettlementSucceeded ||
		f.Settlement.TerminalLogicalOutputID != f.LogicalOutputID ||
		f.Settlement.TerminalEventID != f.EventID ||
		f.Settlement.TerminalSequence != f.Sequence ||
		f.Settlement.TerminalPayloadDigest != f.PayloadDigest ||
		len(f.EncodedSettlement) == 0 ||
		len(f.EncodedSettlement) > MaxToolkitAvailableToolsResultBytes ||
		runtimedomain.SHA256(f.EncodedSettlement) != f.Settlement.ProposalDigest {
		return ErrInvalidToolkitAvailableToolsOutput
	}
	return nil
}

type ExpectedToolkitAvailableTools struct {
	TenantID            string
	ResourceProjectID   string
	ProjectionProjectID string
	CapabilityID        string
	CommandID           string
	ExecutionID         string
	Generation          uint64
	LogicalOutputID     string
	InputBundleID       string
	InputBundleDigest   runtimedomain.Digest
	Settings            ToolkitAvailableToolsInputBinding
	ToolkitType         string
}

func (e ExpectedToolkitAvailableTools) Validate() error {
	if e.TenantID == "" || e.ResourceProjectID == "" || e.ProjectionProjectID == "" ||
		e.CapabilityID != executiondomain.ToolkitAvailableToolsCapability ||
		e.CommandID == "" || e.ExecutionID == "" || e.Generation == 0 ||
		e.LogicalOutputID != "toolkit-available-tools:"+e.ExecutionID ||
		!validIndexMetadata(e.InputBundleID) || e.InputBundleDigest.IsZero() {
		return ErrInvalidToolkitAvailableToolsOutput
	}
	if err := e.Settings.Validate(); err != nil {
		return err
	}
	if !validIndexMetadata(e.ToolkitType) {
		return ErrInvalidToolkitAvailableToolsOutput
	}
	return nil
}

type ToolkitAvailableToolsBindingRepository interface {
	ExpectedToolkitAvailableTools(ctx context.Context, executionID string, generation uint64) (ExpectedToolkitAvailableTools, error)
}

type ToolkitAvailableToolsProjection struct {
	Frame ToolkitAvailableToolsFrame
}

type ToolkitAvailableToolsProjector interface {
	ProjectToolkitAvailableTools(ctx context.Context, projection ToolkitAvailableToolsProjection) (ProjectionOutcome, error)
}

type ToolkitAvailableToolsService struct {
	bindings  ToolkitAvailableToolsBindingRepository
	fences    FenceVerifier
	projector ToolkitAvailableToolsProjector
}

func NewToolkitAvailableToolsService(
	bindings ToolkitAvailableToolsBindingRepository,
	fences FenceVerifier,
	projector ToolkitAvailableToolsProjector,
) (*ToolkitAvailableToolsService, error) {
	if bindings == nil || fences == nil || projector == nil {
		return nil, errors.New("tool-discovery binding, fence and projector dependencies are required")
	}
	return &ToolkitAvailableToolsService{bindings: bindings, fences: fences, projector: projector}, nil
}

func (s *ToolkitAvailableToolsService) IngestToolkitAvailableTools(
	ctx context.Context,
	frame ToolkitAvailableToolsFrame,
) (ProjectionOutcome, error) {
	if err := frame.Validate(); err != nil {
		return ProjectionOutcome{}, err
	}
	if err := s.fences.VerifyActive(ctx, frame.Fence); err != nil {
		return ProjectionOutcome{}, fmt.Errorf("verify tool-discovery output fence: %w", err)
	}
	expected, err := s.bindings.ExpectedToolkitAvailableTools(ctx, frame.Fence.ExecutionID, frame.Fence.Generation)
	if err != nil {
		return ProjectionOutcome{}, fmt.Errorf("load admitted tool-discovery binding: %w", err)
	}
	if err := expected.Validate(); err != nil {
		return ProjectionOutcome{}, fmt.Errorf("invalid admitted tool-discovery binding: %w", err)
	}
	if expected.TenantID != frame.TenantID ||
		expected.ResourceProjectID != frame.ResourceProjectID ||
		expected.ProjectionProjectID != frame.ProjectionProjectID ||
		expected.CommandID != frame.Fence.CommandID ||
		expected.ExecutionID != frame.Fence.ExecutionID ||
		expected.Generation != frame.Fence.Generation ||
		expected.LogicalOutputID != frame.LogicalOutputID ||
		expected.InputBundleID != frame.Result.InputBundleID ||
		expected.InputBundleDigest != frame.Result.InputBundleDigest ||
		expected.Settings != frame.Result.Settings ||
		expected.ToolkitType != frame.Result.ToolkitType {
		return ProjectionOutcome{}, ErrToolkitAvailableToolsBindingMismatch
	}

	outcome, err := s.projector.ProjectToolkitAvailableTools(ctx, ToolkitAvailableToolsProjection{
		Frame: cloneToolkitAvailableToolsFrame(frame),
	})
	if err != nil {
		return ProjectionOutcome{}, fmt.Errorf("project tool discovery: %w", err)
	}
	if outcome.CommittedSequence != frame.Sequence {
		return ProjectionOutcome{}, errors.New("tool-discovery projector returned an unexpected committed sequence")
	}
	return outcome, nil
}

func cloneToolkitAvailableToolsFrame(frame ToolkitAvailableToolsFrame) ToolkitAvailableToolsFrame {
	frame.EncodedResult = append([]byte(nil), frame.EncodedResult...)
	frame.EncodedSettlement = append([]byte(nil), frame.EncodedSettlement...)
	return frame
}
