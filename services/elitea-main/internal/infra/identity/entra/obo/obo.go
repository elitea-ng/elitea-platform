// Package obo exchanges a held Entra user assertion for a resource token
// (ADR-0039 decisions 3 and 4, issue #1117).
//
// Two exchangers implement Exchanger:
//
//   - ClassicOBO: the Microsoft identity platform on-behalf-of flow. The
//     assertion `aud` is the middle-tier application.
//   - AgentOBO: the Entra Agent ID on-behalf-of flow. The assertion `aud` is
//     the agent identity blueprint, and the agent's client assertion (T1)
//     comes from an injected AgentAssertionSource (#1152).
//
// Both exchange on demand; caching and refresh are #1121. Neither logs. An
// error names a typed reason and never the assertion, the issued token, a
// client credential or the provider's response body.
//
// Errors are mapped from the OAuth `error`, `suberror` and `error_codes`
// fields of the token endpoint response, never from description text.
package obo

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/google/uuid"
)

// Assertion is a validated user assertion taken out of delegation custody.
// Only the delegation store builds one from a stored, verified token.
type Assertion struct {
	Token     string
	TenantID  string
	ObjectID  string
	Audience  string
	ExpiresAt time.Time
}

// String keeps the token out of %v and %s.
func (a Assertion) String() string {
	return fmt.Sprintf("obo.Assertion{tid=%s oid=%s aud=%s token=[redacted]}", a.TenantID, a.ObjectID, a.Audience)
}

// GoString keeps the token out of %#v.
func (a Assertion) GoString() string { return a.String() }

// LogValue keeps the token out of slog.
func (a Assertion) LogValue() slog.Value {
	return slog.GroupValue(slog.String("tid", a.TenantID), slog.String("oid", a.ObjectID), slog.String("aud", a.Audience))
}

// ExchangeRequest names what the governed use needs.
type ExchangeRequest struct {
	DelegationID string
	// Scopes are resource scopes, for example
	// "https://graph.microsoft.com/Sites.Selected". They come from the grant.
	Scopes []string
	// Claims is the CAE claims challenge being answered, if any: JSON, or
	// the base64 form a WWW-Authenticate header carries.
	Claims string
	// AgentID is the agent identity's application id. AgentOBO requires it;
	// ClassicOBO refuses it.
	AgentID string
	// ExecutionID and Generation derive the client-request-id sent on the
	// exchange (CorrelationID).
	ExecutionID string
	Generation  int64
}

// AccessToken is transient credential material for one invocation.
type AccessToken struct {
	Token     string
	ExpiresAt time.Time
	Scopes    []string
}

// String keeps the token out of %v and %s.
func (t AccessToken) String() string {
	return fmt.Sprintf("obo.AccessToken{expires_at=%s scopes=%v token=[redacted]}", t.ExpiresAt.UTC().Format(time.RFC3339), t.Scopes)
}

// GoString keeps the token out of %#v.
func (t AccessToken) GoString() string { return t.String() }

// LogValue keeps the token out of slog.
func (t AccessToken) LogValue() slog.Value {
	return slog.GroupValue(slog.Time("expires_at", t.ExpiresAt), slog.Any("scopes", t.Scopes))
}

// Exchanger trades a held assertion for a resource token.
type Exchanger interface {
	Exchange(ctx context.Context, a Assertion, r ExchangeRequest) (AccessToken, error)
}

// AgentAssertionSource returns the agent client assertion (T1) for one agent
// identity. #1152 provides the production implementation. The value is never
// logged or returned to a caller.
type AgentAssertionSource interface {
	AgentAssertion(ctx context.Context, tenantID, agentID string) (string, error)
}

var (
	// ErrInteractionRequired means the user must interact (MFA, consent,
	// sign-in again). The run pauses for authorization; it never falls back
	// to an app-only token.
	ErrInteractionRequired = errors.New("entra obo: user interaction required")
	// ErrAudienceMismatch means the assertion was not issued to this
	// exchanger's application (local check, or AADSTS50013).
	ErrAudienceMismatch = errors.New("entra obo: assertion audience mismatch")
	// ErrUnavailable is a transport failure, a 5xx, a throttle or an
	// unusable response. It is the only retryable error.
	ErrUnavailable = errors.New("entra obo: token endpoint unavailable")
	// ErrRejected is any other refusal by the token endpoint. It is a deny.
	ErrRejected = errors.New("entra obo: exchange rejected")
	// ErrInvalidRequest refuses a request before it leaves the process.
	ErrInvalidRequest = errors.New("entra obo: invalid exchange request")
	// ErrInvalidConfiguration refuses an exchanger configuration.
	ErrInvalidConfiguration = errors.New("entra obo: invalid configuration")
	// ErrAgentConsentNotGranted is AADSTS82014: interactive consent on an
	// agent identity is not possible, so an administrator must pre-grant it.
	// It is a configuration error, not an interaction requirement.
	ErrAgentConsentNotGranted = errors.New("entra obo: agent identity consent must be granted by an administrator")
	// ErrAgentAssertionUnavailable denies Agent OBO when no agent assertion
	// source is configured or it returned nothing.
	ErrAgentAssertionUnavailable = errors.New("entra obo: no agent client assertion is available")
)

// ErrClaimsChallenge is a CAE or Conditional Access claims challenge. Claims
// is the challenge to send back once the user has satisfied it. The run
// pauses for authorization; a challenge it cannot satisfy is a deny.
type ErrClaimsChallenge struct {
	Claims string
}

func (e *ErrClaimsChallenge) Error() string { return "entra obo: claims challenge" }

// correlationNamespace scopes CorrelationID. #1152 derives the same value.
var correlationNamespace = uuid.MustParse("6f0c5e0a-3f43-5b4e-9a7c-1117e1117e11")

// CorrelationID is the client-request-id of every exchange for one execution
// generation: a name-based UUID over the execution id and generation.
func CorrelationID(executionID string, generation int64) string {
	return uuid.NewSHA1(correlationNamespace, []byte(executionID+"\x00"+strconv.FormatInt(generation, 10))).String()
}
