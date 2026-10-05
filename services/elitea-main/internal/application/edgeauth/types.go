// Package edgeauth decides whether a normalized forwarded request may
// proceed. HTTP parsing, trusted-proxy validation, success mapping, and
// redirects remain owned by the API boundary.
package edgeauth

import (
	"context"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	browserapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/browserauth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth/browserflow"
)

const (
	MaxCredentials           = 32
	MaxCredentialTypeBytes   = 64
	MaxCredentialDataBytes   = 16 << 10
	MaxBrowserReferenceBytes = 1024
	MaxMethodBytes           = 32
	MaxProtoBytes            = 32
	MaxHostBytes             = 1024
	MaxURIBytes              = 8 << 10
	MaxIPBytes               = 2048
	MaxTargetBytes           = 512
	MaxScopeBytes            = 512
)

var (
	ErrInvalidConfiguration = errors.New("invalid edge-auth configuration")
	ErrInvalidRequest       = errors.New("invalid edge-auth request")
)

// Source is normalized only after the API boundary has established a trusted
// proxy. TargetPresent and ScopePresent preserve omitted-versus-empty query
// semantics for the later success mapper; the authorization kernel does not
// select or cache a mapper.
type Source struct {
	Method        string
	Proto         string
	Host          string
	URI           string
	IP            string
	Target        string
	TargetPresent bool
	Scope         string
	ScopePresent  bool
}

func (s Source) validate() bool {
	return requiredBoundedText(s.Method, MaxMethodBytes) &&
		requiredBoundedText(s.Proto, MaxProtoBytes) &&
		requiredBoundedText(s.Host, MaxHostBytes) &&
		requiredBoundedText(s.URI, MaxURIBytes) &&
		requiredBoundedText(s.IP, MaxIPBytes) &&
		boundedOptional(s.Target, s.TargetPresent, MaxTargetBytes) &&
		boundedOptional(s.Scope, s.ScopePresent, MaxScopeBytes)
}

type CredentialInput struct {
	// Present distinguishes an absent configured header from a present empty or
	// malformed credential. DirectHTTPTraversal treats the first present input
	// as authoritative; MainTraversal preserves configured order while trying
	// later handlers after an ordinary rejection.
	Present bool
	Type    string
	Data    string
}

func (c CredentialInput) validate() bool {
	if !c.Present {
		return c.Type == "" && c.Data == ""
	}
	return len(c.Type) <= MaxCredentialTypeBytes && utf8.ValidString(c.Type) &&
		!strings.ContainsFunc(c.Type, unicode.IsControl) &&
		len(c.Data) <= MaxCredentialDataBytes && utf8.ValidString(c.Data) &&
		!strings.ContainsFunc(c.Data, unicode.IsControl)
}

// BrowserSessionInput separates the internal server-side ID from an explicit
// compatibility reference. The merged HTTP adapter uses "-" so bearer cookie
// material is not forwarded as identity data.
type BrowserSessionInput struct {
	Present   bool
	ID        string
	Reference string
}

func (s BrowserSessionInput) validate() bool {
	if !s.Present {
		return s.ID == "" && s.Reference == ""
	}
	return browserflow.ValidateOpaqueID(s.ID) == nil &&
		requiredBoundedText(s.Reference, MaxBrowserReferenceBytes)
}

// TraversalPolicy makes the credential-rejection traversal difference between
// the direct Auth Core route and the current Main request hook explicit. Their
// public-rule ownership and transport composition remain separate concerns.
type TraversalPolicy uint8

const (
	DirectHTTPTraversal TraversalPolicy = iota + 1
	MainTraversal
)

type Request struct {
	Source         Source
	Credentials    []CredentialInput
	BrowserSession BrowserSessionInput
	Traversal      TraversalPolicy
}

func (r Request) validate() bool {
	if !r.Source.validate() || len(r.Credentials) > MaxCredentials ||
		!r.BrowserSession.validate() ||
		(r.Traversal != DirectHTTPTraversal && r.Traversal != MainTraversal) {
		return false
	}
	for _, credential := range r.Credentials {
		if !credential.validate() {
			return false
		}
	}
	return true
}

// CredentialResolution is deliberately typed: rejection is an authentication
// result, while a non-nil error is a dependency failure. This prevents an
// unclassified validator outage from being converted into public access.
type CredentialResolution uint8

const (
	CredentialAccepted CredentialResolution = iota + 1
	CredentialRejected
	// CredentialRevoked is a rejected NATIVE credential whose device session
	// is revoked (auth.ErrDeviceRevoked, ADR-0025). It is a rejection in every
	// respect except the answer: a bearer caller is told device_revoked so a
	// native client wipes instead of refreshing.
	CredentialRevoked
)

type CredentialResult struct {
	Resolution CredentialResolution
	Principal  auth.User
}

type CredentialAuthenticator interface {
	AuthenticateCredential(
		ctx context.Context,
		source Source,
		credential CredentialInput,
	) (CredentialResult, error)
}

// SessionAuthorizer is satisfied directly by browserauth.Service.
type SessionAuthorizer interface {
	Authorize(ctx context.Context, sessionID string) (browserapp.Authorization, error)
}

type DecisionKind uint8

const (
	DecisionAllow DecisionKind = iota + 1
	DecisionDeny
	DecisionLogin
	DecisionDependencyFailure
)

type DecisionReason uint8

const (
	ReasonCredentialAccepted DecisionReason = iota + 1
	ReasonBrowserSessionAccepted
	ReasonPublicRuleMatched
	ReasonCredentialRejected
	ReasonAuthenticationRequired
	ReasonDependencyUnavailable
	ReasonMalformedDependencyResult
	// ReasonCredentialRevoked is a deny whose credential was a revoked native
	// device session (CredentialRevoked). Appended last so no existing value
	// moves.
	ReasonCredentialRevoked
)

type AuthenticationType uint8

const (
	AuthenticationToken AuthenticationType = iota + 1
	AuthenticationUser
	AuthenticationPublic
)

type Authentication struct {
	Type                 AuthenticationType
	Principal            auth.User
	Reference            string
	BrowserAuthorization browserapp.Authorization
}

type PublicMatch struct {
	RuleIndex int
	RuleName  string
}

// Decision is a typed policy result. Source is retained so target/scope mapper
// selection remains an explicit, post-authorization API operation. The kernel
// has no request-decision cache.
type Decision struct {
	Kind           DecisionKind
	Reason         DecisionReason
	Source         Source
	Authentication Authentication
	PublicMatch    PublicMatch
}

func boundedUTF8(value string, limit int) bool {
	return len(value) <= limit && utf8.ValidString(value)
}

func requiredBoundedText(value string, limit int) bool {
	return value != "" && value == strings.TrimSpace(value) && boundedUTF8(value, limit) &&
		!strings.ContainsFunc(value, unicode.IsControl)
}

func boundedOptional(value string, present bool, limit int) bool {
	if !present {
		return value == ""
	}
	return boundedUTF8(value, limit) && !strings.ContainsFunc(value, unicode.IsControl)
}
