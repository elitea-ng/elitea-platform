package output

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	configurationdomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/configurations"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
)

const MaxConfigurationValidationResultBytes = 64 * 1024

const DeadlineExceededSafeMessage = "The execution deadline was exceeded."

var (
	ErrInvalidValidationOutput  = errors.New("invalid configuration validation output")
	ErrValidationOutputConflict = errors.New("configuration validation output conflicts with a durable output")
	// ErrOutputCancelled means the exact live output fence remained valid, but
	// cancellation won the durable terminal-output linearization before this
	// non-cancellation frame. It is distinct from a stale fence: only this error
	// authorizes a worker to replace its exact local frame with cancellation.
	ErrOutputCancelled = errors.New("execution cancellation won terminal output linearization")
	// ErrOutputDeadlineExceeded means the exact live output fence remained
	// valid, but the database deadline won before this first non-deadline frame
	// became durable. Only this bound result authorizes replacement with the
	// canonical deadline failure.
	ErrOutputDeadlineExceeded = errors.New("execution deadline won terminal output linearization")
)

type ExpectedValidation struct {
	TenantID            string
	ResourceProjectID   string
	ProjectionProjectID string
	CommandID           string
	ExecutionID         string
	Generation          uint64
	Binding             configurationdomain.ValidationBinding
}

func (e ExpectedValidation) Validate() error {
	if e.TenantID == "" || e.ResourceProjectID == "" || e.ProjectionProjectID == "" || e.CommandID == "" || e.ExecutionID == "" || e.Generation == 0 {
		return ErrInvalidValidationOutput
	}
	return e.Binding.Validate()
}

type ConfigurationValidationFrame struct {
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
	Result                configurationdomain.ValidationResult
}

func (f ConfigurationValidationFrame) Validate() error {
	if f.StreamID == "" || f.TenantID == "" || f.ResourceProjectID == "" || f.ProjectionProjectID == "" || f.WorkloadSessionID == "" || f.ProducerID == "" || f.EventID == "" || f.LogicalOutputID == "" || f.Sequence == 0 || f.OccurredAt.IsZero() {
		return ErrInvalidValidationOutput
	}
	if strings.ContainsAny(f.EventID, "\r\n") || strings.ContainsAny(f.LogicalOutputID, "\r\n") {
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
		f.Result.Binding.Command.ConfigurationRevisionID,
	) {
		return ErrInvalidValidationOutput
	}
	if f.PayloadDigest.IsZero() || len(f.EncodedResult) == 0 || len(f.EncodedResult) > MaxConfigurationValidationResultBytes || runtimedomain.SHA256(f.EncodedResult) != f.PayloadDigest {
		return ErrInvalidValidationOutput
	}
	if err := f.Settlement.Validate(); err != nil || f.Settlement.Fence != f.Fence || f.Settlement.Outcome != executionapp.SettlementSucceeded || f.Settlement.TerminalLogicalOutputID != f.LogicalOutputID || f.Settlement.TerminalEventID != f.EventID || f.Settlement.TerminalSequence != f.Sequence || f.Settlement.TerminalPayloadDigest != f.PayloadDigest || len(f.EncodedSettlement) == 0 || len(f.EncodedSettlement) > MaxConfigurationValidationResultBytes || runtimedomain.SHA256(f.EncodedSettlement) != f.Settlement.ProposalDigest {
		return ErrInvalidValidationOutput
	}
	return f.Result.Validate()
}

func matchesCanonicalTerminalIdentity(streamID, eventID, logicalOutputID string, sequence uint64, proposalID, settlementKey string, fence runtimedomain.Fence, configurationRevisionID string) bool {
	if sequence == 0 ||
		streamID != fence.ExecutionID+":"+strconv.FormatUint(fence.Generation, 10) ||
		eventID != canonicalOutputEventID(fence.CommandID, sequence) ||
		proposalID != fence.CommandID+":settlement" ||
		settlementKey != fence.CommandID+":prepare-settlement" {
		return false
	}
	return configurationRevisionID == "" || logicalOutputID == "configuration-validation:"+configurationRevisionID
}

type ValidationBindingRepository interface {
	ExpectedValidation(ctx context.Context, executionID string, generation uint64) (ExpectedValidation, error)
}

type FenceVerifier interface {
	VerifyActive(ctx context.Context, fence runtimedomain.Fence) error
}

type ValidationProjection struct {
	Frame       ConfigurationValidationFrame
	BrowserData json.RawMessage
}

type ProjectionOutcome struct {
	Inserted          bool
	Cursor            uint64
	CommittedSequence uint64
}

// ConfigurationValidationProjector atomically inserts the output inbox row,
// enforces event/logical-output idempotency, updates the validation view and
// appends the durable browser event. Identical replay returns Inserted=false;
// a reused identity with different payload/fence returns
// ErrValidationOutputConflict.
type ConfigurationValidationProjector interface {
	ProjectConfigurationValidation(ctx context.Context, projection ValidationProjection) (ProjectionOutcome, error)
}

type ConfigurationValidationService struct {
	bindings  ValidationBindingRepository
	fences    FenceVerifier
	projector ConfigurationValidationProjector
}

func NewConfigurationValidationService(bindings ValidationBindingRepository, fences FenceVerifier, projector ConfigurationValidationProjector) (*ConfigurationValidationService, error) {
	if bindings == nil || fences == nil || projector == nil {
		return nil, errors.New("validation binding, fence and projector dependencies are required")
	}
	return &ConfigurationValidationService{bindings: bindings, fences: fences, projector: projector}, nil
}

func (s *ConfigurationValidationService) Ingest(ctx context.Context, frame ConfigurationValidationFrame) (ProjectionOutcome, error) {
	if err := frame.Validate(); err != nil {
		return ProjectionOutcome{}, err
	}
	if frame.WorkloadSessionID != frame.Fence.WorkloadSessionID || frame.ProducerID != frame.Fence.ProducerID {
		return ProjectionOutcome{}, runtimedomain.ErrStaleFence
	}
	if err := s.fences.VerifyActive(ctx, frame.Fence); err != nil {
		return ProjectionOutcome{}, fmt.Errorf("verify validation output fence: %w", err)
	}

	expected, err := s.bindings.ExpectedValidation(ctx, frame.Fence.ExecutionID, frame.Fence.Generation)
	if err != nil {
		return ProjectionOutcome{}, fmt.Errorf("load admitted validation binding: %w", err)
	}
	if err := expected.Validate(); err != nil {
		return ProjectionOutcome{}, fmt.Errorf("invalid admitted validation binding: %w", err)
	}
	if !matchesExpected(expected, frame) {
		return ProjectionOutcome{}, configurationdomain.ErrValidationBindingMismatch
	}

	browserData, err := safeBrowserProjection(frame.Result)
	if err != nil {
		return ProjectionOutcome{}, fmt.Errorf("encode safe validation projection: %w", err)
	}
	outcome, err := s.projector.ProjectConfigurationValidation(ctx, ValidationProjection{
		Frame:       cloneFrame(frame),
		BrowserData: browserData,
	})
	if err != nil {
		return ProjectionOutcome{}, fmt.Errorf("project configuration validation: %w", err)
	}
	if outcome.Cursor == 0 || outcome.CommittedSequence != frame.Sequence {
		return ProjectionOutcome{}, errors.New("validation projector returned an empty durable position")
	}
	return outcome, nil
}

func matchesExpected(expected ExpectedValidation, frame ConfigurationValidationFrame) bool {
	return expected.TenantID == frame.TenantID &&
		expected.ResourceProjectID == frame.ResourceProjectID &&
		expected.ProjectionProjectID == frame.ProjectionProjectID &&
		expected.CommandID == frame.Fence.CommandID &&
		expected.ExecutionID == frame.Fence.ExecutionID &&
		expected.Generation == frame.Fence.Generation &&
		expected.Binding == frame.Result.Binding
}

func cloneFrame(frame ConfigurationValidationFrame) ConfigurationValidationFrame {
	frame.EncodedResult = append([]byte(nil), frame.EncodedResult...)
	frame.EncodedSettlement = append([]byte(nil), frame.EncodedSettlement...)
	frame.Result.Issues = append([]configurationdomain.ValidationIssue(nil), frame.Result.Issues...)
	return frame
}

func safeBrowserProjection(result configurationdomain.ValidationResult) (json.RawMessage, error) {
	issues := make([]map[string]string, len(result.Issues))
	for i, issue := range result.Issues {
		issues[i] = map[string]string{
			"code":         issue.Code,
			"json_pointer": issue.JSONPointer,
			"safe_message": issue.SafeMessage,
		}
	}
	data, err := json.Marshal(struct {
		ConfigurationRevisionID string              `json:"configuration_revision_id"`
		Valid                   bool                `json:"valid"`
		Issues                  []map[string]string `json:"issues"`
	}{
		ConfigurationRevisionID: result.Binding.Command.ConfigurationRevisionID,
		Valid:                   result.Valid,
		Issues:                  issues,
	})
	if err != nil {
		return nil, err
	}
	return data, nil
}
