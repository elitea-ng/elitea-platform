package output

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
)

const (
	MaxAgentExecutionResultBytes = 64 * 1024
	AgentResultMediaType         = "application/vnd.elitea.agent-execution-result.v1+json"
	AgentResultClassification    = "tenant-confidential"

	// MaxAgentExecutionAttachmentContentBytes is the aggregate ceiling on one
	// terminal frame's attachment write-backs (#607). It restates the cap the
	// WORKER already applies before it builds the frame --
	// MAX_ATTACHMENT_CONTENT_WRITEBACK_BYTES in agents/attachments.py, cited by
	// AgentExecutionResultV1.attachment_contents in
	// libs/proto/elitea/runtime/v1/agent.proto -- on this side of the seam,
	// because a worker's promise is not a bound: the frame arrives over gRPC
	// from a process elitea-main does not control, and this is the only place
	// that can refuse an over-size list before it reaches a transaction.
	//
	// 32 KiB, not the 64 KiB MaxAgentExecutionResultBytes the whole encoded
	// result is held to. The proto records the measurement: the terminal
	// AgentExecutionResultV1 is ~1 KiB today and the frame ceiling is 64 KiB,
	// so half the frame is a list the worker chose to send and half stays
	// headroom for the identity, digest and settlement fields that make the
	// frame projectable at all. A write-back that crowded those out would turn
	// a large attachment into a rejected TURN, which is precisely the trade the
	// proto rejected when it chose this carrier over result_artifact.
	MaxAgentExecutionAttachmentContentBytes = 32 * 1024
)

var (
	ErrInvalidAgentExecutionOutput   = errors.New("invalid agent execution output")
	ErrAgentExecutionBindingMismatch = errors.New("agent execution output does not match admitted input")
	ErrAgentExecutionResultMismatch  = errors.New("agent execution result does not match durable full message")
	ErrAgentExecutionOutputConflict  = errors.New("agent execution output conflicts with durable output")
)

type AgentExecutionArtifactReference struct {
	ArtifactID       string
	ImmutableVersion string
	MediaType        string
	ByteLength       uint64
	Digest           runtimedomain.Digest
	Classification   string
}

func (a AgentExecutionArtifactReference) Validate() error {
	if !validIndexMetadata(a.ArtifactID) || !validIndexMetadata(a.ImmutableVersion) ||
		a.MediaType != AgentResultMediaType || a.Classification != AgentResultClassification ||
		a.ByteLength == 0 || a.ByteLength > MaxNodeEventOutputBytes || a.Digest.IsZero() {
		return ErrInvalidAgentExecutionOutput
	}
	return nil
}

type AgentExecutionTerminalState string

const (
	AgentExecutionTerminalCompleted           AgentExecutionTerminalState = "completed"
	AgentExecutionTerminalPausedHITL          AgentExecutionTerminalState = "paused_hitl"
	AgentExecutionTerminalPausedAuthorization AgentExecutionTerminalState = "paused_authorization"
)

func (s AgentExecutionTerminalState) Validate() error {
	switch s {
	case AgentExecutionTerminalCompleted, AgentExecutionTerminalPausedHITL,
		AgentExecutionTerminalPausedAuthorization:
		return nil
	default:
		return ErrInvalidAgentExecutionOutput
	}
}

// AgentExecutionAttachmentContent is ONE `chat_messages_attachment` row's
// enriched content, as the worker reported it (#607).
//
// ItemID is the `chat_message_items.uuid` the admission scaffold stamped into
// the chunk's `elitea_attachment` marker
// (application/agentexecution/attachments.go, attachmentContentScaffold), NOT
// the (bucket, name) pair. The proto records why: the same file attached twice
// in one conversation is ordinary and produces two rows with identical bucket
// and name, so a (bucket, name) match would write one file's text onto the
// other's row.
//
// Content is the COMPLETE value the column is to hold, not a delta: the
// scaffold's own header chunk with its `elitea_attachment` marker intact --
// that marker is what stops a later turn re-reading the file -- followed by the
// extracted text as a second `{"type":"text"}` chunk. Storing the whole array
// rather than appending here is what keeps this write idempotent: a redelivered
// terminal frame rewrites the same bytes instead of appending the text twice.
type AgentExecutionAttachmentContent struct {
	ItemID  string
	Content json.RawMessage
}

// Validate is the per-entry half of the rule; the list-level half (aggregate
// size, duplicate ids) lives in AcceptedAgentExecutionAttachmentContents,
// which is the only thing that should ever build this slice from a wire frame.
func (a AgentExecutionAttachmentContent) Validate() error {
	if !validAttachmentItemID(a.ItemID) || !validAttachmentContentChunks(a.Content) ||
		len(a.Content) > MaxAgentExecutionAttachmentContentBytes {
		return ErrInvalidAgentExecutionOutput
	}
	return nil
}

type AgentExecutionResult struct {
	InputBundleID           string
	InputBundleDigest       runtimedomain.Digest
	RequestEntryID          string
	RequestImmutableVersion string
	RequestContentDigest    runtimedomain.Digest
	TerminalState           AgentExecutionTerminalState
	ResultArtifact          AgentExecutionArtifactReference
	// AttachmentContents is empty on every ordinary turn (#607).
	AttachmentContents []AgentExecutionAttachmentContent
}

func (r AgentExecutionResult) Validate() error {
	if !validIndexMetadata(r.InputBundleID) || r.InputBundleDigest.IsZero() ||
		!validIndexMetadata(r.RequestEntryID) || !validIndexMetadata(r.RequestImmutableVersion) ||
		r.RequestContentDigest.IsZero() || r.TerminalState.Validate() != nil {
		return ErrInvalidAgentExecutionOutput
	}
	if err := validateAgentExecutionAttachmentContents(r.AttachmentContents); err != nil {
		return err
	}
	return r.ResultArtifact.Validate()
}

type AgentExecutionFrame struct {
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
	Result                AgentExecutionResult
}

func (f AgentExecutionFrame) Validate() error {
	if f.StreamID == "" || f.TenantID == "" || f.ResourceProjectID == "" ||
		f.ProjectionProjectID == "" || f.WorkloadSessionID == "" || f.ProducerID == "" ||
		f.EventID == "" || f.LogicalOutputID == "" || f.Sequence == 0 || f.OccurredAt.IsZero() ||
		f.ClaimHandoffWatermark >= f.Sequence {
		return ErrInvalidAgentExecutionOutput
	}
	if strings.ContainsAny(f.EventID, "\x00\r\n") || strings.ContainsAny(f.LogicalOutputID, "\x00\r\n") ||
		f.WorkloadSessionID != f.Fence.WorkloadSessionID || f.ProducerID != f.Fence.ProducerID {
		return ErrInvalidAgentExecutionOutput
	}
	if err := f.Fence.Validate(); err != nil {
		return err
	}
	if !matchesCanonicalTerminalIdentity(
		f.StreamID, f.EventID, f.LogicalOutputID, f.Sequence,
		f.Settlement.ProposalID, f.Settlement.IdempotencyKey, f.Fence, "",
	) || f.LogicalOutputID != "agent-execution:"+f.Fence.ExecutionID {
		return ErrInvalidAgentExecutionOutput
	}
	if f.PayloadDigest.IsZero() || len(f.EncodedResult) == 0 ||
		len(f.EncodedResult) > MaxAgentExecutionResultBytes ||
		runtimedomain.SHA256(f.EncodedResult) != f.PayloadDigest {
		return ErrInvalidAgentExecutionOutput
	}
	if err := f.Result.Validate(); err != nil {
		return err
	}
	if err := f.Settlement.Validate(); err != nil || f.Settlement.Fence != f.Fence ||
		f.Settlement.Outcome != executionapp.SettlementSucceeded ||
		f.Settlement.TerminalLogicalOutputID != f.LogicalOutputID ||
		f.Settlement.TerminalEventID != f.EventID || f.Settlement.TerminalSequence != f.Sequence ||
		f.Settlement.TerminalPayloadDigest != f.PayloadDigest || len(f.EncodedSettlement) == 0 ||
		len(f.EncodedSettlement) > MaxAgentExecutionResultBytes ||
		runtimedomain.SHA256(f.EncodedSettlement) != f.Settlement.ProposalDigest {
		return ErrInvalidAgentExecutionOutput
	}
	return nil
}

type ExpectedAgentExecution struct {
	TenantID                  string
	ResourceProjectID         string
	ProjectionProjectID       string
	CapabilityID              string
	CommandID                 string
	ExecutionID               string
	Generation                uint64
	LogicalOutputID           string
	InputBundleID             string
	InputBundleDigest         runtimedomain.Digest
	RequestEntryID            string
	RequestImmutableVersion   string
	RequestContentDigest      runtimedomain.Digest
	ClientStreamID            string
	ClientMessageID           string
	ClientExecutionGeneration string
	SIOEvent                  string
}

func (e ExpectedAgentExecution) Validate() error {
	if e.TenantID == "" || e.ResourceProjectID == "" || e.ProjectionProjectID == "" ||
		(e.CapabilityID != executiondomain.AgentApplicationCapability && e.CapabilityID != executiondomain.AgentAdhocCapability) ||
		e.CommandID == "" || e.ExecutionID == "" || e.Generation == 0 ||
		e.LogicalOutputID != "agent-execution:"+e.ExecutionID || !validIndexMetadata(e.InputBundleID) ||
		e.InputBundleDigest.IsZero() || !validIndexMetadata(e.RequestEntryID) ||
		!validIndexMetadata(e.RequestImmutableVersion) || e.RequestContentDigest.IsZero() ||
		!validIndexMetadata(e.ClientStreamID) || !validIndexMetadata(e.ClientMessageID) ||
		!validIndexMetadata(e.ClientExecutionGeneration) ||
		(e.SIOEvent != "chat_predict" && e.SIOEvent != "chat_continue_predict") {
		return ErrInvalidAgentExecutionOutput
	}
	return nil
}

type AgentExecutionBindingRepository interface {
	ExpectedAgentExecution(context.Context, string, uint64) (ExpectedAgentExecution, error)
}

type AgentExecutionProjection struct {
	Frame    AgentExecutionFrame
	Expected ExpectedAgentExecution
}

type AgentExecutionProjector interface {
	ProjectAgentExecution(context.Context, AgentExecutionProjection) (ProjectionOutcome, error)
}

type AgentExecutionService struct {
	bindings  AgentExecutionBindingRepository
	fences    FenceVerifier
	projector AgentExecutionProjector
}

func NewAgentExecutionService(bindings AgentExecutionBindingRepository, fences FenceVerifier, projector AgentExecutionProjector) (*AgentExecutionService, error) {
	if bindings == nil || fences == nil || projector == nil {
		return nil, errors.New("agent binding, fence and projector dependencies are required")
	}
	return &AgentExecutionService{bindings: bindings, fences: fences, projector: projector}, nil
}

func (s *AgentExecutionService) IngestAgent(ctx context.Context, frame AgentExecutionFrame) (ProjectionOutcome, error) {
	if err := frame.Validate(); err != nil {
		return ProjectionOutcome{}, err
	}
	if err := s.fences.VerifyActive(ctx, frame.Fence); err != nil {
		return ProjectionOutcome{}, fmt.Errorf("verify agent output fence: %w", err)
	}
	expected, err := s.bindings.ExpectedAgentExecution(ctx, frame.Fence.ExecutionID, frame.Fence.Generation)
	if err != nil {
		return ProjectionOutcome{}, fmt.Errorf("load admitted agent binding: %w", err)
	}
	if err := expected.Validate(); err != nil {
		return ProjectionOutcome{}, fmt.Errorf("invalid admitted agent binding: %w", err)
	}
	result := frame.Result
	if expected.TenantID != frame.TenantID || expected.ResourceProjectID != frame.ResourceProjectID ||
		expected.ProjectionProjectID != frame.ProjectionProjectID || expected.CommandID != frame.Fence.CommandID ||
		expected.ExecutionID != frame.Fence.ExecutionID || expected.Generation != frame.Fence.Generation ||
		expected.LogicalOutputID != frame.LogicalOutputID || expected.InputBundleID != result.InputBundleID ||
		expected.InputBundleDigest != result.InputBundleDigest || expected.RequestEntryID != result.RequestEntryID ||
		expected.RequestImmutableVersion != result.RequestImmutableVersion ||
		expected.RequestContentDigest != result.RequestContentDigest {
		return ProjectionOutcome{}, ErrAgentExecutionBindingMismatch
	}
	projection := AgentExecutionProjection{Frame: cloneAgentExecutionFrame(frame), Expected: expected}
	outcome, err := s.projector.ProjectAgentExecution(ctx, projection)
	if err != nil {
		return ProjectionOutcome{}, fmt.Errorf("project agent execution: %w", err)
	}
	if outcome.Cursor == 0 || outcome.CommittedSequence != frame.Sequence {
		return ProjectionOutcome{}, errors.New("agent projector returned an empty durable position")
	}
	return outcome, nil
}

func cloneAgentExecutionFrame(frame AgentExecutionFrame) AgentExecutionFrame {
	frame.EncodedResult = append([]byte(nil), frame.EncodedResult...)
	frame.EncodedSettlement = append([]byte(nil), frame.EncodedSettlement...)
	return frame
}
