// Package agentidentity is the provider-neutral core of ADR-0039 decision 11
// (issue #1151): the agent identity Provider port, the Registry of adapters
// built at the composition root, the ExternalIdentityService behind the admin
// API, and the per-run assume Broker.
//
// Only two callers reach an adapter: Main admission, through
// Broker.OpenSession, and the credential broker, through Broker.Credential.
// The admin API reaches one through ExternalIdentityService for the
// provider object's own life cycle. Nothing here loads plugins: the
// composition root passes the adapters to NewRegistry, and only with the
// governance flag on.
//
// No type here persists or logs a token. SessionCredential redacts itself
// in every formatting path.
package agentidentity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	identity "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity"
)

// Provider is one identity provider adapter (Entra #1152, Elitea-native
// #1153, others #1155). Every method takes references and answers
// references, except AssumeApp and AssumeOnBehalfOf, which answer a
// SessionCredential.
type Provider interface {
	// Name is the registry key, identity.ValidProviderName.
	Name() string
	Capabilities() Capabilities
	// Provision creates or finds the provider object. Idempotent on
	// spec.ExternalAgentIdentityID.
	Provision(ctx context.Context, spec ProvisionSpec) (ProvisionResult, error)
	Activate(ctx context.Context, ref identity.ExternalRef) error
	// Resolve reads the provider object. A recreated object or a generation
	// mismatch is ErrIdentityReplaced; an object that no longer exists is
	// ErrIdentityNotFound.
	Resolve(ctx context.Context, ref identity.ExternalRef) (Resolved, error)
	AssumeApp(ctx context.Context, spec AssumeSpec) (SessionCredential, error)
	// AssumeOnBehalfOf answers *InteractionRequired or an error when the
	// exchange fails. It never falls back to an app-only credential.
	AssumeOnBehalfOf(ctx context.Context, spec OBOSpec) (SessionCredential, error)
	Suspend(ctx context.Context, ref identity.ExternalRef) error
	Resume(ctx context.Context, ref identity.ExternalRef) error
	Revoke(ctx context.Context, spec RevokeSpec) error
	// List pages the provider objects tagged as Elitea-owned (the reconcile
	// loop, #1154). An object Elitea holds no record for is an orphan.
	List(ctx context.Context, spec ListSpec) (ListPage, error)
}

// ErrIdentityReplaced means the provider object behind a ref is not the one
// recorded: it was recreated, or its generation moved.
var ErrIdentityReplaced = errors.New("agent identity provider: the external identity was replaced")

// ErrIdentityNotFound means the provider object behind a ref no longer
// exists. Any other Resolve error is a provider failure, not a finding.
var ErrIdentityNotFound = errors.New("agent identity provider: the external identity was not found")

// InteractionRequired is the OBO failure a user must resolve interactively
// (consent, step-up, a claims challenge the run cannot satisfy). It is a
// deny, never a retry and never an app-only fallback.
type InteractionRequired struct {
	// Reason is a stable code ("consent_required", "claims_challenge").
	Reason string
}

func (e *InteractionRequired) Error() string {
	return "agent identity provider: user interaction required: " + e.Reason
}

// NarrowingKind names a dimension a provider can narrow a credential by.
type NarrowingKind string

const (
	NarrowAudience NarrowingKind = "audience"
	NarrowScope    NarrowingKind = "scope"
	NarrowResource NarrowingKind = "resource"
)

// RevocationGranularity is the smallest thing a provider can revoke.
type RevocationGranularity string

const (
	RevokeIdentity   RevocationGranularity = "identity"
	RevokeSession    RevocationGranularity = "session"
	RevokeCredential RevocationGranularity = "credential"
)

// Capabilities is what an adapter declares. The broker reads it; it never
// probes the provider.
type Capabilities struct {
	AgentObject        bool // the IdP has a first-class agent object
	AsyncProvisioning  bool // Provision may answer Pending
	WorkloadFederation bool // the adapter authenticates with a workload credential
	AppOnly            bool // AssumeApp is supported
	DelegationFidelity ActorFidelity
	// VerifiesUser: AssumeOnBehalfOf takes VerifiedUserEvidence.
	VerifiesUser bool
	// AcceptsAssertedUser: the adapter can take a user Elitea states
	// (AWS SourceIdentity, AgentCore ForUserId). The broker passes asserted
	// evidence only when the policy allows it as well.
	AcceptsAssertedUser   bool
	NarrowingKinds        []NarrowingKind
	MinTTL                time.Duration
	MaxTTL                time.Duration
	RevocationGranularity RevocationGranularity
	// IdentityLimit is advisory (Entra: 250 agent identities per blueprint).
	IdentityLimit int
}

// ProvisionSpec asks a provider for the object of one record.
type ProvisionSpec struct {
	ExternalAgentIdentityID string
	TenantProjectID         int64
	ApplicationID           int64
	Mode                    identity.IdentityMode
	ProviderTenant          string
	ProviderConfigKey       string
	OwnerRef                string
}

// ProvisionResult is the provider object. Pending means it is still being
// created (Capabilities.AsyncProvisioning); the record stays provisioning.
type ProvisionResult struct {
	Ref          identity.ExternalRef
	AgentUserRef string
	Pending      bool
}

// Resolved is what Resolve reads, and one object of a List page.
type Resolved struct {
	Ref     identity.ExternalRef
	Enabled bool
	// OwnerRef is the owner the provider reports. Empty: the provider does
	// not report one, and the reconcile loop does not compare it.
	OwnerRef string
	// CredentialChanged reports a credential added, removed or rotated on
	// the object outside Elitea, where the provider can tell. A provider
	// that cannot tell leaves it false.
	CredentialChanged bool
}

// AssumeSpec asks for an app-only credential of the agent.
type AssumeSpec struct {
	Ref               identity.ExternalRef
	ProviderConfigKey string
	SessionID         string
	Audience          string
	Scopes            []string
	TTL               time.Duration
	// Run is what the broker read for this call. An adapter that issues its
	// own assertion (Elitea-native, #1153) states it in the claims; an
	// adapter that exchanges with an IdP ignores it.
	Run RunContext
}

// RunContext names the run a credential is for: references and the epochs
// the broker checked, never a token.
type RunContext struct {
	TenantProjectID         int64
	ExecutionID             string
	Generation              int64
	AgentBindingID          string
	AgentBindingEpoch       int64
	ExternalAgentIdentityID string
	ExternalEpoch           int64
	// VersionDigest is the binding's frozen definition digest.
	VersionDigest    [32]byte
	PolicyRevisionID string
	// SessionExpiresAt bounds every credential of the session.
	SessionExpiresAt time.Time
}

// OBOSpec asks for a credential of the agent acting for a user.
type OBOSpec struct {
	AssumeSpec
	Evidence UserEvidence
}

// RevokeSpec asks a provider to revoke an object.
type RevokeSpec struct {
	Ref    identity.ExternalRef
	Reason string
}

// ListSpec pages the provider's objects (the reconcile loop, #1154).
type ListSpec struct {
	ProviderTenant    string
	ProviderConfigKey string
	PageToken         string
	PageSize          int
}

// ListPage is one page. An empty NextPageToken is the last page.
type ListPage struct {
	Objects       []Resolved
	NextPageToken string
}

// UserEvidence is the sum type of ADR-0039 decision 3: VerifiedUserEvidence
// or AssertedUserEvidence. A nil UserEvidence is an app-only run.
type UserEvidence interface{ userEvidence() }

// VerifiedUserEvidence is an IdP-validated user held in delegation custody
// (#1117). It names the delegation; the adapter reads the assertion there.
type VerifiedUserEvidence struct{ Issuer, TenantID, ObjectID, Audience, DelegationID string }

// AssertedUserEvidence is a user Elitea states on the basis of a mandate.
// It never satisfies a policy that requires verified evidence.
type AssertedUserEvidence struct{ Issuer, Subject, MandateID string }

func (VerifiedUserEvidence) userEvidence() {}
func (AssertedUserEvidence) userEvidence() {}

// Principal is one normalised party of an actor chain.
type Principal struct {
	Kind string // "user", "agent", "workload", "application"
	Ref  string
}

// NormalizedActor is the one actor shape policy reads (ADR-0039 decision 5,
// RFC 8693 section 4.1).
type NormalizedActor struct {
	Subject  *Principal // nil for app-only
	Current  Principal  // the agent; the only actor used for access control
	Chain    []Principal
	Fidelity ActorFidelity
}

// SessionCredential is the one value here that holds a token. Token is never
// logged and never persisted outside the broker cache (#1121): String,
// GoString, LogValue and MarshalJSON all redact it.
type SessionCredential struct {
	Token     string
	TokenType string
	ExpiresAt time.Time
	Actor     NormalizedActor
}

const redacted = "[REDACTED]"

func (c SessionCredential) String() string {
	return fmt.Sprintf("SessionCredential{TokenType:%s ExpiresAt:%s Token:%s}", c.TokenType, c.ExpiresAt.UTC().Format(time.RFC3339), redacted)
}

// GoString keeps %#v from printing the token.
func (c SessionCredential) GoString() string { return c.String() }

// LogValue keeps slog from printing the token.
func (c SessionCredential) LogValue() slog.Value {
	return slog.GroupValue(slog.String("token_type", c.TokenType), slog.Time("expires_at", c.ExpiresAt),
		slog.String("token", redacted))
}

// MarshalJSON keeps an accidental encode from writing the token.
func (c SessionCredential) MarshalJSON() ([]byte, error) {
	return []byte(fmt.Sprintf(`{"token":%q,"token_type":%q}`, redacted, c.TokenType)), nil
}

// ActorFidelity is how faithfully an issued credential names its actor
// (ADR-0039 decision 5). The values match securityaudit.ActorFidelity.
type ActorFidelity string

const (
	FidelityNativeAct     ActorFidelity = "native_act"
	FidelityConfiguredAct ActorFidelity = "configured_act"
	FidelityAZPOnly       ActorFidelity = "azp_only"
	FidelityAttributed    ActorFidelity = "attributed"
	FidelityNone          ActorFidelity = "none"
)
