package storage

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	app "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/httpaction"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/guardrails"
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

// httpActionStatus maps typed failures to the Worker-facing status and code.
// Unknown errors are 500 "internal" with no detail.
func httpActionStatus(err error) (int, string) {
	switch {
	case app.FailureCode(err) == app.CodeInputsTooBig:
		return http.StatusRequestEntityTooLarge, app.CodeInputsTooBig
	case errors.Is(err, app.ErrNonDeterministic):
		return http.StatusConflict, app.CodeNonDetermined
	case errors.Is(err, app.ErrInvalid):
		return http.StatusUnprocessableEntity, app.CodeInvalidInput
	case errors.Is(err, app.ErrUnauthorized):
		return http.StatusForbidden, app.CodePolicyDenied
	case errors.Is(err, app.ErrDispatchInFlight):
		return http.StatusServiceUnavailable, "dispatch_in_flight"
	case errors.Is(err, app.ErrUnavailable), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return http.StatusServiceUnavailable, "unavailable"
	default:
		return http.StatusInternalServerError, "internal"
	}
}

func writeHTTPActionError(w http.ResponseWriter, status int, failure string) {
	if status == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", "1")
	}
	w.Header().Set("Content-Type", "application/json")
	setPrivateNoCacheHeaders(w.Header())
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"code":"` + failure + `"}`))
}

func (s *ContentServer) PostHTTPAction(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	claim, err := parseExecutionClaim(r)
	if err != nil {
		writeHTTPActionError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	actions := s.runtimeHTTPActions
	if actions == nil {
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		return
	}
	if !actions.acquire(claim.ExecutionID) {
		writeHTTPActionError(w, http.StatusServiceUnavailable, "capacity")
		return
	}
	defer actions.release(claim.ExecutionID)
	data, err := readHTTPActionBody(r)
	if err != nil {
		writeHTTPActionError(w, http.StatusRequestEntityTooLarge, app.CodeInputsTooBig)
		return
	}
	var request app.Invocation
	if app.Decode(data, &request) != nil {
		writeHTTPActionError(w, http.StatusUnprocessableEntity, app.CodeInvalidInput)
		return
	}
	receipt, err := actions.Execute(r.Context(), claim, request)
	// Safe fields only: never the URL, headers, bodies, inputs or credential.
	fields := []any{slog.String("execution_id", claim.ExecutionID), slog.Uint64("generation", claim.Generation), slog.String("node_id", boundedLogID(request.NodeID)), slog.Uint64("attempt", uint64(request.Attempt)), slog.Int64("duration_ms", time.Since(started).Milliseconds())}
	if err != nil {
		status, failure := httpActionStatus(err)
		actions.logger.WarnContext(r.Context(), "http action refused", append(fields, slog.Int("status", status), slog.String("code", failure))...)
		writeHTTPActionError(w, status, failure)
		return
	}
	failure := ""
	if receipt.FailureCode != nil {
		failure = *receipt.FailureCode
	}
	actions.logger.InfoContext(r.Context(), "http action", append(fields, slog.String("state", receipt.State), slog.String("failure_code", failure), slog.String("effect_id", receipt.EffectID))...)
	body, err := json.Marshal(receipt)
	if err != nil || len(body) > 3*1024*1024 {
		writeHTTPActionError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	setPrivateNoCacheHeaders(w.Header())
	w.Header().Set("X-Content-Type-Options", "nosniff")
	digest := sha256.Sum256(body)
	w.Header().Set("Content-Digest", formatSHA256Digest(digest))
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func boundedLogID(value string) string {
	if len(value) > 128 {
		return value[:128]
	}
	return value
}

func readHTTPActionBody(r *http.Request) ([]byte, error) {
	if r.ContentLength > app.MaxInvocation {
		return nil, app.ErrInvalid
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, app.MaxInvocation+1))
	if err != nil || len(body) > app.MaxInvocation {
		return nil, app.ErrInvalid
	}
	return body, nil
}
