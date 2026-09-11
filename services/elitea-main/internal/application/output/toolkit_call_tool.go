package output

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
)

const (
	// MaxToolkitCallToolResultBytes bounds the encoded terminal payload, and
	// MaxToolkitCallToolBodyBytes the JSON body inside it. The second is the
	// worker's own cap; a worker that exceeded it must report truncated=true
	// with NO body, because half a JSON document reads as a corrupt result.
	MaxToolkitCallToolResultBytes = 64 * 1024
	MaxToolkitCallToolBodyBytes   = 48 * 1024
)

var (
	ErrInvalidToolkitCallToolOutput   = errors.New("invalid tool-run output")
	ErrToolkitCallToolBindingMismatch = errors.New("tool-run output does not match admitted inputs")
	ErrToolkitCallToolOutputConflict  = errors.New("tool-run output conflicts with a durable output")
)

// ToolkitCallToolInputBinding identifies one exact immutable input the worker
// consumed. It is metadata only; the bytes stay on the input data plane.
type ToolkitCallToolInputBinding struct {
	EntryID          string
	ImmutableVersion string
	ContentDigest    runtimedomain.Digest
}

func (b ToolkitCallToolInputBinding) Validate() error {
	if !validIndexMetadata(b.EntryID) || b.ContentDigest.IsZero() {
		return ErrInvalidToolkitCallToolOutput
	}
	return nil
}

type ToolkitCallToolStatus string

const (
	ToolkitCallToolStatusOK                    ToolkitCallToolStatus = "ok"
	ToolkitCallToolStatusAuthorizationRequired ToolkitCallToolStatus = "authorization_required"
	ToolkitCallToolStatusToolError             ToolkitCallToolStatus = "tool_error"
	ToolkitCallToolStatusUnsupportedToolkit    ToolkitCallToolStatus = "unsupported_toolkit"
	ToolkitCallToolStatusUnknownTool           ToolkitCallToolStatus = "unknown_tool"
)

// ToolkitCallToolSummary is the bounded typed terminal result. The outer SDK
// response is deliberately not represented: it can carry redeemed configuration
// and other worker-local fields.
type ToolkitCallToolSummary struct {
	Status                ToolkitCallToolStatus
	ResultJSON            string
	Truncated             bool
	ErrorMessage          string
	AuthorizationRequired *executiondomain.ToolkitAuthorizationRequired
}

func (s ToolkitCallToolSummary) Validate() error {
	switch s.Status {
	case ToolkitCallToolStatusOK, ToolkitCallToolStatusToolError, ToolkitCallToolStatusAuthorizationRequired,
		ToolkitCallToolStatusUnsupportedToolkit, ToolkitCallToolStatusUnknownTool:
	default:
		return ErrInvalidToolkitCallToolOutput
	}
	for _, value := range []string{s.ResultJSON, s.ErrorMessage} {
		if len(value) > MaxToolkitCallToolBodyBytes || !utf8.ValidString(value) ||
			strings.ContainsRune(value, '\x00') {
			return ErrInvalidToolkitCallToolOutput
		}
	}
	if s.Status == ToolkitCallToolStatusAuthorizationRequired {
		if s.AuthorizationRequired == nil || s.AuthorizationRequired.Validate() != nil || s.ErrorMessage != executiondomain.ToolkitAuthorizationMessage || s.ResultJSON != "" || s.Truncated {
			return ErrInvalidToolkitCallToolOutput
		}
	} else if s.AuthorizationRequired != nil {
		return ErrInvalidToolkitCallToolOutput
	}
	// The truncation contract, enforced on this side too. A worker that sent a
	// body AND claimed truncation would have cut a JSON document in half.
	if s.Truncated && s.ResultJSON != "" {
		return ErrInvalidToolkitCallToolOutput
	}
	// Both refusals happen before any provider work, so neither can carry a
	// result, and each must say why.
	switch s.Status {
	case ToolkitCallToolStatusUnsupportedToolkit, ToolkitCallToolStatusUnknownTool:
		if s.ResultJSON != "" || s.Truncated || s.ErrorMessage == "" {
			return ErrInvalidToolkitCallToolOutput
		}
	case ToolkitCallToolStatusToolError:
		if s.ErrorMessage == "" {
			return ErrInvalidToolkitCallToolOutput
		}
	}
	return nil
}

// ToolkitCallToolResult binds one terminal tool result to the exact immutable
// inputs the SDK call consumed.
type ToolkitCallToolResult struct {
	ToolkitType       string
	ToolName          string
	InputBundleID     string
	InputBundleDigest runtimedomain.Digest
	Settings          ToolkitCallToolInputBinding
	Arguments         ToolkitCallToolInputBinding
	ResultSummary     ToolkitCallToolSummary
}

func (r ToolkitCallToolResult) Validate() error {
	if !validIndexMetadata(r.ToolkitType) || !validIndexMetadata(r.ToolName) ||
		!validIndexMetadata(r.InputBundleID) || r.InputBundleDigest.IsZero() {
		return ErrInvalidToolkitCallToolOutput
	}
	if err := r.Settings.Validate(); err != nil {
		return err
	}
	if err := r.Arguments.Validate(); err != nil {
		return err
	}
	if r.Settings.EntryID == r.Arguments.EntryID {
		return ErrInvalidToolkitCallToolOutput
	}
	if challenge := r.ResultSummary.AuthorizationRequired; challenge != nil && challenge.ToolkitType != r.ToolkitType {
		return ErrInvalidToolkitCallToolOutput
	}
	return r.ResultSummary.Validate()
}

type ToolkitCallToolFrame struct {
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
	Result                ToolkitCallToolResult
}

func (f ToolkitCallToolFrame) Validate() error {
	if f.StreamID == "" || f.TenantID == "" || f.ResourceProjectID == "" ||
		f.ProjectionProjectID == "" || f.WorkloadSessionID == "" || f.ProducerID == "" ||
		f.EventID == "" || f.LogicalOutputID == "" || f.Sequence == 0 || f.OccurredAt.IsZero() {
		return ErrInvalidToolkitCallToolOutput
	}
	if strings.ContainsAny(f.EventID, "\r\n") || strings.ContainsAny(f.LogicalOutputID, "\r\n") ||
		f.WorkloadSessionID != f.Fence.WorkloadSessionID || f.ProducerID != f.Fence.ProducerID {
		return ErrInvalidToolkitCallToolOutput
	}
	if err := f.Fence.Validate(); err != nil {
		return err
	}
	if !matchesCanonicalTerminalIdentity(
		f.StreamID, f.EventID, f.LogicalOutputID, f.Sequence,
		f.Settlement.ProposalID, f.Settlement.IdempotencyKey, f.Fence, "",
	) {
		return ErrInvalidToolkitCallToolOutput
	}
	if f.PayloadDigest.IsZero() || len(f.EncodedResult) == 0 ||
		len(f.EncodedResult) > MaxToolkitCallToolResultBytes ||
		runtimedomain.SHA256(f.EncodedResult) != f.PayloadDigest {
		return ErrInvalidToolkitCallToolOutput
	}
	if err := f.Result.Validate(); err != nil {
		return err
	}
	// The settlement outcome is DERIVED from the status, never taken from the
	// worker. A tool that raised is a completed run — the caller asked whether
	// the tool works and got the answer — so only the two refusals made before
	// any provider work settle as FAILED. This mirrors the worker's own choice
	// in protocol/codec.py, and disagreeing with it is how a run would settle
	// one way here and another there.
	expectedOutcome := executionapp.SettlementSucceeded
	switch f.Result.ResultSummary.Status {
	case ToolkitCallToolStatusUnsupportedToolkit, ToolkitCallToolStatusUnknownTool:
		expectedOutcome = executionapp.SettlementFailed
	}
	if err := f.Settlement.Validate(); err != nil ||
		f.Settlement.Fence != f.Fence ||
		f.Settlement.Outcome != expectedOutcome ||
		f.Settlement.TerminalLogicalOutputID != f.LogicalOutputID ||
		f.Settlement.TerminalEventID != f.EventID ||
		f.Settlement.TerminalSequence != f.Sequence ||
		f.Settlement.TerminalPayloadDigest != f.PayloadDigest ||
		len(f.EncodedSettlement) == 0 ||
		len(f.EncodedSettlement) > MaxToolkitCallToolResultBytes ||
		runtimedomain.SHA256(f.EncodedSettlement) != f.Settlement.ProposalDigest {
		return ErrInvalidToolkitCallToolOutput
	}
	return nil
}

// ExpectedToolkitCallTool is the admitted binding the frame must match. It
// needs no capability-owned table: every field is on execution_jobs,
// input_bundles and input_bundle_entries.
type ExpectedToolkitCallTool struct {
	ToolkitID           string
	ToolkitType         string
	ToolkitName         string
	ServerURL           string
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
	Settings            ToolkitCallToolInputBinding
	Arguments           ToolkitCallToolInputBinding
}

func (e ExpectedToolkitCallTool) Validate() error {
	if e.TenantID == "" || e.ResourceProjectID == "" || e.ProjectionProjectID == "" ||
		e.CapabilityID != executiondomain.ToolkitCallToolCapability ||
		e.CommandID == "" || e.ExecutionID == "" || e.Generation == 0 ||
		e.LogicalOutputID != "toolkit-call-tool:"+e.ExecutionID ||
		!validIndexMetadata(e.InputBundleID) || e.InputBundleDigest.IsZero() {
		return ErrInvalidToolkitCallToolOutput
	}
	if err := e.Settings.Validate(); err != nil {
		return err
	}
	if err := e.Arguments.Validate(); err != nil {
		return err
	}
	if e.Settings.EntryID == e.Arguments.EntryID {
		return ErrInvalidToolkitCallToolOutput
	}
	return nil
}

type ToolkitCallToolBindingRepository interface {
	ExpectedToolkitCallTool(ctx context.Context, executionID string, generation uint64) (ExpectedToolkitCallTool, error)
}

type ToolkitCallToolProjection struct {
	Frame ToolkitCallToolFrame
}

// ToolkitCallToolProjector atomically inserts the terminal output and prepares
// settlement. There is NO projection table beside it: a tool run has no chat
// transcript and no index metadata, and `agent.execute.*` has none either. The
// output inbox row IS the durable result, and the producer's bounded wait reads
// it back from there.
type ToolkitCallToolProjector interface {
	ProjectToolkitCallTool(ctx context.Context, projection ToolkitCallToolProjection) (ProjectionOutcome, error)
}

type ToolkitCallToolService struct {
	bindings  ToolkitCallToolBindingRepository
	fences    FenceVerifier
	projector ToolkitCallToolProjector
}

func NewToolkitCallToolService(
	bindings ToolkitCallToolBindingRepository,
	fences FenceVerifier,
	projector ToolkitCallToolProjector,
) (*ToolkitCallToolService, error) {
	if bindings == nil || fences == nil || projector == nil {
		return nil, errors.New("tool-run binding, fence and projector dependencies are required")
	}
	return &ToolkitCallToolService{bindings: bindings, fences: fences, projector: projector}, nil
}

func (s *ToolkitCallToolService) IngestToolkitCallTool(
	ctx context.Context,
	frame ToolkitCallToolFrame,
) (ProjectionOutcome, error) {
	if err := frame.Validate(); err != nil {
		return ProjectionOutcome{}, err
	}
	if err := s.fences.VerifyActive(ctx, frame.Fence); err != nil {
		return ProjectionOutcome{}, fmt.Errorf("verify tool-run output fence: %w", err)
	}
	expected, err := s.bindings.ExpectedToolkitCallTool(ctx, frame.Fence.ExecutionID, frame.Fence.Generation)
	if err != nil {
		return ProjectionOutcome{}, fmt.Errorf("load admitted tool-run binding: %w", err)
	}
	if err := expected.Validate(); err != nil {
		return ProjectionOutcome{}, fmt.Errorf("invalid admitted tool-run binding: %w", err)
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
		expected.Arguments != frame.Result.Arguments {
		return ProjectionOutcome{}, ErrToolkitCallToolBindingMismatch
	}

	if challenge := frame.Result.ResultSummary.AuthorizationRequired; challenge != nil {
		if challenge.ToolkitID != expected.ToolkitID || challenge.ToolkitType != expected.ToolkitType || challenge.ToolkitName != expected.ToolkitName || challenge.ServerURL != expected.ServerURL {
			return ProjectionOutcome{}, ErrToolkitCallToolBindingMismatch
		}
	}
	outcome, err := s.projector.ProjectToolkitCallTool(ctx, ToolkitCallToolProjection{
		Frame: cloneToolkitCallToolFrame(frame),
	})
	if err != nil {
		return ProjectionOutcome{}, fmt.Errorf("project tool run: %w", err)
	}
	// Cursor is NOT asserted here, unlike index and agent output. Both of those
	// append a durable browser replay event and take its identity as the
	// cursor; a tool run appends none — the caller is a bounded HTTP wait, not
	// a tab, and putting an arbitrary provider response on the replay stream
	// would give it a reader it does not have. CommittedSequence is what the
	// acknowledgement uses, and it is asserted.
	if outcome.CommittedSequence != frame.Sequence {
		return ProjectionOutcome{}, errors.New("tool-run projector returned an unexpected committed sequence")
	}
	return outcome, nil
}

func cloneToolkitCallToolFrame(frame ToolkitCallToolFrame) ToolkitCallToolFrame {
	if challenge := frame.Result.ResultSummary.AuthorizationRequired; challenge != nil {
		cloned := *challenge
		cloned.ResourceMetadata = append([]byte(nil), challenge.ResourceMetadata...)
		frame.Result.ResultSummary.AuthorizationRequired = &cloned
	}
	frame.EncodedResult = append([]byte(nil), frame.EncodedResult...)
	frame.EncodedSettlement = append([]byte(nil), frame.EncodedSettlement...)
	return frame
}
