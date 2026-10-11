package output

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"mime"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
)

const (
	MaxIndexIngestResultBytes         = 64 * 1024
	MaxIndexIngestSummaryMessageBytes = 48 * 1024
	maxIndexOutputMetadataBytes       = 256
)

var (
	ErrInvalidIndexIngestOutput       = errors.New("invalid index ingest output")
	ErrIndexIngestBindingMismatch     = errors.New("index ingest output does not match admitted inputs")
	ErrIndexIngestArtifactMismatch    = errors.New("index ingest artifact metadata does not match durable storage")
	ErrIndexIngestArtifactUnavailable = errors.New("index ingest artifact is not durably available")
	ErrIndexIngestOutputConflict      = errors.New("index ingest output conflicts with a durable output")
)

// IndexInputBinding identifies one exact immutable input consumed by the
// worker. It carries metadata only; input bytes remain on the input data plane.
type IndexInputBinding struct {
	EntryID          string
	ImmutableVersion string
	ContentDigest    runtimedomain.Digest
}

func (b IndexInputBinding) Validate() error {
	if !validIndexMetadata(b.EntryID) || !validIndexMetadata(b.ImmutableVersion) || b.ContentDigest.IsZero() {
		return ErrInvalidIndexIngestOutput
	}
	return nil
}

type OptionalIndexInputBinding struct {
	Present bool
	Binding IndexInputBinding
}

func (b OptionalIndexInputBinding) Validate() error {
	if !b.Present {
		if b.Binding != (IndexInputBinding{}) {
			return ErrInvalidIndexIngestOutput
		}
		return nil
	}
	return b.Binding.Validate()
}

// IndexIngestBindings is deliberately closed over the six v1 input roles.
// Unknown protobuf fields are rejected at the transport boundary, so a worker
// cannot smuggle an unadmitted sixth binding through a newer message shape.
type IndexIngestBindings struct {
	ToolkitConfiguration IndexInputBinding
	ToolParameters       IndexInputBinding
	LLMModel             OptionalIndexInputBinding
	LLMConfiguration     OptionalIndexInputBinding
	MCPTokens            OptionalIndexInputBinding
	EmbeddingBinding     OptionalIndexInputBinding
}

func (b IndexIngestBindings) Validate() error {
	if err := b.ToolkitConfiguration.Validate(); err != nil {
		return err
	}
	if err := b.ToolParameters.Validate(); err != nil {
		return err
	}
	for _, optional := range []OptionalIndexInputBinding{b.LLMModel, b.LLMConfiguration, b.MCPTokens, b.EmbeddingBinding} {
		if err := optional.Validate(); err != nil {
			return err
		}
	}

	bindings := [6]IndexInputBinding{b.ToolkitConfiguration, b.ToolParameters}
	bindingCount := 2
	for _, optional := range []OptionalIndexInputBinding{b.LLMModel, b.LLMConfiguration, b.MCPTokens, b.EmbeddingBinding} {
		if optional.Present {
			bindings[bindingCount] = optional.Binding
			bindingCount++
		}
	}
	for i := 0; i < bindingCount; i++ {
		for j := i + 1; j < bindingCount; j++ {
			if bindings[i].EntryID == bindings[j].EntryID {
				return ErrInvalidIndexIngestOutput
			}
		}
	}
	return nil
}

// IndexArtifactReference is reference-only metadata. Artifact bytes, storage
// URIs and bearer credentials are intentionally absent from this type.
type IndexArtifactReference struct {
	ArtifactID       string
	ImmutableVersion string
	MediaType        string
	ByteLength       uint64
	Digest           runtimedomain.Digest
	Classification   string
}

type IndexIngestStatus string

const (
	IndexIngestStatusOK            IndexIngestStatus = "ok"
	IndexIngestStatusPartlyIndexed IndexIngestStatus = "partly_indexed"
	IndexIngestStatusError         IndexIngestStatus = "error"
)

type IndexIngestTerminalState string

const (
	IndexIngestTerminalUnspecified      IndexIngestTerminalState = ""
	IndexIngestTerminalCreated          IndexIngestTerminalState = "created"
	IndexIngestTerminalCompleted        IndexIngestTerminalState = "completed"
	IndexIngestTerminalScheduledReindex IndexIngestTerminalState = "scheduled_reindex"
	IndexIngestTerminalFailed           IndexIngestTerminalState = "failed"
	IndexIngestTerminalPartlyIndexed    IndexIngestTerminalState = "partly_indexed"
)

// IndexIngestSummary is the allowlisted terminal result plus the bounded
// current index-status callback fields needed for notification parity. The
// outer SDK result is intentionally not represented because it can contain
// redeemed configuration and other worker-local fields.
type IndexIngestSummary struct {
	Status         IndexIngestStatus
	Message        string
	TerminalState  IndexIngestTerminalState
	Indexed        uint64
	Updated        uint64
	ReindexPresent bool
	Reindex        bool

	// The typed result of the Rust indexing runtime (ADR-0030 decision 2).
	// Main applies these to the index_registry row, so no result sentence is
	// parsed. All of them are zero on the Python SDK path.
	IndexedDocuments uint64
	IndexedChunks    uint64
	FailedChunks     uint64
	// SkippedJSON is the closed skip breakdown as canonical JSON (an object of
	// reason to count, keys sorted), or "" when nothing was skipped. A string
	// and not a map keeps the summary comparable: the projection tells "no
	// summary" from "a summary" with ==.
	SkippedJSON        string
	EmbeddingModel     string
	EmbeddingDimension uint32
}

const (
	// MaxIndexSkipReasons and MaxIndexSkipReasonBytes bound the skip breakdown.
	// A reason is a closed lower snake_case token, never free text.
	MaxIndexSkipReasons     = 32
	MaxIndexSkipReasonBytes = 64
	// MaxIndexEmbeddingModelBytes and MaxIndexEmbeddingDimension bound the
	// embedding stamp. 65535 is far above any model in use (768-3072).
	MaxIndexEmbeddingModelBytes = 256
	MaxIndexEmbeddingDimension  = 65535
)

var indexSkipReasonPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// NewIndexSkipped encodes a skip breakdown as the canonical JSON the summary
// carries. An empty breakdown is "". It is the only constructor of
// SkippedJSON, so a value that reached a summary is sorted and bounded.
func NewIndexSkipped(skipped map[string]uint64) (string, error) {
	if len(skipped) == 0 {
		return "", nil
	}
	if len(skipped) > MaxIndexSkipReasons {
		return "", ErrInvalidIndexIngestOutput
	}
	for reason, count := range skipped {
		if reason == "" || len(reason) > MaxIndexSkipReasonBytes ||
			!indexSkipReasonPattern.MatchString(reason) || count > math.MaxInt64 {
			return "", ErrInvalidIndexIngestOutput
		}
	}
	// encoding/json sorts map keys, which is the canonical order.
	encoded, err := json.Marshal(skipped)
	if err != nil {
		return "", ErrInvalidIndexIngestOutput
	}
	return string(encoded), nil
}

// Skipped decodes the breakdown. A summary that passed Validate always decodes.
func (s IndexIngestSummary) Skipped() map[string]uint64 {
	if s.SkippedJSON == "" {
		return map[string]uint64{}
	}
	skipped := map[string]uint64{}
	if err := json.Unmarshal([]byte(s.SkippedJSON), &skipped); err != nil {
		return map[string]uint64{}
	}
	return skipped
}

func (s IndexIngestSummary) validateTyped() error {
	if s.IndexedDocuments > math.MaxInt64 || s.IndexedChunks > math.MaxInt64 ||
		s.FailedChunks > math.MaxInt64 {
		return ErrInvalidIndexIngestOutput
	}
	if s.SkippedJSON != "" {
		var decoded map[string]uint64
		if err := json.Unmarshal([]byte(s.SkippedJSON), &decoded); err != nil || len(decoded) == 0 {
			return ErrInvalidIndexIngestOutput
		}
		canonical, err := NewIndexSkipped(decoded)
		if err != nil || canonical != s.SkippedJSON {
			return ErrInvalidIndexIngestOutput
		}
	}
	// The stamp is a pair: a model without a dimension, or the reverse, would
	// let the registry record half an embedding space.
	if (s.EmbeddingModel == "") != (s.EmbeddingDimension == 0) {
		return ErrInvalidIndexIngestOutput
	}
	if s.EmbeddingDimension > MaxIndexEmbeddingDimension ||
		len(s.EmbeddingModel) > MaxIndexEmbeddingModelBytes ||
		!utf8.ValidString(s.EmbeddingModel) || strings.ContainsRune(s.EmbeddingModel, '\x00') {
		return ErrInvalidIndexIngestOutput
	}
	return nil
}

// HasTypedResult reports whether the summary describes a Rust run: any typed
// field set. The Python SDK path never sets one.
func (s IndexIngestSummary) HasTypedResult() bool {
	return s.IndexedDocuments != 0 || s.IndexedChunks != 0 || s.FailedChunks != 0 ||
		s.SkippedJSON != "" || s.EmbeddingModel != ""
}

func (s IndexIngestSummary) Validate() error {
	if err := s.validateTyped(); err != nil {
		return err
	}
	switch s.Status {
	case IndexIngestStatusOK, IndexIngestStatusPartlyIndexed, IndexIngestStatusError:
	default:
		return ErrInvalidIndexIngestOutput
	}
	if s.Message == "" || len(s.Message) > MaxIndexIngestSummaryMessageBytes || !utf8.ValidString(s.Message) || strings.ContainsRune(s.Message, '\x00') {
		return ErrInvalidIndexIngestOutput
	}
	if s.Indexed > math.MaxInt64 || s.Updated > math.MaxInt64 ||
		(!s.ReindexPresent && s.Reindex) {
		return ErrInvalidIndexIngestOutput
	}
	switch s.TerminalState {
	case IndexIngestTerminalUnspecified:
		if s.Indexed != 0 || s.Updated != 0 || s.ReindexPresent {
			return ErrInvalidIndexIngestOutput
		}
	case IndexIngestTerminalCreated,
		IndexIngestTerminalCompleted,
		IndexIngestTerminalScheduledReindex:
		if s.Status != IndexIngestStatusOK {
			return ErrInvalidIndexIngestOutput
		}
	case IndexIngestTerminalFailed:
		if s.Status != IndexIngestStatusError {
			return ErrInvalidIndexIngestOutput
		}
	case IndexIngestTerminalPartlyIndexed:
		if s.Status != IndexIngestStatusPartlyIndexed {
			return ErrInvalidIndexIngestOutput
		}
	default:
		return ErrInvalidIndexIngestOutput
	}
	return nil
}

func (a IndexArtifactReference) Validate() error {
	if !validIndexMetadata(a.ArtifactID) || !validIndexMetadata(a.ImmutableVersion) || !validIndexMetadata(a.MediaType) || !validIndexMetadata(a.Classification) || a.ByteLength == 0 || a.Digest.IsZero() {
		return ErrInvalidIndexIngestOutput
	}
	if _, _, err := mime.ParseMediaType(a.MediaType); err != nil {
		return ErrInvalidIndexIngestOutput
	}
	return nil
}

// IndexArtifactContract is admitted server policy for the terminal artifact.
// The size bound is supplied by the capability policy instead of being chosen
// by the worker or inferred from the claimed artifact metadata.
type IndexArtifactContract struct {
	MediaType      string
	Classification string
	MaxByteLength  uint64
}

func (c IndexArtifactContract) Validate() error {
	if !validIndexMetadata(c.MediaType) || !validIndexMetadata(c.Classification) || c.MaxByteLength == 0 {
		return ErrInvalidIndexIngestOutput
	}
	if _, _, err := mime.ParseMediaType(c.MediaType); err != nil {
		return ErrInvalidIndexIngestOutput
	}
	return nil
}

type IndexIngestResult struct {
	InputBundleID     string
	InputBundleDigest runtimedomain.Digest
	Bindings          IndexIngestBindings
	ResultArtifact    IndexArtifactReference
	ResultSummary     IndexIngestSummary
}

func (r IndexIngestResult) Validate() error {
	if !validIndexMetadata(r.InputBundleID) || r.InputBundleDigest.IsZero() {
		return ErrInvalidIndexIngestOutput
	}
	if err := r.Bindings.Validate(); err != nil {
		return err
	}
	hasArtifact := r.ResultArtifact != (IndexArtifactReference{})
	hasSummary := r.ResultSummary != (IndexIngestSummary{})
	if hasArtifact == hasSummary {
		return ErrInvalidIndexIngestOutput
	}
	if hasArtifact {
		return r.ResultArtifact.Validate()
	}
	return r.ResultSummary.Validate()
}

type IndexIngestFrame struct {
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
	Result                IndexIngestResult
}

func (f IndexIngestFrame) Validate() error {
	if f.StreamID == "" || f.TenantID == "" || f.ResourceProjectID == "" || f.ProjectionProjectID == "" || f.WorkloadSessionID == "" || f.ProducerID == "" || f.EventID == "" || f.LogicalOutputID == "" || f.Sequence == 0 || f.OccurredAt.IsZero() {
		return ErrInvalidIndexIngestOutput
	}
	if strings.ContainsAny(f.EventID, "\r\n") || strings.ContainsAny(f.LogicalOutputID, "\r\n") || f.WorkloadSessionID != f.Fence.WorkloadSessionID || f.ProducerID != f.Fence.ProducerID {
		return ErrInvalidIndexIngestOutput
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
		return ErrInvalidIndexIngestOutput
	}
	if f.PayloadDigest.IsZero() || len(f.EncodedResult) == 0 || len(f.EncodedResult) > MaxIndexIngestResultBytes || runtimedomain.SHA256(f.EncodedResult) != f.PayloadDigest {
		return ErrInvalidIndexIngestOutput
	}
	if err := f.Result.Validate(); err != nil {
		return err
	}
	expectedOutcome := executionapp.SettlementSucceeded
	if f.Result.ResultSummary.Status == IndexIngestStatusError {
		expectedOutcome = executionapp.SettlementFailed
	}
	if err := f.Settlement.Validate(); err != nil || f.Settlement.Fence != f.Fence || f.Settlement.Outcome != expectedOutcome || f.Settlement.TerminalLogicalOutputID != f.LogicalOutputID || f.Settlement.TerminalEventID != f.EventID || f.Settlement.TerminalSequence != f.Sequence || f.Settlement.TerminalPayloadDigest != f.PayloadDigest || len(f.EncodedSettlement) == 0 || len(f.EncodedSettlement) > MaxIndexIngestResultBytes || runtimedomain.SHA256(f.EncodedSettlement) != f.Settlement.ProposalDigest {
		return ErrInvalidIndexIngestOutput
	}
	return nil
}

type ExpectedIndexIngest struct {
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
	Bindings            IndexIngestBindings
	ArtifactContract    IndexArtifactContract
}

func (e ExpectedIndexIngest) Validate() error {
	if e.TenantID == "" || e.ResourceProjectID == "" || e.ProjectionProjectID == "" || e.CapabilityID != executiondomain.IndexIngestCapability || e.CommandID == "" || e.ExecutionID == "" || e.Generation == 0 || e.LogicalOutputID == "" || !validIndexMetadata(e.InputBundleID) || e.InputBundleDigest.IsZero() {
		return ErrInvalidIndexIngestOutput
	}
	if strings.ContainsAny(e.LogicalOutputID, "\r\n") {
		return ErrInvalidIndexIngestOutput
	}
	if err := e.Bindings.Validate(); err != nil {
		return err
	}
	return e.ArtifactContract.Validate()
}

type IndexIngestBindingRepository interface {
	ExpectedIndexIngest(ctx context.Context, executionID string, generation uint64) (ExpectedIndexIngest, error)
}

type ArtifactVerificationRequest struct {
	TenantID            string
	ResourceProjectID   string
	ProjectionProjectID string
	CommandID           string
	ExecutionID         string
	Generation          uint64
	Artifact            IndexArtifactReference
}

func (r ArtifactVerificationRequest) Validate() error {
	if r.TenantID == "" || r.ResourceProjectID == "" || r.ProjectionProjectID == "" || r.CommandID == "" || r.ExecutionID == "" || r.Generation == 0 {
		return ErrInvalidIndexIngestOutput
	}
	return r.Artifact.Validate()
}

// DurableIndexArtifact is authoritative storage evidence. StorageRecordID is
// an opaque durable metadata-row identity, never a data-plane URI or token.
type DurableIndexArtifact struct {
	Reference       IndexArtifactReference
	StorageRecordID string
	VerifiedAt      time.Time
}

func (a DurableIndexArtifact) Validate() error {
	if err := a.Reference.Validate(); err != nil {
		return err
	}
	if !validIndexMetadata(a.StorageRecordID) || a.VerifiedAt.IsZero() {
		return ErrIndexIngestArtifactUnavailable
	}
	return nil
}

// ArtifactVerifier must prove that an immutable artifact metadata record and
// its bytes are durably present, and that digest and length match Reference.
// A successful return is required before any output projection or settlement.
type ArtifactVerifier interface {
	VerifyDurable(ctx context.Context, request ArtifactVerificationRequest) (DurableIndexArtifact, error)
}

type IndexIngestProjection struct {
	Frame            IndexIngestFrame
	VerifiedArtifact DurableIndexArtifact
}

// IndexIngestProjector atomically inserts the terminal output, projects the
// index result and prepares settlement. Identical replay is idempotent; a
// reused terminal identity with different data returns
// ErrIndexIngestOutputConflict.
type IndexIngestProjector interface {
	ProjectIndexIngest(ctx context.Context, projection IndexIngestProjection) (ProjectionOutcome, error)
}

type IndexIngestService struct {
	bindings  IndexIngestBindingRepository
	fences    FenceVerifier
	artifacts ArtifactVerifier
	projector IndexIngestProjector
}

func NewIndexIngestService(bindings IndexIngestBindingRepository, fences FenceVerifier, artifacts ArtifactVerifier, projector IndexIngestProjector) (*IndexIngestService, error) {
	if bindings == nil || fences == nil || artifacts == nil || projector == nil {
		return nil, errors.New("index binding, fence, artifact verifier and projector dependencies are required")
	}
	return &IndexIngestService{bindings: bindings, fences: fences, artifacts: artifacts, projector: projector}, nil
}

func (s *IndexIngestService) IngestIndex(ctx context.Context, frame IndexIngestFrame) (ProjectionOutcome, error) {
	if err := frame.Validate(); err != nil {
		return ProjectionOutcome{}, err
	}
	if err := s.fences.VerifyActive(ctx, frame.Fence); err != nil {
		return ProjectionOutcome{}, fmt.Errorf("verify index output fence: %w", err)
	}

	expected, err := s.bindings.ExpectedIndexIngest(ctx, frame.Fence.ExecutionID, frame.Fence.Generation)
	if err != nil {
		return ProjectionOutcome{}, fmt.Errorf("load admitted index binding: %w", err)
	}
	if err := expected.Validate(); err != nil {
		return ProjectionOutcome{}, fmt.Errorf("invalid admitted index binding: %w", err)
	}
	if !matchesExpectedIndexIdentity(expected, frame) || expected.InputBundleID != frame.Result.InputBundleID || expected.InputBundleDigest != frame.Result.InputBundleDigest || expected.Bindings != frame.Result.Bindings {
		return ProjectionOutcome{}, ErrIndexIngestBindingMismatch
	}

	verified := DurableIndexArtifact{}
	if frame.Result.ResultArtifact != (IndexArtifactReference{}) {
		if expected.ArtifactContract.MediaType != frame.Result.ResultArtifact.MediaType || expected.ArtifactContract.Classification != frame.Result.ResultArtifact.Classification || frame.Result.ResultArtifact.ByteLength > expected.ArtifactContract.MaxByteLength {
			return ProjectionOutcome{}, ErrIndexIngestArtifactMismatch
		}

		verificationRequest := ArtifactVerificationRequest{
			TenantID:            expected.TenantID,
			ResourceProjectID:   expected.ResourceProjectID,
			ProjectionProjectID: expected.ProjectionProjectID,
			CommandID:           expected.CommandID,
			ExecutionID:         expected.ExecutionID,
			Generation:          expected.Generation,
			Artifact:            frame.Result.ResultArtifact,
		}
		if err := verificationRequest.Validate(); err != nil {
			return ProjectionOutcome{}, err
		}
		verified, err = s.artifacts.VerifyDurable(ctx, verificationRequest)
		if err != nil {
			return ProjectionOutcome{}, fmt.Errorf("verify durable index artifact: %w", err)
		}
		if err := verified.Validate(); err != nil {
			return ProjectionOutcome{}, err
		}
		if verified.Reference != frame.Result.ResultArtifact {
			return ProjectionOutcome{}, ErrIndexIngestArtifactMismatch
		}
	}

	projection := IndexIngestProjection{Frame: cloneIndexIngestFrame(frame), VerifiedArtifact: verified}
	outcome, err := s.projector.ProjectIndexIngest(ctx, projection)
	if err != nil {
		return ProjectionOutcome{}, fmt.Errorf("project index ingest: %w", err)
	}
	if outcome.Cursor == 0 || outcome.CommittedSequence != frame.Sequence {
		return ProjectionOutcome{}, errors.New("index projector returned an empty durable position")
	}
	return outcome, nil
}

func matchesExpectedIndexIdentity(expected ExpectedIndexIngest, frame IndexIngestFrame) bool {
	return expected.TenantID == frame.TenantID &&
		expected.ResourceProjectID == frame.ResourceProjectID &&
		expected.ProjectionProjectID == frame.ProjectionProjectID &&
		expected.CommandID == frame.Fence.CommandID &&
		expected.ExecutionID == frame.Fence.ExecutionID &&
		expected.Generation == frame.Fence.Generation &&
		expected.LogicalOutputID == frame.LogicalOutputID
}

func cloneIndexIngestFrame(frame IndexIngestFrame) IndexIngestFrame {
	frame.EncodedResult = append([]byte(nil), frame.EncodedResult...)
	frame.EncodedSettlement = append([]byte(nil), frame.EncodedSettlement...)
	return frame
}

func validIndexMetadata(value string) bool {
	if value == "" || len(value) > maxIndexOutputMetadataBytes || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}
