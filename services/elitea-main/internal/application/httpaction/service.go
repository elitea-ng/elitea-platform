package httpaction

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"time"
)

// Admission contains facts from Main's live claim and immutable input.
// No request field supplies these facts.
type Admission struct {
	ExecutionID        string
	Generation         uint64
	ProjectID          int64
	ActorID            int64
	ClaimID            string
	LeaseEpoch         uint64
	Deadline           time.Time
	CredentialRevision string
	PolicyDigest       string
	OutputBucket       string
}
type Admitter interface {
	Admit(context.Context, Invocation, Request) (Admission, error)
}
type Effects interface {
	// Lookup reuses a receipt before artifact or credential redemption.
	Lookup(context.Context, Admission, Invocation, string) (Receipt, bool, error)
	// Begin persists immutable intent and changes one new effect to dispatching.
	// Existing effects return their receipt and never permit another dispatch.
	Begin(context.Context, Admission, Invocation, string) (Receipt, bool, error)
	// Commit uses the original dispatch token. A new worker cannot overwrite it.
	Commit(context.Context, Admission, Receipt) error
}
type Credentials interface {
	Headers(context.Context, Admission, CredentialReference) (http.Header, error)
}
type Artifacts interface {
	Read(context.Context, Admission, Artifact, uint64) ([]byte, error)
	Write(context.Context, Admission, string, string, []byte) (Artifact, error)
}
type Transport interface {
	Do(*http.Request) (*http.Response, error)
}
type Service struct {
	admit       Admitter
	effects     Effects
	credentials Credentials
	artifacts   Artifacts
	transport   Transport
}

func New(admit Admitter, effects Effects, credentials Credentials, artifacts Artifacts, transport Transport) (*Service, error) {
	if admit == nil || effects == nil || transport == nil {
		return nil, ErrUnavailable
	}
	return &Service{admit: admit, effects: effects, credentials: credentials, artifacts: artifacts, transport: transport}, nil
}
func (s *Service) Execute(ctx context.Context, inv Invocation) (Receipt, error) {
	wire, err := inv.RequestBytes()
	if err != nil {
		return Receipt{}, err
	}
	inv.Request = wire
	request, err := Parse(inv)
	if err != nil {
		return Receipt{}, err
	}
	admitted, err := s.admit.Admit(ctx, inv, request)
	if err != nil {
		return Receipt{}, err
	}
	if admitted.ExecutionID == "" || admitted.Generation == 0 || admitted.ProjectID <= 0 || admitted.ActorID <= 0 || !admitted.Deadline.After(time.Now()) {
		return Receipt{}, ErrUnauthorized
	}
	deadline := time.Now().Add(time.Duration(request.TimeoutMS) * time.Millisecond)
	if admitted.Deadline.Before(deadline) {
		deadline = admitted.Deadline
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	if existing, found, lookupErr := s.effects.Lookup(ctx, admitted, inv, EffectID(admitted.ExecutionID, admitted.Generation, inv)); lookupErr != nil {
		return Receipt{}, lookupErr
	} else if found {
		return existing, nil
	}
	// Resolve bounded bytes and credentials before the effect dispatch fence.
	var body []byte
	switch request.Body.Kind {
	case "json":
		body = append([]byte(nil), request.Body.Value...)
	case "text":
		var text string
		json.Unmarshal(request.Body.Value, &text)
		body = []byte(text)
	case "artifact":
		if s.artifacts == nil {
			return Receipt{}, ErrUnauthorized
		}
		body, err = s.artifacts.Read(ctx, admitted, *request.Body.Reference, MaxResponse)
		if err != nil {
			return Receipt{}, ErrUnauthorized
		}
	}
	if len(body) > MaxResponse {
		return Receipt{}, ErrInvalid
	}
	headers := make(http.Header)
	for _, header := range request.Headers {
		headers.Set(header.Name, header.Value)
	}
	if request.Credential != nil {
		if s.credentials == nil || !ValidDigest(admitted.CredentialRevision) {
			return Receipt{}, ErrUnauthorized
		}
		secret, secretErr := s.credentials.Headers(ctx, admitted, *request.Credential)
		if secretErr != nil {
			return Receipt{}, ErrUnauthorized
		}
		defer func() {
			for name, values := range secret {
				for index := range values {
					values[index] = ""
				}
				delete(secret, name)
			}
		}()
		for name, values := range secret {
			if _, exists := headers[http.CanonicalHeaderKey(name)]; exists {
				return Receipt{}, ErrUnauthorized
			}
			headers[name] = values
		}
	}
	count := 0
	for name, values := range headers {
		if len(values) != 1 {
			return Receipt{}, ErrUnauthorized
		}
		count += len(name) + len(values[0])
	}
	if count > 16384 || len(headers) > 32 {
		return Receipt{}, ErrUnauthorized
	}
	if request.Body.Kind == "json" {
		headers.Set("Content-Type", "application/json")
	} else if request.Body.ContentType != "" {
		headers.Set("Content-Type", request.Body.ContentType)
	}
	if request.IdempotencyKey != "" {
		headers.Set("Idempotency-Key", request.IdempotencyKey)
	}
	outbound, err := http.NewRequestWithContext(ctx, request.Method, request.URL, bytes.NewReader(body))
	if err != nil {
		return Receipt{}, ErrInvalid
	}
	outbound.Header = headers
	receipt, dispatch, err := s.effects.Begin(ctx, admitted, inv, EffectID(admitted.ExecutionID, admitted.Generation, inv))
	if err != nil {
		return Receipt{}, err
	}
	if !dispatch {
		return receipt, nil
	}
	// No retry or redirect occurs beyond this durable dispatch fence.
	response, callErr := s.transport.Do(outbound)
	if callErr != nil {
		receipt.State = "uncertain"
		receipt.FailureCode = code("reconciliation_required")
		return s.finish(ctx, admitted, receipt)
	}
	if response == nil || response.Body == nil || response.StatusCode < 200 || response.StatusCode > 599 {
		receipt.State = "uncertain"
		receipt.FailureCode = code("invalid_response")
		return s.finish(ctx, admitted, receipt)
	}
	if response.ContentLength > int64(request.Response.MaxBytes) {
		response.Body.Close()
		receipt.State = "uncertain"
		receipt.FailureCode = code("resource_exhausted")
		return s.finish(ctx, admitted, receipt)
	}
	captured, readErr := io.ReadAll(io.LimitReader(response.Body, int64(request.Response.MaxBytes)+1))
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil || uint64(len(captured)) > request.Response.MaxBytes {
		receipt.State = "uncertain"
		receipt.FailureCode = code("reconciliation_required")
		return s.finish(ctx, admitted, receipt)
	}
	var descriptor *Artifact
	if request.Response.Mode == "artifact" && request.Method != "HEAD" && response.StatusCode != 204 && response.StatusCode != 205 && acceptedStatus(request, uint16(response.StatusCode)) {
		if s.artifacts == nil || admitted.OutputBucket == "" {
			receipt.State = "uncertain"
			receipt.FailureCode = code("artifact_unavailable")
			return s.finish(ctx, admitted, receipt)
		}
		saved, saveErr := s.artifacts.Write(ctx, admitted, receipt.EffectID, response.Header.Get("Content-Type"), captured)
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
	// Cancellation can leave dispatching durable. Recovery then reports uncertainty.
	if err := s.effects.Commit(ctx, admitted, receipt); err != nil {
		return Receipt{}, ErrUnavailable
	}
	return receipt, nil
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
