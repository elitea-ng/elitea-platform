package remoteagent

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	coreoidc "github.com/coreos/go-oidc/v3/oidc"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/agentidentity/eliteanative"
)

// Refusal codes of an inbound assertion. They are stable: the MCP endpoint
// answers them in its 401 body and the audit event carries them.
const (
	CodeMalformed         = "agent_assertion_malformed"
	CodeIssuerNotTrusted  = "agent_assertion_issuer_not_trusted"
	CodeSignatureInvalid  = "agent_assertion_signature_invalid"
	CodeAudienceMismatch  = "agent_assertion_audience_mismatch"
	CodeExpired           = "agent_assertion_expired"
	CodeLifetimeTooLong   = "agent_assertion_lifetime_too_long"
	CodeReplayed          = "agent_assertion_replayed"
	CodeUnavailable       = "agent_assertion_unavailable"
	CodeAgentCallerNeeded = "agent_invocation_requires_agent_credential"
	CodeScopeExceeded     = "agent_invocation_scope_exceeded"
)

// RefusalError is one refused inbound assertion. Code is a constant above;
// nothing from the assertion is in it.
type RefusalError struct{ Code string }

func (e *RefusalError) Error() string { return "remote agent: assertion refused: " + e.Code }

func refused(code string) error { return &RefusalError{Code: code} }

// RefusalCode answers the code of a refusal, or CodeUnavailable for any
// other error.
func RefusalCode(err error) string {
	var refusal *RefusalError
	if errors.As(err, &refusal) {
		return refusal.Code
	}
	return CodeUnavailable
}

// clockSkew is the tolerance for exp, nbf and iat between deployments.
const clockSkew = 30 * time.Second

// TrustedKeys answers the key set of an allowlisted issuer. ok is false for
// an issuer not on the list. An error means the list could not be read.
type TrustedKeys interface {
	KeySet(ctx context.Context, issuer string) (keys coreoidc.KeySet, ok bool, err error)
}

// ReplayStore binds an assertion id to the credential that first presents
// it. Claim answers true for the first presentation and for a later one by
// the same presenter (one MCP session sends one cached assertion on each of
// its requests); it answers false when another presenter already holds the
// pair (a replay). The record is kept until expiresAt.
type ReplayStore interface {
	Claim(ctx context.Context, issuer, jti, presenter string, expiresAt time.Time) (fresh bool, err error)
}

// Verified is an accepted assertion. It holds no token.
type Verified struct {
	Issuer    string
	Subject   string
	JTI       string
	Run       string
	Tenant    string
	Project   string
	User      string
	Mandate   string
	Scope     eliteanative.DelegationScope
	ExpiresAt time.Time
}

// Verifier checks inbound delegation assertions. Safe for concurrent use.
type Verifier struct {
	keys   TrustedKeys
	replay ReplayStore
	now    func() time.Time
}

// NewVerifier builds the verifier. now nil uses time.Now.
func NewVerifier(keys TrustedKeys, replay ReplayStore, now func() time.Time) (*Verifier, error) {
	if keys == nil || replay == nil {
		return nil, errors.New("remote agent: the verifier needs trusted keys and a replay store")
	}
	if now == nil {
		now = time.Now
	}
	return &Verifier{keys: keys, replay: replay, now: now}, nil
}

type header struct {
	Algorithm string `json:"alg"`
	Type      string `json:"typ"`
	KeyID     string `json:"kid"`
}

type inboundClaims struct {
	Issuer    string                        `json:"iss"`
	Subject   string                        `json:"sub"`
	Audience  json.RawMessage               `json:"aud"`
	IssuedAt  *int64                        `json:"iat"`
	NotBefore *int64                        `json:"nbf"`
	Expiry    *int64                        `json:"exp"`
	ID        string                        `json:"jti"`
	Purpose   string                        `json:"elitea_purpose"`
	Run       string                        `json:"elitea_run"`
	Tenant    string                        `json:"elitea_tenant"`
	Project   string                        `json:"elitea_project"`
	User      *eliteanative.AssertedUser    `json:"elitea_user"`
	Mandate   string                        `json:"elitea_mandate"`
	Scope     *eliteanative.DelegationScope `json:"elitea_scope"`
}

// Verify checks raw for audience (NormalizeAudience of the endpoint the
// request reached). The order: shape, issuer allowlist, signature,
// audience, lifetime, then the one-time record, so a replay is detected only
// for an assertion that is otherwise valid.
func (v *Verifier) Verify(ctx context.Context, raw, audience, presenter string) (Verified, error) {
	if presenter == "" {
		return Verified{}, errors.New("remote agent: the presenting credential is required")
	}
	if raw == "" || len(raw) > eliteanative.MaxDelegationTokenBytes {
		return Verified{}, refused(CodeMalformed)
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return Verified{}, refused(CodeMalformed)
	}
	var head header
	if !decodeSegment(parts[0], &head) || head.Type != eliteanative.TokenType ||
		(head.Algorithm != string(eliteanative.ES256) && head.Algorithm != string(eliteanative.RS256)) {
		return Verified{}, refused(CodeMalformed)
	}
	var unverified struct {
		Issuer string `json:"iss"`
	}
	if !decodeSegment(parts[1], &unverified) || unverified.Issuer == "" {
		return Verified{}, refused(CodeMalformed)
	}
	keys, ok, err := v.keys.KeySet(ctx, unverified.Issuer)
	if err != nil {
		return Verified{}, err
	}
	if !ok || keys == nil {
		return Verified{}, refused(CodeIssuerNotTrusted)
	}
	payload, err := keys.VerifySignature(ctx, raw)
	if err != nil {
		return Verified{}, refused(CodeSignatureInvalid)
	}
	var claims inboundClaims
	if json.Unmarshal(payload, &claims) != nil || claims.Issuer != unverified.Issuer || claims.Subject == "" ||
		claims.ID == "" || len(claims.ID) > 128 || claims.Expiry == nil || claims.IssuedAt == nil ||
		claims.Purpose != eliteanative.DelegationPurpose || claims.Scope == nil || !eliteanative.ValidScope(*claims.Scope) {
		return Verified{}, refused(CodeMalformed)
	}
	if got, ok := singleAudience(claims.Audience); !ok || got != audience {
		return Verified{}, refused(CodeAudienceMismatch)
	}
	now := v.now()
	expiry := time.Unix(*claims.Expiry, 0)
	issued := time.Unix(*claims.IssuedAt, 0)
	if expiry.Sub(issued) > eliteanative.DelegationMaxTTL || !expiry.After(issued) {
		return Verified{}, refused(CodeLifetimeTooLong)
	}
	if !now.Add(-clockSkew).Before(expiry) {
		return Verified{}, refused(CodeExpired)
	}
	if issued.After(now.Add(clockSkew)) ||
		(claims.NotBefore != nil && time.Unix(*claims.NotBefore, 0).After(now.Add(clockSkew))) {
		return Verified{}, refused(CodeExpired)
	}
	fresh, err := v.replay.Claim(ctx, claims.Issuer, claims.ID, presenterDigest(presenter), expiry.Add(clockSkew))
	if err != nil {
		return Verified{}, err
	}
	if !fresh {
		return Verified{}, refused(CodeReplayed)
	}
	out := Verified{
		Issuer: claims.Issuer, Subject: claims.Subject, JTI: claims.ID, Run: claims.Run, Tenant: claims.Tenant,
		Project: claims.Project, Mandate: claims.Mandate, Scope: *claims.Scope, ExpiresAt: expiry.UTC(),
	}
	if claims.User != nil {
		out.User = claims.User.Subject
	}
	return out, nil
}

func decodeSegment(segment string, into any) bool {
	raw, err := base64.RawURLEncoding.DecodeString(segment)
	return err == nil && json.Unmarshal(raw, into) == nil
}

func singleAudience(raw json.RawMessage) (string, bool) {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return one, one != ""
	}
	var many []string
	if json.Unmarshal(raw, &many) == nil && len(many) == 1 {
		return many[0], many[0] != ""
	}
	return "", false
}

// ScopeAllows answers the first toolkit type of want that the asserted
// scope does not list ("" when every one is listed). The receiving
// platform calls it with the toolkit types of the agent it is asked to run.
func ScopeAllows(scope eliteanative.DelegationScope, want []string) string {
	allowed := make(map[string]bool, len(scope.Toolkits))
	for _, t := range scope.Toolkits {
		allowed[t] = true
	}
	for _, t := range want {
		if !allowed[t] {
			return t
		}
	}
	return ""
}

type verifiedKey struct{}

// ContextWithVerified carries an accepted assertion to the handlers.
func ContextWithVerified(ctx context.Context, verified Verified) context.Context {
	return context.WithValue(ctx, verifiedKey{}, verified)
}

// VerifiedFromContext answers the accepted assertion of the request.
func VerifiedFromContext(ctx context.Context) (Verified, bool) {
	verified, ok := ctx.Value(verifiedKey{}).(Verified)
	return verified, ok
}

// presenterDigest is the stored form of the presenting credential's
// reference: a SHA-256 digest, so the table holds no credential id.
func presenterDigest(presenter string) string {
	sum := sha256.Sum256([]byte("elitea.remote-agent.presenter.v1\x00" + presenter))
	return hex.EncodeToString(sum[:])
}
