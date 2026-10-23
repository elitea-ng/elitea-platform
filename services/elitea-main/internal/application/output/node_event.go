package output

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
)

const MaxNodeEventOutputBytes = 64 * 1024

var (
	ErrInvalidNodeEventOutput  = errors.New("invalid node event output")
	ErrNodeEventOutputConflict = errors.New("node event output conflicts with durable replay")
)

// NodeEventFrame is one non-terminal, browser-compatible progress event. The
// protobuf bytes remain the worker-to-Main digest contract; BrowserData is the
// separately validated current NodeEvent JSON projected for SSE replay.
type NodeEventFrame struct {
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
	EncodedEvent          []byte
	BrowserData           json.RawMessage
}

func (f NodeEventFrame) Validate() error {
	if f.StreamID == "" || f.TenantID == "" || f.ResourceProjectID == "" || f.ProjectionProjectID == "" || f.WorkloadSessionID == "" || f.ProducerID == "" || f.EventID == "" || f.LogicalOutputID == "" || f.Sequence == 0 || f.OccurredAt.IsZero() {
		return ErrInvalidNodeEventOutput
	}
	if strings.ContainsAny(f.EventID, "\r\n\x00") || strings.ContainsAny(f.LogicalOutputID, "\r\n\x00") || f.WorkloadSessionID != f.Fence.WorkloadSessionID || f.ProducerID != f.Fence.ProducerID || f.ClaimHandoffWatermark >= f.Sequence {
		return ErrInvalidNodeEventOutput
	}
	if err := f.Fence.Validate(); err != nil {
		return err
	}
	if f.StreamID != f.Fence.ExecutionID+":"+strconv.FormatUint(f.Fence.Generation, 10) ||
		f.EventID != canonicalOutputEventID(f.Fence.CommandID, f.Sequence) ||
		f.LogicalOutputID != NodeEventLogicalOutputID(f.Fence.ExecutionID, f.Sequence) {
		return ErrInvalidNodeEventOutput
	}
	if f.PayloadDigest.IsZero() || len(f.EncodedEvent) == 0 || len(f.EncodedEvent) > MaxNodeEventOutputBytes || runtimedomain.SHA256(f.EncodedEvent) != f.PayloadDigest {
		return ErrInvalidNodeEventOutput
	}
	if len(f.BrowserData) == 0 || len(f.BrowserData) > MaxNodeEventOutputBytes || !json.Valid(f.BrowserData) {
		return ErrInvalidNodeEventOutput
	}
	return nil
}

func canonicalOutputEventID(commandID string, sequence uint64) string {
	return commandID + ":" + strconv.FormatUint(sequence, 10)
}

func NodeEventLogicalOutputID(executionID string, sequence uint64) string {
	return "node-event:" + executionID + ":" + strconv.FormatUint(sequence, 10)
}

type NodeEventProjector interface {
	// ProjectNodeEvent must verify the exact live claim and execution binding in
	// the same transaction that appends the replay event. Identical replay is
	// idempotent; a reused canonical sequence with different data conflicts.
	ProjectNodeEvent(ctx context.Context, frame NodeEventFrame) (ProjectionOutcome, error)
}

type NodeEventService struct {
	fences    FenceVerifier
	projector NodeEventProjector
}

func NewNodeEventService(fences FenceVerifier, projector NodeEventProjector) (*NodeEventService, error) {
	if fences == nil || projector == nil {
		return nil, errors.New("node event fence and projector dependencies are required")
	}
	return &NodeEventService{fences: fences, projector: projector}, nil
}

func (s *NodeEventService) IngestNodeEvent(ctx context.Context, frame NodeEventFrame) (ProjectionOutcome, error) {
	if err := frame.Validate(); err != nil {
		return ProjectionOutcome{}, err
	}
	if err := s.fences.VerifyActive(ctx, frame.Fence); err != nil {
		return ProjectionOutcome{}, fmt.Errorf("verify node event fence: %w", err)
	}
	frame.EncodedEvent = append([]byte(nil), frame.EncodedEvent...)
	frame.BrowserData = append(json.RawMessage(nil), frame.BrowserData...)
	outcome, err := s.projector.ProjectNodeEvent(ctx, frame)
	if err != nil {
		return ProjectionOutcome{}, fmt.Errorf("project node event: %w", err)
	}
	if outcome.Cursor == 0 || outcome.CommittedSequence != frame.Sequence {
		return ProjectionOutcome{}, errors.New("node event projector returned an empty durable position")
	}
	return outcome, nil
}
