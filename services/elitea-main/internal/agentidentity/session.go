package agentidentity

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
	"time"
)

// SecurityProfile is fixed at OpenSession (ADR-0039 decision 3). Nothing
// converts one into the other.
type SecurityProfile string

const (
	// ProfileAutonomousAttributed: the agent acts as itself, or for a user
	// Elitea asserts on the basis of a mandate.
	ProfileAutonomousAttributed SecurityProfile = "autonomous_attributed"
	// ProfileDelegatedVerified: the agent acts for a user whose token an IdP
	// validated. Requires verified evidence.
	ProfileDelegatedVerified SecurityProfile = "delegated_verified"
)

// Valid reports whether p is a modelled profile.
func (p SecurityProfile) Valid() bool {
	return p == ProfileAutonomousAttributed || p == ProfileDelegatedVerified
}

// EvidenceKind is the kind of UserEvidence a session holds.
type EvidenceKind string

const (
	EvidenceNone     EvidenceKind = "none"
	EvidenceVerified EvidenceKind = "verified"
	EvidenceAsserted EvidenceKind = "asserted"
)

// EvidenceKindOf answers the kind of e (nil is none).
func EvidenceKindOf(e UserEvidence) EvidenceKind {
	switch e.(type) {
	case VerifiedUserEvidence, *VerifiedUserEvidence:
		return EvidenceVerified
	case AssertedUserEvidence, *AssertedUserEvidence:
		return EvidenceAsserted
	}
	return EvidenceNone
}

// SessionState is where a session is.
type SessionState string

const (
	SessionOpen   SessionState = "open"
	SessionClosed SessionState = "closed"
)

// Narrowing is the ceiling a session may ever be narrowed to: the relying
// parties (audiences) and the scopes. Every Credential call asks for one
// audience in it and a subset of its scopes.
type Narrowing struct {
	Audiences []string
	Scopes    []string
}

// CredentialRequest is the narrowing one Credential call asks for. Empty
// Scopes asks for every scope of the ceiling.
type CredentialRequest struct {
	Audience string
	Scopes   []string
}

// AgentSession is the run-scoped session row (elitea_identity.agent_sessions).
// It holds references and epochs only. The session ref itself is never
// stored: the row keys on its sha256.
type AgentSession struct {
	ID                      string
	TenantProjectID         int64
	ExecutionID             string
	Generation              int64
	AgentBindingID          string
	AgentBindingEpoch       int64
	ExternalAgentIdentityID string
	ExternalGeneration      int64
	ExternalEpoch           int64
	Profile                 SecurityProfile
	EvidenceKind            EvidenceKind
	DelegationID            string
	MandateID               string
	// The evidence's references: issuer, tenant, subject (object id or
	// asserted subject) and audience. Never an assertion.
	EvidenceIssuer   string
	EvidenceTenantID string
	EvidenceSubject  string
	EvidenceAudience string
	Ceiling          Narrowing
	// WorkerClaimRef is bound at the first Credential call; every later call
	// must present the same one.
	WorkerClaimRef   string
	PolicyRevisionID string
	State            SessionState
	ExpiresAt        time.Time
	CreatedAt        time.Time
}

// SessionRef is the opaque reference admission puts into
// ExecutionSecurityContextV1.agent_session_ref. It is not a credential on
// its own: Credential also checks execution, generation and worker claim.
type SessionRef string

const (
	sessionRefPrefix   = "ags1_"
	sessionRefRawBytes = 32
)

var errMalformedRef = errors.New("agent identity: malformed session ref")

func newSessionRef() (SessionRef, [32]byte, error) {
	raw := make([]byte, sessionRefRawBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", [32]byte{}, err
	}
	ref := SessionRef(sessionRefPrefix + base64.RawURLEncoding.EncodeToString(raw))
	return ref, sha256.Sum256([]byte(ref)), nil
}

// Digest is the key the session row is stored under.
func (r SessionRef) Digest() ([32]byte, error) {
	s := string(r)
	if !strings.HasPrefix(s, sessionRefPrefix) {
		return [32]byte{}, errMalformedRef
	}
	raw, err := base64.RawURLEncoding.DecodeString(s[len(sessionRefPrefix):])
	if err != nil || len(raw) != sessionRefRawBytes {
		return [32]byte{}, errMalformedRef
	}
	return sha256.Sum256([]byte(s)), nil
}

// String keeps a ref out of logs by default; the caller that must carry it
// uses string(ref).
func (r SessionRef) String() string { return "agent_session_ref[REDACTED]" }

// SessionClaim is what the credential broker presents with a ref: the
// execution and generation it redeems for and the worker claim it verified.
type SessionClaim struct {
	Ref            SessionRef
	ExecutionID    string
	Generation     int64
	WorkerClaimRef string
}
