package httpaction

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"time"
)

// Admission contains facts from Main's live claim and the frozen start input.
// No invocation field supplies these facts.
type Admission struct {
	ExecutionID string
	Generation  uint64
	ProjectID   int64
	ActorID     int64
	ClaimID     string
	LeaseEpoch  uint64
	Deadline    time.Time
	// Template is the frozen node template the invocation's binding digest
	// selected from the start snapshot.
	Template NodeTemplate
	// Tools answers the frozen guardrail policy for the rule's toolkit/tool.
	Tools ToolPolicy
}

// ToolPolicy is the frozen toolkit guardrail policy of the execution.
type ToolPolicy interface {
	// Permits is false for a blocked toolkit or tool, and for a sensitive one
	// (call-bound HTTP approval does not exist, so sensitive fails closed).
	Permits(toolkit, tool string) bool
}

type Admitter interface {
	Admit(context.Context, Invocation) (Admission, error)
}

// Rules reads the live rule a template names inside the execution's project.
type Rules interface {
	Rule(ctx context.Context, projectID int64, name string) (Rule, error)
}

// Intent is the immutable record Begin persists before the one dispatch.
type Intent struct {
	EffectID           string
	Rule               Rule
	RequestWire        []byte
	RequestDigest      string
	CredentialRevision string
	// Retryable is the rule's class for this request (D2); Begin admits an
	// attempt > 0 only for a retryable rule after an uncertain or failed one.
	Retryable        bool
	DispatchDeadline time.Time
}

type Effects interface {
	// Lookup returns a terminal receipt of this attempt before any credential
	// or artifact redemption. A stale dispatching row (other claim, or past
	// its dispatch deadline) is converted to uncertain and returned; a live
	// one is ErrDispatchInFlight. Different inputs for the same attempt are
	// ErrNonDeterministic.
	Lookup(context.Context, Admission, Invocation) (Receipt, bool, error)
	// Begin persists the intent and returns dispatch=true exactly once per
	// (activation, attempt). In the same transaction it fences the claim and
	// re-reads the rule (revision, enabled) and the credential (actor access,
	// revision), so a concurrent disable or permission removal is ordered
	// before or after it, never interleaved.
	Begin(context.Context, Admission, Invocation, Intent) (Receipt, bool, error)
	// Commit uses the original dispatch claim. A new claim cannot overwrite.
	Commit(context.Context, Admission, Receipt) error
}

// Credential is resolved server-side from a configuration reference.
type Credential struct {
	Headers  http.Header
	Revision string
}

type Credentials interface {
	// Resolve checks the original actor may use the configuration now, reads
	// its live revision and returns the auth headers. It never reaches YAML,
	// invocations, receipts, events or logs.
	Resolve(ctx context.Context, admission Admission, configurationID int32) (Credential, error)
}

type Artifacts interface {
	Read(context.Context, Admission, Artifact, uint64) ([]byte, error)
	Write(ctx context.Context, admission Admission, bucket, effectID, contentType string, data []byte) (Artifact, error)
}

// Transport is the only dispatch path: an http.RoundTripper, which never
// follows a redirect. An *http.Client does not satisfy it, so a
// redirect-following client cannot be wired (FU-073). Production wires
// egress.NewHTTPActionGuard(...).RoundTripper(): HTTPS only, no proxy,
// DNS-rebinding-safe dial under ELITEA_HTTP_ACTION_EGRESS_ALLOWLIST.
type Transport = http.RoundTripper

// commitTimeout bounds the receipt write after a dispatch. It runs detached
// from the caller's cancellation so a Worker disconnect does not turn a known
// outcome into an uncertain one.
const commitTimeout = 5 * time.Second

type Service struct {
	admit       Admitter
	rules       Rules
	effects     Effects
	credentials Credentials
	artifacts   Artifacts
	transport   Transport
}

func New(admit Admitter, rules Rules, effects Effects, credentials Credentials, artifacts Artifacts, transport Transport) (*Service, error) {
	if admit == nil || rules == nil || effects == nil || transport == nil {
		return nil, ErrUnavailable
	}
	return &Service{admit: admit, rules: rules, effects: effects, credentials: credentials, artifacts: artifacts, transport: transport}, nil
}

// Execute admits, renders, records and dispatches one attempt at most once.
func (s *Service) Execute(ctx context.Context, inv Invocation) (Receipt, error) {
	inv, err := ParseInvocation(inv)
	if err != nil {
		return Receipt{}, err
	}
	admitted, err := s.admit.Admit(ctx, inv)
	if err != nil {
		return Receipt{}, err
	}
	if admitted.ExecutionID == "" || admitted.Generation == 0 || admitted.ProjectID <= 0 || admitted.ActorID <= 0 || admitted.Tools == nil || !admitted.Deadline.After(time.Now()) {
		return Receipt{}, ErrUnauthorized
	}
	if existing, found, err := s.effects.Lookup(ctx, admitted, inv); err != nil {
		return Receipt{}, err
	} else if found {
		return existing, nil
	}
	rule, err := s.rules.Rule(ctx, admitted.ProjectID, admitted.Template.Operation)
	if err != nil {
		return Receipt{}, err
	}
	if !admitted.Tools.Permits(rule.Toolkit, rule.Tool) {
		return Receipt{}, policyDenied()
	}
	rendered, err := Render(rule, admitted.Template, inv.Inputs)
	if err != nil {
		return Receipt{}, err
	}
	request := rendered.Request
	retryable := rule.Retryable(request)
	if inv.Attempt > 0 && !retryable {
		return Receipt{}, policyDenied()
	}
	if request.Response.Mode == "artifact" && (s.artifacts == nil || rule.OutputBucket == "") {
		return Receipt{}, policyDenied()
	}
	// Resolve bounded bytes and credentials before the effect dispatch fence.
	body := rendered.Body
	if request.Body.Kind == "artifact" {
		if s.artifacts == nil {
			return Receipt{}, policyDenied()
		}
		body, err = s.artifacts.Read(ctx, admitted, *request.Body.Reference, MaxResponse)
		if err != nil {
			return Receipt{}, ErrUnauthorized
		}
	}
	if len(body) > MaxResponse {
		return Receipt{}, invalidInput()
	}
	headers := make(http.Header, len(request.Headers)+4)
	for _, header := range request.Headers {
		headers.Set(header.Name, header.Value)
	}
	credentialRevision := ""
	if request.Credential != nil {
		if s.credentials == nil {
			return Receipt{}, ErrUnauthorized
		}
		credential, err := s.credentials.Resolve(ctx, admitted, request.Credential.ConfigurationID)
		if err != nil {
			return Receipt{}, err
		}
		defer zero(credential.Headers)
		if !ValidDigest(credential.Revision) {
			return Receipt{}, ErrUnauthorized
		}
		credentialRevision = credential.Revision
		for name, values := range credential.Headers {
			if _, exists := headers[http.CanonicalHeaderKey(name)]; exists {
				return Receipt{}, policyDenied()
			}
			headers[http.CanonicalHeaderKey(name)] = values
		}
	}
	count := 0
	for name, values := range headers {
		if len(values) != 1 {
			return Receipt{}, policyDenied()
		}
		count += len(name) + len(values[0])
	}
	if count > 16384 || len(headers) > 32 {
		return Receipt{}, invalidInput()
	}
	if request.Body.Kind == "json" {
		headers.Set("Content-Type", "application/json")
	} else if request.Body.ContentType != "" {
		headers.Set("Content-Type", request.Body.ContentType)
	}
	if request.IdempotencyKey != "" {
		headers.Set("Idempotency-Key", request.IdempotencyKey)
	}
	// The dispatch keeps running if the Worker disconnects: the effect is
	// recorded, and the Worker's retry reads the committed receipt.
	deadline := time.Now().Add(time.Duration(request.TimeoutMS) * time.Millisecond)
	if admitted.Deadline.Before(deadline) {
		deadline = admitted.Deadline
	}
	dispatchCtx, cancel := context.WithDeadline(context.WithoutCancel(ctx), deadline)
	defer cancel()
	outbound, err := http.NewRequestWithContext(dispatchCtx, request.Method, request.URL, bytes.NewReader(body))
	if err != nil {
		return Receipt{}, invalidInput()
	}
	outbound.Header = headers
	outbound.ContentLength = int64(len(body))
	intent := Intent{EffectID: EffectID(admitted.ExecutionID, admitted.Generation, inv), Rule: rule, RequestWire: rendered.Wire, RequestDigest: rendered.Digest, CredentialRevision: credentialRevision, Retryable: retryable, DispatchDeadline: deadline.Add(commitTimeout)}
	receipt, dispatch, err := s.effects.Begin(ctx, admitted, inv, intent)
	if err != nil {
		return Receipt{}, err
	}
	if !dispatch {
		return receipt, nil
	}
	// No retry or redirect occurs beyond this durable dispatch fence.
	response, callErr := s.transport.RoundTrip(outbound)
	if callErr != nil {
		receipt.State = "uncertain"
		receipt.FailureCode = code("reconciliation_required")
		return s.finish(ctx, admitted, receipt)
	}
	if response == nil || response.Body == nil || response.StatusCode < 200 || response.StatusCode > 599 {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		receipt.State = "uncertain"
		receipt.FailureCode = code("invalid_response")
		return s.finish(ctx, admitted, receipt)
	}
	if response.ContentLength > int64(request.Response.MaxBytes) {
		closeErr := response.Body.Close()
		receipt.State = "failed"
		receipt.FailureCode = code("response_too_large")
		if closeErr != nil {
			receipt.State = "uncertain"
			receipt.FailureCode = code("reconciliation_required")
		}
		return s.finish(ctx, admitted, receipt)
	}
	captured, readErr := io.ReadAll(io.LimitReader(response.Body, int64(request.Response.MaxBytes)+1))
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil {
		receipt.State = "uncertain"
		receipt.FailureCode = code("reconciliation_required")
		return s.finish(ctx, admitted, receipt)
	}
	if uint64(len(captured)) > request.Response.MaxBytes {
		receipt.State = "failed"
		receipt.FailureCode = code("response_too_large")
		return s.finish(ctx, admitted, receipt)
	}
	var descriptor *Artifact
	spill := request.Response.Mode == "artifact" || len(captured) > MaxInline
	if spill && request.Method != "HEAD" && response.StatusCode != 204 && response.StatusCode != 205 && acceptedStatus(request, uint16(response.StatusCode)) {
		if s.artifacts == nil || rule.OutputBucket == "" {
			receipt.State = "failed"
			receipt.FailureCode = code("response_too_large")
			return s.finish(ctx, admitted, receipt)
		}
		saved, saveErr := s.artifacts.Write(ctx, admitted, rule.OutputBucket, receipt.EffectID, response.Header.Get("Content-Type"), captured)
		if saveErr != nil {
			receipt.State = "uncertain"
			receipt.FailureCode = code("artifact_unavailable")
			return s.finish(ctx, admitted, receipt)
		}
		descriptor = &saved
	}
	projection, failure := Project(request, uint16(response.StatusCode), response.Header.Get("Content-Type"), captured, descriptor)
	if failure != "" {
		receipt.State = "failed"
		receipt.FailureCode = code(failure)
	} else {
		receipt.State = "completed"
		receipt.Result = projection
	}
	return s.finish(ctx, admitted, receipt)
}

func (s *Service) finish(ctx context.Context, admitted Admission, receipt Receipt) (Receipt, error) {
	// A lost lease leaves dispatching durable; the next Lookup reports it
	// uncertain. Caller cancellation does not abandon a known outcome.
	commitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), commitTimeout)
	defer cancel()
	if err := s.effects.Commit(commitCtx, admitted, receipt); err != nil {
		return Receipt{}, ErrUnavailable
	}
	return receipt, nil
}

func zero(headers http.Header) {
	for name, values := range headers {
		for index := range values {
			values[index] = ""
		}
		delete(headers, name)
	}
}

func code(value string) *string { return &value }
func Digest(data []byte) string { hash := sha256.Sum256(data); return hex.EncodeToString(hash[:]) }

func acceptedStatus(request Request, status uint16) bool {
	for _, accepted := range request.Response.AcceptedStatuses {
		if status >= accepted.First && status <= accepted.Last {
			return true
		}
	}
	return false
}
