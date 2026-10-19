package storage

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	app "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/httpaction"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/guardrails"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
)

// HTTP action pool (T-5D §3.4, decision D5): a dedicated pool so HTTP calls
// (up to 30 s each) never hold the shared content slots that token, version
// and artifact reads use. Per Main replica, in process.
const (
	// MaxHTTPActionSlots bounds concurrent HTTP actions on one Main replica.
	MaxHTTPActionSlots = 32
	// MaxHTTPActionsPerExecution bounds one execution's concurrent actions;
	// the 9th gets 503 + Retry-After, which the Worker retries.
	MaxHTTPActionsPerExecution = 8
)

type HTTPActionInput struct {
	Payload             []byte
	ProjectID           int64
	ActorID             int64
	TenantID            string
	ProjectionProjectID int64
	ClaimID             string
	LeaseEpoch          uint64
	Deadline            time.Time
}
type HTTPActionSource interface {
	ReadHTTPActionInput(context.Context, ContentClaim) (HTTPActionInput, error)
}

// RuntimeHTTPActionService serves runtime-context/http-actions. The route is
// live wherever this service is composed; its Worker caller arrives in 5d-2.
type RuntimeHTTPActionService struct {
	source      HTTPActionSource
	rules       app.Rules
	effects     app.Effects
	credentials app.Credentials
	artifacts   app.Artifacts
	transport   app.Transport
	logger      *slog.Logger

	slots       chan struct{}
	mu          sync.Mutex
	inExecution map[string]int
}

// NewRuntimeHTTPActionService wires the action path. transport must be the
// egress guard's RoundTripper (see NewHTTPActionTransport).
func NewRuntimeHTTPActionService(source HTTPActionSource, rules app.Rules, effects app.Effects, credentials app.Credentials, artifacts app.Artifacts, transport app.Transport, logger *slog.Logger) (*RuntimeHTTPActionService, error) {
	if source == nil || rules == nil || effects == nil || credentials == nil || transport == nil {
		return nil, app.ErrUnavailable
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &RuntimeHTTPActionService{source: source, rules: rules, effects: effects, credentials: credentials, artifacts: artifacts, transport: transport, logger: logger, slots: make(chan struct{}, MaxHTTPActionSlots), inExecution: make(map[string]int, MaxHTTPActionSlots)}, nil
}

func (s *RuntimeHTTPActionService) Execute(ctx context.Context, claim ContentClaim, inv app.Invocation) (app.Receipt, error) {
	runner, err := app.New(httpAdmitter{source: s.source, claim: claim}, s.rules, s.effects, s.credentials, s.artifacts, s.transport)
	if err != nil {
		return app.Receipt{}, err
	}
	return runner.Execute(ctx, inv)
}

// acquire takes one global and one per-execution slot without blocking.
func (s *RuntimeHTTPActionService) acquire(executionID string) bool {
	select {
	case s.slots <- struct{}{}:
	default:
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inExecution[executionID] >= MaxHTTPActionsPerExecution {
		<-s.slots
		return false
	}
	s.inExecution[executionID]++
	return true
}

func (s *RuntimeHTTPActionService) release(executionID string) {
	s.mu.Lock()
	if s.inExecution[executionID] <= 1 {
		delete(s.inExecution, executionID)
	} else {
		s.inExecution[executionID]--
	}
	s.mu.Unlock()
	<-s.slots
}

type httpAdmitter struct {
	source HTTPActionSource
	claim  ContentClaim
}

// Admit reads the live claim and the frozen start input. The project, actor
// and template come only from Main's records, never from the invocation.
func (a httpAdmitter) Admit(ctx context.Context, inv app.Invocation) (app.Admission, error) {
	input, err := a.source.ReadHTTPActionInput(ctx, a.claim)
	if err != nil {
		return app.Admission{}, app.ErrUnauthorized
	}
	var execution runtimev1.AgentExecutionInputV1
	if len(input.Payload) > 8*1024*1024 || proto.Unmarshal(input.Payload, &execution) != nil || len(execution.ToolkitGuardrails) == 0 {
		return app.Admission{}, app.ErrUnauthorized
	}
	var application struct {
		ID             int64 `json:"id"`
		VersionID      int64 `json:"version_id"`
		VersionDetails struct {
			Instructions string             `json:"instructions"`
			Snapshot     app.FrozenSnapshot `json:"http_action_snapshot"`
		} `json:"version_details"`
	}
	if json.Unmarshal(execution.Application, &application) != nil {
		return app.Admission{}, app.ErrUnauthorized
	}
	template, err := application.VersionDetails.Snapshot.Verify(application.ID, application.VersionID, application.VersionDetails.Instructions, inv)
	if err != nil {
		return app.Admission{}, app.ErrUnauthorized
	}
	// Root graph only; fan-out children carry a scope_ref (5d-3), refused earlier.
	if inv.ThreadID != app.RootGraphThread(input.TenantID, input.ProjectID, input.ProjectionProjectID, execution.GetThreadId()) {
		return app.Admission{}, app.ErrUnauthorized
	}
	var frozen guardrails.RuntimePolicy
	if json.Unmarshal(execution.ToolkitGuardrails, &frozen) != nil {
		return app.Admission{}, app.ErrUnauthorized
	}
	policy := guardrails.NewPolicy(guardrails.PolicyInput{BlockedToolkits: frozen.BlockedToolkits, BlockedTools: frozen.BlockedTools, SensitiveTools: frozen.SensitiveTools})
	return app.Admission{ExecutionID: a.claim.ExecutionID, Generation: a.claim.Generation, ProjectID: input.ProjectID, ActorID: input.ActorID, ClaimID: input.ClaimID, LeaseEpoch: input.LeaseEpoch, Deadline: input.Deadline, Template: template, Tools: toolPolicy{policy}}, nil
}

type toolPolicy struct{ policy guardrails.Policy }

// Permits refuses blocked toolkits and tools. No alternate sensitive or
// delegated decision is invented by the HTTP path: until call-bound HTTP
// approval exists, a sensitive rule fails closed.
func (p toolPolicy) Permits(toolkit, tool string) bool {
	if p.policy.ToolkitBlocked(toolkit) || p.policy.ToolBlocked(toolkit, tool) {
		return false
	}
	_, sensitive := p.policy.SensitiveMatch(tool, toolkit)
	return !sensitive
}

func (s *ContentServer) WithRuntimeHTTPActions(actions *RuntimeHTTPActionService) *ContentServer {
	if s != nil {
		s.runtimeHTTPActions = actions
	}
	return s
}

// httpActionStatus maps typed failures to the Worker-facing gRPC status code
// and log code. Unknown errors are codes.Internal with no detail.
func httpActionStatus(err error) (codes.Code, string) {
	switch {
	case app.FailureCode(err) == app.CodeInputsTooBig:
		return codes.ResourceExhausted, app.CodeInputsTooBig
	case errors.Is(err, app.ErrNonDeterministic):
		return codes.Aborted, app.CodeNonDetermined
	case errors.Is(err, app.ErrInvalid):
		return codes.InvalidArgument, app.CodeInvalidInput
	case errors.Is(err, app.ErrUnauthorized):
		return codes.PermissionDenied, app.CodePolicyDenied
	case errors.Is(err, app.ErrDispatchInFlight):
		return codes.Unavailable, "dispatch_in_flight"
	case errors.Is(err, app.ErrUnavailable), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return codes.Unavailable, "unavailable"
	default:
		return codes.Internal, "internal"
	}
}

// ExecuteHTTPAction runs one claim-scoped HTTP action invocation using the
// dedicated HTTP action pool (D5) so long outbound calls never starve content
// or token reads on the shared pool.
func (s *ContentServer) ExecuteHTTPAction(ctx context.Context, request *runtimev1.ExecuteHTTPActionRequestV1) (*runtimev1.ExecuteHTTPActionResponseV1, error) {
	started := time.Now()
	actions := s.runtimeHTTPActions
	if actions == nil {
		return nil, contentStatus(codes.Unimplemented)
	}
	claim, err := parseExecutionClaim(ctx, request.GetScope())
	if err != nil {
		return nil, contentStatus(codes.Unauthenticated)
	}
	if !actions.acquire(claim.ExecutionID) {
		return nil, contentStatus(codes.Unavailable)
	}
	defer actions.release(claim.ExecutionID)
	data := request.GetInvocationJson()
	if len(data) > app.MaxInvocation {
		return nil, contentStatus(codes.ResourceExhausted)
	}
	var invocation app.Invocation
	if app.Decode(data, &invocation) != nil {
		return nil, contentStatus(codes.InvalidArgument)
	}
	receipt, err := actions.Execute(ctx, claim, invocation)
	// Safe fields only: never the URL, headers, bodies, inputs or credential.
	fields := []any{
		slog.String("execution_id", claim.ExecutionID),
		slog.Uint64("generation", claim.Generation),
		slog.String("node_id", boundedLogID(invocation.NodeID)),
		slog.Uint64("attempt", uint64(invocation.Attempt)),
		slog.Int64("duration_ms", time.Since(started).Milliseconds()),
	}
	if err != nil {
		code, failure := httpActionStatus(err)
		actions.logger.WarnContext(ctx, "http action refused", append(fields, slog.String("grpc_code", code.String()), slog.String("code", failure))...)
		return nil, contentStatus(code)
	}
	failure := ""
	if receipt.FailureCode != nil {
		failure = *receipt.FailureCode
	}
	actions.logger.InfoContext(ctx, "http action", append(fields, slog.String("state", receipt.State), slog.String("failure_code", failure), slog.String("effect_id", receipt.EffectID))...)
	body, err := json.Marshal(receipt)
	if err != nil || len(body) > 3*1024*1024 {
		return nil, contentStatus(codes.Unavailable)
	}
	return &runtimev1.ExecuteHTTPActionResponseV1{ReceiptJson: body}, nil
}

func boundedLogID(value string) string {
	if len(value) > 128 {
		return value[:128]
	}
	return value
}
