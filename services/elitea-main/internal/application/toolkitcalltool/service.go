package toolkitcalltool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	"google.golang.org/protobuf/proto"
)

const (
	toolRunIdempotencyPrefix = "toolkit-call-tool-v1:"

	// DefaultRunDeadline bounds one synchronous tool run.
	//
	// It is smaller than the MCP agent bound (90 s) because a tool run is ONE
	// SDK call rather than an agent loop, and larger than any HTTP client's
	// first retry so a slow provider is not reported as a failure. On expiry the
	// caller is told the execution id: the run is durable and still going, and
	// the id is what lets a person find or cancel it.
	DefaultRunDeadline = 60 * time.Second

	pollInitialInterval = 200 * time.Millisecond
	pollMaxInterval     = 2 * time.Second

	// recordTimeout bounds the analytics write. It is short: the row is a
	// statistic and the caller is holding a finished tool result.
	recordTimeout = 5 * time.Second

	// MaxToolArgumentsBytes is this boundary's bound on caller content. It is
	// the input-entry bound, applied here so an oversized body is refused where
	// the caller can see why rather than inside the bundle factory.
	MaxToolArgumentsBytes = executiondomain.MaxInputEntryContentBytes
)

var (
	ErrInvalidToolRun          = errors.New("invalid tool-run request")
	ErrToolkitNotVisible       = errors.New("toolkit is not visible in the requested project")
	ErrUnsupportedToolkitType  = errors.New("toolkit type cannot be run by this deployment")
	ErrToolRunDeadlineExceeded = errors.New("tool run did not settle within the bounded wait")
)

// RunRequest is caller-controlled invocation data plus stable server-derived
// identity. Toolkit settings and credentials are deliberately absent: the
// resolver reloads them from the saved row, which is what stops a caller
// running a tool against settings it supplied itself.
type RunRequest struct {
	RequestID                 string
	ProjectID                 int64
	ActorUserID               int64
	ToolkitID                 int64
	ToolName                  string
	Arguments                 json.RawMessage
	LLMModel                  string
	MCPAuthorizationReference string
	LLMSettings               json.RawMessage
}

func (r RunRequest) Validate() error {
	if len(r.RequestID) > 128 || !utf8.ValidString(r.RequestID) || strings.ContainsAny(r.RequestID, "\x00\r\n") || r.RequestID != strings.TrimSpace(r.RequestID) {
		return ErrInvalidToolRun
	}
	if r.ProjectID <= 0 || r.ActorUserID <= 0 || r.ToolkitID <= 0 ||
		r.ProjectID > math.MaxInt32 || r.ToolkitID > math.MaxInt32 {
		return ErrInvalidToolRun
	}
	if r.ToolName == "" || len(r.ToolName) > executiondomain.MaxSafeCommandStringBytes ||
		!utf8.ValidString(r.ToolName) || strings.ContainsAny(r.ToolName, "\x00\r\n") ||
		r.ToolName != strings.TrimSpace(r.ToolName) {
		return ErrInvalidToolRun
	}
	if len(r.LLMModel) > maxAdmissionStringBytes || !utf8.ValidString(r.LLMModel) || strings.ContainsAny(r.LLMModel, "\x00\r\n") || r.LLMModel != strings.TrimSpace(r.LLMModel) {
		return ErrInvalidToolRun
	}
	if r.MCPAuthorizationReference != "" && !validMCPReference(r.MCPAuthorizationReference) {
		return ErrInvalidToolRun
	}
	if !validModelSettings(r.LLMSettings) {
		return ErrInvalidToolRun
	}
	if len(r.Arguments) > MaxToolArgumentsBytes {
		return ErrInvalidToolRun
	}
	if len(r.Arguments) > 0 && !validBoundedJSONObject(r.Arguments) {
		return ErrInvalidToolRun
	}
	return nil
}

func (r RunRequest) Clone() RunRequest {
	r.Arguments = append(json.RawMessage(nil), r.Arguments...)
	r.LLMSettings = append(json.RawMessage(nil), r.LLMSettings...)
	return r
}

// RunStatus is the closed outcome set a caller sees. It mirrors
// ToolkitCallToolStatusV1 and adds RunStatusRuntimeFailure, which the proto has
// no member for because a runtime failure is not a tool result at all — it
// arrives on the RUNTIME_FAILURE payload instead.
type RunStatus string

const (
	RunStatusOK                    RunStatus = "ok"
	RunStatusAuthorizationRequired RunStatus = "authorization_required"
	RunStatusToolError             RunStatus = "tool_error"
	RunStatusUnsupportedToolkit    RunStatus = "unsupported_toolkit"
	RunStatusUnknownTool           RunStatus = "unknown_tool"
	RunStatusRuntimeFailure        RunStatus = "runtime_failure"
)

// RunOutcome is one settled tool run. ResultJSON is the SDK return value's
// canonical JSON encoding; it is EMPTY when Truncated is true, because half a
// JSON document reads as a corrupt result and this boundary must never hand a
// caller one.
type RunOutcome struct {
	ExecutionID           string
	Status                RunStatus
	ResultJSON            string
	Truncated             bool
	ErrorMessage          string
	AuthorizationRequired *executiondomain.ToolkitAuthorizationRequired
	ToolkitType           string
	ToolName              string
}

// AuthoritativeInputResolver reloads the saved toolkit and freezes its settings.
// It is the ONLY place project visibility is decided: a toolkit id belonging to
// another project must come back found=false, never as a run against another
// project's credentials.
type AuthoritativeInputResolver interface {
	Resolve(context.Context, RunRequest) (AuthoritativeInputs, error)
}

// ToolkitTypeVerdict answers whether this deployment can build a toolkit of the
// named type at all. The refusal happens BEFORE admission on purpose: a type no
// image can build will never become runnable, so admitting it would burn a
// bounded wait and an admission slot to reach a conclusion already known here.
// The signature is the one internal/api/v2/toolkits already answers with
// (type_catalogue.go:243), reason included, so the refusal names the type AND
// says why rather than restating "unsupported".
type ToolkitTypeVerdict interface {
	SupportsToolkitType(toolkitType string) (bool, string)
}

// Settlement is one terminal row of the durable output inbox.
type Settlement struct {
	Outcome     executionapp.SettlementOutcome
	PayloadType string
	Payload     []byte
}

// SettlementReader polls for the terminal output of one execution. found=false
// means the run has not settled yet, never that it failed.
type SettlementReader interface {
	ReadToolkitCallToolSettlement(ctx context.Context, executionID string, generation uint64) (Settlement, bool, error)
}

type admissionSubmitter interface {
	Submit(context.Context, SubmitRequest) (AdmittedRun, error)
}

// ToolRunRecord is one settled — or still-running — explicit tool run, in the
// shape the analytics TOOL dimension needs (issue 618).
//
// It is a SEPARATE type from RunOutcome because the two answer different
// questions. RunOutcome is what the caller gets back and carries the tool's
// return value; this carries the identity and the clock, and never the payload:
// a durable analytics record must not become a second copy of a tool result
// nobody deleted.
type ToolRunRecord struct {
	ProjectID   int64
	ActorUserID int64
	ToolkitID   int64
	ToolkitType string
	ToolName    string
	ExecutionID string
	StartedAt   time.Time
	// FinishedAt is the zero time when the bounded wait expired before the run
	// settled. The run is still going and the record says so, rather than
	// claiming a duration it does not have.
	FinishedAt time.Time
	IsError    bool
}

// RunRecorder is the durable per-tool-call record this service writes on its way
// out. It is OPTIONAL: a deployment composed without one runs tools exactly as
// before and reports no tool analytics, which is the state every deployment was
// in before shared migration 0119.
//
// A recorder failure never fails the run. The tool has already executed and its
// result is in hand; refusing to hand it back because a statistics row did not
// commit would turn a reporting gap into an outage. It is LOGGED rather than
// swallowed, because a producer that silently stops writing is the failure the
// analytics dimension exists to avoid.
type RunRecorder interface {
	RecordToolRun(ctx context.Context, record ToolRunRecord) error
}

// RunOption configures optional collaborators. The required five stay positional
// so a deployment cannot compose a RunService that quietly cannot run a tool.
type RunOption func(*RunService)

func WithRunRecorder(recorder RunRecorder) RunOption {
	return func(s *RunService) { s.recorder = recorder }
}

type runDispatcher interface {
	Dispatch(context.Context, Dispatch) error
}

// DispatchPolicy carries the per-deployment command-envelope values the
// dispatcher needs and admission does not. They are configuration, not caller
// input, so they live beside the service rather than on the request.
type DispatchPolicy struct {
	CapabilityVersion string
	ResourceClass     string
	IsolationClass    string
	Priority          uint32
	LimitsRevision    string
}

func (p DispatchPolicy) validate() error {
	if p.CapabilityVersion == "" || p.ResourceClass == "" || p.IsolationClass == "" ||
		p.LimitsRevision == "" || p.Priority == 0 {
		return errors.New("tool-run dispatch policy is incomplete")
	}
	return nil
}

// RunService is the application boundary: resolve, refuse, admit, dispatch,
// wait, answer.
type RunService struct {
	resolver    AuthoritativeInputResolver
	verdict     ToolkitTypeVerdict
	admissions  admissionSubmitter
	dispatcher  runDispatcher
	settlements SettlementReader
	policy      DispatchPolicy
	newID       executionapp.IDGenerator
	deadline    time.Duration
	recorder    RunRecorder
	now         func() time.Time
}

func NewRunService(
	resolver AuthoritativeInputResolver,
	verdict ToolkitTypeVerdict,
	admissions admissionSubmitter,
	dispatcher runDispatcher,
	settlements SettlementReader,
	policy DispatchPolicy,
	newID executionapp.IDGenerator,
	deadline time.Duration,
	options ...RunOption,
) (*RunService, error) {
	if resolver == nil || verdict == nil || admissions == nil ||
		dispatcher == nil || settlements == nil || newID == nil {
		return nil, errors.New("tool-run service dependencies are required")
	}
	if err := policy.validate(); err != nil {
		return nil, err
	}
	if deadline <= 0 {
		deadline = DefaultRunDeadline
	}
	service := &RunService{
		resolver:    resolver,
		verdict:     verdict,
		admissions:  admissions,
		dispatcher:  dispatcher,
		settlements: settlements,
		policy:      policy,
		newID:       newID,
		deadline:    deadline,
		now:         time.Now,
	}
	for _, option := range options {
		if option != nil {
			option(service)
		}
	}
	return service, nil
}

// PendingRun is what a caller is told when the bounded wait expires. The run is
// durable and still going; the execution id is how a person finds it.
type PendingRun struct {
	ExecutionID string
	Waited      time.Duration
}

func (e *PendingRun) Error() string {
	return fmt.Sprintf("tool run %s did not settle within %s", e.ExecutionID, e.Waited)
}

func (e *PendingRun) Unwrap() error { return ErrToolRunDeadlineExceeded }

func (s *RunService) RunTool(ctx context.Context, request RunRequest) (RunOutcome, error) {
	if s == nil || ctx == nil {
		return RunOutcome{}, ErrInvalidToolRun
	}
	if err := ctx.Err(); err != nil {
		return RunOutcome{}, err
	}
	if err := request.Validate(); err != nil {
		return RunOutcome{}, err
	}
	if len(request.Arguments) == 0 {
		request.Arguments = json.RawMessage(`{}`)
	}

	inputs, err := s.resolver.Resolve(ctx, request.Clone())
	if err != nil {
		return RunOutcome{}, err
	}
	// The unrunnable-type refusal, before ANY durable write. See
	// ToolkitTypeVerdict for why it belongs here and not after dispatch.
	if supported, reason := s.verdict.SupportsToolkitType(inputs.ToolkitType); !supported {
		if reason == "" {
			reason = "this deployment cannot build a toolkit of this type"
		}
		return RunOutcome{}, fmt.Errorf("%w: %s: %s", ErrUnsupportedToolkitType, inputs.ToolkitType, reason)
	}
	inputs.ToolName = request.ToolName
	inputs.Arguments = append(json.RawMessage(nil), request.Arguments...)

	idempotencyKey, err := s.idempotencyKey(request, inputs)
	if err != nil {
		return RunOutcome{}, err
	}
	projectID := strconv.FormatInt(request.ProjectID, 10)
	admitted, err := s.admissions.Submit(ctx, SubmitRequest{
		Identity: executionapp.AdmissionIdentity{
			// The current platform has project-local schemas but no independent
			// tenant identifier, so the authorized project is the tenant,
			// resource and projection boundary — the same choice index
			// admission and configuration validation already made.
			TenantID:            projectID,
			ResourceProjectID:   projectID,
			ProjectionProjectID: projectID,
			ActorID:             strconv.FormatInt(request.ActorUserID, 10),
		},
		IdempotencyKey: idempotencyKey,
		Inputs:         inputs,
	})
	if err != nil {
		return RunOutcome{}, err
	}

	if admitted.Outcome.Created {
		if err := s.dispatcher.Dispatch(ctx, Dispatch{
			OutboxID:              admitted.OutboxID,
			CommandID:             admitted.Outcome.CommandID,
			ExecutionID:           admitted.Outcome.ExecutionID,
			Generation:            1,
			DispatchOrdinal:       1,
			TenantID:              projectID,
			ResourceProjectID:     projectID,
			ProjectionProjectID:   projectID,
			PrincipalRef:          strconv.FormatInt(request.ActorUserID, 10),
			InputBundleID:         admitted.InputBundle.ID,
			InputBundleVersion:    admitted.InputBundle.Version,
			InputBundleMediaType:  admitted.InputBundle.MediaType,
			InputBundleByteLength: uint64(len(admitted.InputBundle.Manifest)),
			InputBundleDigest:     admitted.InputBundle.Digest,
			CapabilityID:          executiondomain.ToolkitCallToolCapability,
			CapabilityVersion:     s.policy.CapabilityVersion,
			ResourceClass:         s.policy.ResourceClass,
			IsolationClass:        s.policy.IsolationClass,
			Priority:              s.policy.Priority,
			Deadline:              admitted.Outcome.Deadline,
			LimitsRevision:        s.policy.LimitsRevision,
			ToolkitType:           admitted.Binding.ToolkitType,
			ToolName:              admitted.Binding.ToolName,
			ToolkitID:             strconv.FormatInt(admitted.Binding.ToolkitID, 10),
			ToolkitVersion:        admitted.Binding.ToolkitVersion,
			SettingsEntryID:       admitted.Binding.SettingsEntryID,
			ArgumentsEntryID:      admitted.Binding.ArgumentsEntryID,
		}); err != nil {
			return RunOutcome{}, fmt.Errorf("dispatch tool run: %w", err)
		}
	}

	outcome, err := s.await(ctx, admitted)
	s.record(ctx, request, inputs, admitted, outcome, err)
	return outcome, err
}

// record writes the durable per-tool-call row the analytics TOOL dimension is
// built on (issue 618).
//
// It runs for a SETTLED run and for one whose bounded wait expired, and for
// nothing else. Every earlier return in RunTool happens before the command is
// dispatched — an unrunnable toolkit type, a refused admission — so the tool
// never ran and there is no call to record. A wait that expired DID dispatch,
// so the call exists; it is recorded with no finish time rather than dropped,
// because dropping it would make a hanging tool disappear from the tab that
// should show it.
//
// The identity comes from the ADMISSION, not from the request: admitted.Binding
// is what the runtime was actually told to run, and an idempotent re-admission
// returns the same execution id, so a replay updates one row instead of adding
// a second.
func (s *RunService) record(
	ctx context.Context,
	request RunRequest,
	inputs AuthoritativeInputs,
	admitted AdmittedRun,
	outcome RunOutcome,
	runErr error,
) {
	if s.recorder == nil {
		return
	}
	pending := errors.Is(runErr, ErrToolRunDeadlineExceeded)
	if runErr != nil && !pending {
		return
	}

	toolkitType := admitted.Binding.ToolkitType
	if toolkitType == "" {
		toolkitType = inputs.ToolkitType
	}
	toolName := admitted.Binding.ToolName
	if toolName == "" {
		toolName = request.ToolName
	}
	record := ToolRunRecord{
		ProjectID:   request.ProjectID,
		ActorUserID: request.ActorUserID,
		ToolkitID:   admitted.Binding.ToolkitID,
		ToolkitType: toolkitType,
		ToolName:    toolName,
		ExecutionID: admitted.Outcome.ExecutionID,
		StartedAt:   admitted.Outcome.AdmittedAt,
		IsError:     outcome.Status != RunStatusOK,
	}
	if record.ToolkitID <= 0 {
		record.ToolkitID = request.ToolkitID
	}
	if record.StartedAt.IsZero() {
		record.StartedAt = s.clock()
	}
	if !pending {
		record.FinishedAt = s.clock()
		if record.FinishedAt.Before(record.StartedAt) {
			// clock_timestamp on the admitting database and this process's own
			// clock are two clocks. A finish before the start is a skew, not a
			// negative duration, so the record reports the call as instantaneous
			// rather than storing a value the CHECK constraint would refuse.
			record.FinishedAt = record.StartedAt
		}
	} else {
		record.IsError = false
	}

	// A fresh, bounded context: ctx may already be done — a pending run's
	// caller has given up waiting — and the record must still commit.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	if err := s.recorder.RecordToolRun(writeCtx, record); err != nil {
		slog.ErrorContext(writeCtx, "tool run was not recorded for analytics",
			"execution_id", record.ExecutionID, "project_id", record.ProjectID,
			"tool_name", record.ToolName, "err", err)
	}
}

func (s *RunService) clock() time.Time {
	if s.now == nil {
		return time.Now()
	}
	return s.now()
}

// await polls the durable output inbox. There is no notification channel this
// package can subscribe to without taking on the replay stream's authorization
// and cursor machinery, so it polls; the interval backs off so a slow provider
// is not a busy loop, and the first polls are quick because a tool that answers
// immediately is the common case.
func (s *RunService) await(ctx context.Context, admitted AdmittedRun) (RunOutcome, error) {
	deadline, cancel := context.WithTimeout(ctx, s.deadline)
	defer cancel()

	interval := pollInitialInterval
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		// The expiry check is a STATEMENT and not one arm of the select below.
		// Both channels are ready once the deadline has passed and select picks
		// a ready arm at random, so the select alone would report expiry only
		// about half the time.
		if deadline.Err() != nil {
			return RunOutcome{}, &PendingRun{
				ExecutionID: admitted.Outcome.ExecutionID,
				Waited:      s.deadline,
			}
		}
		select {
		case <-deadline.Done():
			continue
		case <-timer.C:
		}

		settlement, found, err := s.settlements.ReadToolkitCallToolSettlement(
			deadline, admitted.Outcome.ExecutionID, 1,
		)
		switch {
		case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
			continue
		case err != nil:
			return RunOutcome{}, fmt.Errorf("read tool-run settlement: %w", err)
		}
		if found {
			return decodeSettlement(admitted, settlement)
		}

		interval *= 2
		if interval > pollMaxInterval {
			interval = pollMaxInterval
		}
		timer.Reset(interval)
	}
}

func decodeSettlement(admitted AdmittedRun, settlement Settlement) (RunOutcome, error) {
	outcome := RunOutcome{
		ExecutionID: admitted.Outcome.ExecutionID,
		ToolkitType: admitted.Binding.ToolkitType,
		ToolName:    admitted.Binding.ToolName,
	}
	switch settlement.PayloadType {
	case PayloadTypeToolkitCallToolResult:
		var result runtimev1.ToolkitCallToolResultV1
		if err := (proto.UnmarshalOptions{DiscardUnknown: false}).
			Unmarshal(settlement.Payload, &result); err != nil {
			return RunOutcome{}, fmt.Errorf("decode tool-run result: %w", err)
		}
		summary := result.GetResultSummary()
		if summary == nil {
			// result_artifact is the only other arm and no artifact writer
			// exists in this platform, so a result with no summary is a worker
			// that changed shape without this producer being told.
			return RunOutcome{}, errors.New("tool-run result carries no bounded summary")
		}
		outcome.ResultJSON = summary.GetResultJson()
		outcome.Truncated = summary.GetTruncated()
		outcome.ErrorMessage = summary.GetErrorMessage()
		switch summary.GetStatus() {
		case runtimev1.ToolkitCallToolStatusV1_TOOLKIT_CALL_TOOL_STATUS_V1_AUTHORIZATION_REQUIRED:
			challenge := summary.GetAuthorizationRequired()
			if challenge == nil {
				return RunOutcome{}, errors.New("tool authorization challenge is absent")
			}
			value := &executiondomain.ToolkitAuthorizationRequired{ToolkitName: challenge.GetToolkitName(), ToolkitType: challenge.GetToolkitType(), ToolkitID: challenge.GetToolkitId(), ServerURL: challenge.GetServerUrl(), ResourceMetadataURL: challenge.GetResourceMetadataUrl(), ResourceMetadata: append([]byte(nil), challenge.GetResourceMetadataJson()...)}
			if value.Validate() != nil || value.ToolkitID != strconv.FormatInt(admitted.Binding.ToolkitID, 10) || value.ToolkitType != admitted.Binding.ToolkitType || summary.GetResultJson() != "" || summary.GetTruncated() || summary.GetErrorMessage() != executiondomain.ToolkitAuthorizationMessage {
				return RunOutcome{}, errors.New("tool authorization challenge does not match admitted toolkit")
			}
			outcome.Status = RunStatusAuthorizationRequired
			outcome.AuthorizationRequired = value
		case runtimev1.ToolkitCallToolStatusV1_TOOLKIT_CALL_TOOL_STATUS_V1_OK:
			outcome.Status = RunStatusOK
		case runtimev1.ToolkitCallToolStatusV1_TOOLKIT_CALL_TOOL_STATUS_V1_TOOL_ERROR:
			outcome.Status = RunStatusToolError
		case runtimev1.ToolkitCallToolStatusV1_TOOLKIT_CALL_TOOL_STATUS_V1_UNSUPPORTED_TOOLKIT:
			outcome.Status = RunStatusUnsupportedToolkit
		case runtimev1.ToolkitCallToolStatusV1_TOOLKIT_CALL_TOOL_STATUS_V1_UNKNOWN_TOOL:
			outcome.Status = RunStatusUnknownTool
		default:
			return RunOutcome{}, errors.New("tool-run result carries an unknown status")
		}
		return outcome, nil
	case PayloadTypeRuntimeFailure:
		var failure runtimev1.RuntimeErrorV1
		outcome.Status = RunStatusRuntimeFailure
		if err := (proto.UnmarshalOptions{DiscardUnknown: false}).
			Unmarshal(settlement.Payload, &failure); err != nil {
			outcome.ErrorMessage = "the runtime operation failed"
			return outcome, nil
		}
		// SafeMessage is what the runtime chose to say; it is already bounded
		// and carries no provider body, so it is the only field relayed.
		outcome.ErrorMessage = failure.GetSafeMessage()
		if outcome.ErrorMessage == "" {
			outcome.ErrorMessage = "the runtime operation failed"
		}
		return outcome, nil
	default:
		return RunOutcome{}, fmt.Errorf("tool-run settlement carries payload type %q", settlement.PayloadType)
	}
}

// PayloadTypeToolkitCallToolResult and PayloadTypeRuntimeFailure are the two
// durable payload types this capability can settle with. They are the strings
// shared migration 0115 admits on output_inbox.
const (
	PayloadTypeToolkitCallToolResult = "TOOLKIT_CALL_TOOL_RESULT"
	PayloadTypeRuntimeFailure        = "RUNTIME_FAILURE"
)

// idempotencyKey binds retries to one deliberate request. Callers without a
// request ID start a new run. Identical settings alone never identify a run.
func (s *RunService) idempotencyKey(request RunRequest, inputs AuthoritativeInputs) (string, error) {
	requestID := request.RequestID
	if requestID == "" {
		var err error
		requestID, err = s.newID()
		if err != nil {
			return "", fmt.Errorf("create tool-run request identity: %w", err)
		}
	}
	hash := sha256.New()
	for _, value := range []string{
		requestID,
		strconv.FormatInt(request.ProjectID, 10),
		strconv.FormatInt(request.ActorUserID, 10),
		strconv.FormatInt(request.ToolkitID, 10),
		inputs.ToolkitType,
		inputs.ToolkitVersion,
		request.ToolName,
		string(request.Arguments),
		string(inputs.Settings),
		string(inputs.RuntimeContext),
	} {
		var length [8]byte
		for index := 0; index < 8; index++ {
			length[7-index] = byte(len(value) >> (8 * index))
		}
		if _, err := hash.Write(length[:]); err != nil {
			return "", fmt.Errorf("derive tool-run idempotency key: %w", err)
		}
		if _, err := hash.Write([]byte(value)); err != nil {
			return "", fmt.Errorf("derive tool-run idempotency key: %w", err)
		}
	}
	return toolRunIdempotencyPrefix + hex.EncodeToString(hash.Sum(nil)), nil
}

var _ interface {
	RunTool(context.Context, RunRequest) (RunOutcome, error)
} = (*RunService)(nil)
