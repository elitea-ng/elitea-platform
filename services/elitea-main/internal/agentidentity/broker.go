package agentidentity

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	identity "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// DenyReason is the typed reason of a broker denial. It is safe to return
// and to record.
type DenyReason string

const (
	ReasonInvalidRequest            DenyReason = "invalid_request"
	ReasonModeNotAllowed            DenyReason = "identity_mode_not_allowed"
	ReasonBindingNotUsable          DenyReason = "binding_not_usable"
	ReasonBindingEpochStale         DenyReason = "binding_epoch_stale"
	ReasonExternalIdentityNotActive DenyReason = "external_identity_not_active"
	ReasonExternalIdentityStale     DenyReason = "external_identity_epoch_stale"
	ReasonExternalIdentityReplaced  DenyReason = "external_identity_replaced"
	ReasonProviderNotConfigured     DenyReason = "provider_not_configured"
	ReasonProfileEvidenceMismatch   DenyReason = "profile_evidence_mismatch"
	ReasonAssertedUserNotAccepted   DenyReason = "asserted_user_not_accepted"
	ReasonVerifiedUserNotSupported  DenyReason = "verified_user_not_supported"
	ReasonAppOnlyNotSupported       DenyReason = "app_only_not_supported"
	ReasonSessionNotFound           DenyReason = "session_not_found"
	ReasonSessionNotOpen            DenyReason = "session_not_open"
	ReasonSessionExpired            DenyReason = "session_expired"
	ReasonSessionClaimMismatch      DenyReason = "session_claim_mismatch"
	ReasonOutsideCeiling            DenyReason = "narrowing_outside_ceiling"
	ReasonTTLTooShort               DenyReason = "remaining_ttl_below_provider_minimum"
	ReasonInteractionRequired       DenyReason = "interaction_required"
	ReasonProviderFailed            DenyReason = "provider_failed"
	ReasonAuditFailed               DenyReason = "audit_failed"
)

// ErrDenied matches every broker denial (errors.Is). errors.As with
// *Denial reads the reason.
var ErrDenied = errors.New("agent identity broker: denied")

// Denial is a typed broker denial. Cause, when set, is matched by errors.Is
// and errors.As (an *InteractionRequired, for example) but never printed.
type Denial struct {
	Reason DenyReason
	Cause  error
}

func (d *Denial) Error() string { return "agent identity broker: denied: " + string(d.Reason) }

// Unwrap exposes ErrDenied and the cause.
func (d *Denial) Unwrap() []error {
	if d.Cause == nil {
		return []error{ErrDenied}
	}
	return []error{ErrDenied, d.Cause}
}

func deny(reason DenyReason, cause error) *Denial { return &Denial{Reason: reason, Cause: cause} }

// ReasonOf answers the reason of a broker denial.
func ReasonOf(err error) (DenyReason, bool) {
	var d *Denial
	if errors.As(err, &d) {
		return d.Reason, true
	}
	return "", false
}

// SessionPolicy is what the broker reads from the active policy revision.
type SessionPolicy interface {
	identity.ModePolicy
	// AllowsAssertedUser reports whether asserted user evidence may reach an
	// adapter for mode. The default revision allows none (gate D4).
	AllowsAssertedUser(mode identity.IdentityMode) bool
}

// DenyAssertedUser is the default SessionPolicy: the revision's modes, and
// asserted user evidence never accepted. No revision field enables it yet.
type DenyAssertedUser struct{ identity.ModePolicy }

// AllowsAssertedUser always refuses.
func (DenyAssertedUser) AllowsAssertedUser(identity.IdentityMode) bool { return false }

// SessionStore persists sessions.
//
// CreateSession inserts s keyed on refDigest and appends event in the same
// transaction. ClaimSession locks the row of refDigest (FOR UPDATE), passes
// it to check and writes check's WorkerClaimRef when it changed; a missing
// row is ErrSessionNotFound. AppendEvent appends one event in its own
// transaction.
type SessionStore interface {
	CreateSession(ctx context.Context, s AgentSession, refDigest [32]byte, event securityaudit.SecurityEvent) error
	ClaimSession(ctx context.Context, refDigest [32]byte, check func(AgentSession) (AgentSession, error)) (AgentSession, error)
	AppendEvent(ctx context.Context, event securityaudit.SecurityEvent) error
}

// ErrSessionNotFound is a ref with no row.
var ErrSessionNotFound = errors.New("agent identity: session not found")

// BrokerConfig is the broker's dependencies. Now and NewID are optional.
type BrokerConfig struct {
	Registry   *Registry
	Policy     SessionPolicy
	Bindings   identity.BindingReader
	Identities ExternalIdentityStore
	Sessions   SessionStore
	Now        func() time.Time
	NewID      func() (string, error)
}

// Broker opens run-scoped agent sessions for admission and turns them into
// credentials for the credential broker. Those are its only two callers.
type Broker struct {
	registry   *Registry
	policy     SessionPolicy
	bindings   identity.BindingReader
	identities ExternalIdentityStore
	sessions   SessionStore
	now        func() time.Time
	newID      func() (string, error)
}

// NewBroker builds the broker. Every dependency except Now and NewID is
// required; an empty registry is valid.
func NewBroker(cfg BrokerConfig) (*Broker, error) {
	if cfg.Registry == nil || cfg.Policy == nil || cfg.Bindings == nil || cfg.Identities == nil || cfg.Sessions == nil {
		return nil, errors.New("agent identity broker: registry, policy, bindings, identities and sessions are required")
	}
	b := &Broker{registry: cfg.Registry, policy: cfg.Policy, bindings: cfg.Bindings, identities: cfg.Identities,
		sessions: cfg.Sessions, now: cfg.Now, newID: cfg.NewID}
	if b.now == nil {
		b.now = func() time.Time { return time.Now().UTC() }
	}
	if b.newID == nil {
		b.newID = newUUIDv7
	}
	return b, nil
}

// SessionAdmission is what admission passes to OpenSession after its AND
// decision (#1116) allowed a governed run.
type SessionAdmission struct {
	TenantProjectID int64
	ExecutionID     string
	Generation      int64
	// AgentBindingID and AgentBindingEpoch are the binding and the epoch the
	// admission decision read. A binding that moved since is denied.
	AgentBindingID    string
	AgentBindingEpoch int64
	Profile           SecurityProfile
	// Evidence is nil for an app-only run, VerifiedUserEvidence for
	// delegated_verified, AssertedUserEvidence for a delegated-background
	// mandate under autonomous_attributed.
	Evidence         UserEvidence
	MandateID        string
	DelegationID     string
	Ceiling          Narrowing
	ExpiresAt        time.Time
	PolicyRevisionID string
}

const (
	maxRefBytes      = 256
	maxCeilingItems  = 64
	brokerActor      = "system:agent-identity-broker"
	auditSessionType = "agent_session"
)

func boundedText(values ...string) bool {
	for _, v := range values {
		if len(v) > maxRefBytes || strings.ContainsAny(v, "\r\n\t\x00") {
			return false
		}
	}
	return true
}

func (in SessionAdmission) validate(now time.Time) error {
	switch {
	case in.TenantProjectID <= 0, strings.TrimSpace(in.ExecutionID) == "", len(in.ExecutionID) > 128,
		in.Generation <= 0, in.AgentBindingEpoch < 0, !in.Profile.Valid(), !in.ExpiresAt.After(now),
		len(in.Ceiling.Audiences) == 0, len(in.Ceiling.Audiences) > maxCeilingItems,
		len(in.Ceiling.Scopes) > maxCeilingItems:
		return deny(ReasonInvalidRequest, nil)
	}
	if _, err := uuid.Parse(in.AgentBindingID); err != nil {
		return deny(ReasonInvalidRequest, nil)
	}
	if !boundedText(in.MandateID, in.DelegationID, in.PolicyRevisionID) ||
		!boundedText(in.Ceiling.Audiences...) || !boundedText(in.Ceiling.Scopes...) {
		return deny(ReasonInvalidRequest, nil)
	}
	for _, a := range in.Ceiling.Audiences {
		if a == "" {
			return deny(ReasonInvalidRequest, nil)
		}
	}
	// The profile fixes the evidence kind; nothing converts one into the
	// other (ADR-0039 decision 3).
	switch evidence := in.Evidence.(type) {
	case nil:
		if in.Profile != ProfileAutonomousAttributed {
			return deny(ReasonProfileEvidenceMismatch, nil)
		}
	case VerifiedUserEvidence:
		if in.Profile != ProfileDelegatedVerified || evidence.DelegationID == "" || evidence.DelegationID != in.DelegationID ||
			!boundedText(evidence.Issuer, evidence.TenantID, evidence.ObjectID, evidence.Audience) {
			return deny(ReasonProfileEvidenceMismatch, nil)
		}
	case AssertedUserEvidence:
		// Asserted evidence never satisfies delegated_verified.
		if in.Profile != ProfileAutonomousAttributed || evidence.MandateID == "" || evidence.MandateID != in.MandateID ||
			!boundedText(evidence.Issuer, evidence.Subject) {
			return deny(ReasonProfileEvidenceMismatch, nil)
		}
	default:
		// A pointer or an unknown implementation is refused rather than
		// guessed at.
		return deny(ReasonInvalidRequest, nil)
	}
	return nil
}

// checkEvidence refuses evidence the adapter or the policy cannot take. It
// runs before any adapter call, in OpenSession and again in Credential.
func (b *Broker) checkEvidence(kind EvidenceKind, caps Capabilities, mode identity.IdentityMode) error {
	switch kind {
	case EvidenceAsserted:
		if !caps.AcceptsAssertedUser || !b.policy.AllowsAssertedUser(mode) {
			return deny(ReasonAssertedUserNotAccepted, nil)
		}
	case EvidenceVerified:
		if !caps.VerifiesUser {
			return deny(ReasonVerifiedUserNotSupported, nil)
		}
	default:
		if !caps.AppOnly {
			return deny(ReasonAppOnlyNotSupported, nil)
		}
	}
	return nil
}

// OpenSession opens a run-scoped agent session and answers its opaque ref.
// No token is minted. It re-reads the binding and its external identity,
// refuses evidence the profile, the adapter or the policy does not accept,
// resolves the provider object once (a replaced object is marked replaced
// and denied), and writes the session row with its audit event. Every call,
// allowed or denied, appends one TypeAgentSession event.
func (b *Broker) OpenSession(ctx context.Context, in SessionAdmission) (SessionRef, error) {
	now := b.now()
	event := securityaudit.SecurityEvent{
		Type: securityaudit.TypeAgentSession, Time: now, TenantProjectID: max(in.TenantProjectID, 0),
		Action: "agent_session.open", PrincipalRef: brokerActor, ExecutionID: boundedOrEmpty(in.ExecutionID),
		AgentBindingID: boundedOrEmpty(in.AgentBindingID), MandateRef: boundedOrEmpty(in.MandateID),
		DelegationRef: boundedOrEmpty(in.DelegationID), PolicyRevisionID: boundedOrEmpty(in.PolicyRevisionID),
		ResourceType: auditSessionType, AuthorizationEpoch: max(in.AgentBindingEpoch, 0),
		Reasons: []string{profileReason(in.Profile), "evidence." + string(EvidenceKindOf(in.Evidence))},
	}
	ref, session, err := b.openSession(ctx, in, now, &event)
	if err != nil {
		event.Effect = securityaudit.EffectDeny
		if reason, ok := ReasonOf(err); ok {
			event.Reasons = append(event.Reasons, string(reason))
		}
		if auditErr := b.sessions.AppendEvent(ctx, event); auditErr != nil {
			return "", errors.Join(err, deny(ReasonAuditFailed, auditErr))
		}
		return "", err
	}
	event.Effect = securityaudit.EffectAllow
	event.ResourceID = session.ID
	digest, _ := ref.Digest()
	if err := b.sessions.CreateSession(ctx, session, digest, event); err != nil {
		return "", deny(ReasonAuditFailed, err)
	}
	return ref, nil
}

func boundedOrEmpty(v string) string {
	if !boundedText(v) {
		return ""
	}
	return v
}

func profileReason(p SecurityProfile) string {
	if !p.Valid() {
		return "profile.unknown"
	}
	return "profile." + string(p)
}

func (b *Broker) openSession(ctx context.Context, in SessionAdmission, now time.Time, event *securityaudit.SecurityEvent) (SessionRef, AgentSession, error) {
	if err := in.validate(now); err != nil {
		return "", AgentSession{}, err
	}
	binding, err := b.bindings.Get(ctx, in.AgentBindingID)
	if err != nil || binding.TenantProjectID != in.TenantProjectID || binding.State != identity.BindingActive ||
		binding.ExternalAgentIdentityID == "" {
		return "", AgentSession{}, deny(ReasonBindingNotUsable, nil)
	}
	if binding.RevocationEpoch != in.AgentBindingEpoch {
		return "", AgentSession{}, deny(ReasonBindingEpochStale, nil)
	}
	if !b.policy.AllowsIdentityMode(binding.Mode) {
		return "", AgentSession{}, deny(ReasonModeNotAllowed, nil)
	}
	external, err := b.identities.GetExternalIdentity(ctx, binding.ExternalAgentIdentityID)
	if err != nil || external.TenantProjectID != in.TenantProjectID || external.State != identity.ExternalIdentityActive {
		return "", AgentSession{}, deny(ReasonExternalIdentityNotActive, nil)
	}
	event.AuthorityID = "external_agent_identity:" + external.ID
	event.RevocationEpoch = external.RevocationEpoch
	provider, ok := b.registry.Provider(external.Ref.Provider)
	if !ok {
		return "", AgentSession{}, deny(ReasonProviderNotConfigured, nil)
	}
	kind := EvidenceKindOf(in.Evidence)
	if err := b.checkEvidence(kind, provider.Capabilities(), binding.Mode); err != nil {
		return "", AgentSession{}, err
	}
	resolved, err := provider.Resolve(ctx, external.Ref)
	if errors.Is(err, ErrIdentityReplaced) || (err == nil && (resolved.Ref.ID != external.Ref.ID || resolved.Ref.Generation != external.Ref.Generation)) {
		return "", AgentSession{}, b.replaced(ctx, external)
	}
	if err != nil || !resolved.Enabled {
		return "", AgentSession{}, deny(ReasonProviderFailed, nil)
	}
	id, err := b.newID()
	if err != nil {
		return "", AgentSession{}, deny(ReasonInvalidRequest, err)
	}
	ref, _, err := newSessionRef()
	if err != nil {
		return "", AgentSession{}, deny(ReasonInvalidRequest, err)
	}
	session := AgentSession{
		ID: id, TenantProjectID: in.TenantProjectID, ExecutionID: in.ExecutionID, Generation: in.Generation,
		AgentBindingID: binding.ID, AgentBindingEpoch: binding.RevocationEpoch,
		ExternalAgentIdentityID: external.ID, ExternalGeneration: external.Ref.Generation, ExternalEpoch: external.RevocationEpoch,
		Profile: in.Profile, EvidenceKind: kind, DelegationID: in.DelegationID, MandateID: in.MandateID,
		Ceiling:          Narrowing{Audiences: slices.Clone(in.Ceiling.Audiences), Scopes: slices.Clone(in.Ceiling.Scopes)},
		PolicyRevisionID: in.PolicyRevisionID, State: SessionOpen, ExpiresAt: in.ExpiresAt.UTC(), CreatedAt: now,
	}
	switch evidence := in.Evidence.(type) {
	case VerifiedUserEvidence:
		session.EvidenceIssuer, session.EvidenceTenantID = evidence.Issuer, evidence.TenantID
		session.EvidenceSubject, session.EvidenceAudience = evidence.ObjectID, evidence.Audience
	case AssertedUserEvidence:
		session.EvidenceIssuer, session.EvidenceSubject = evidence.Issuer, evidence.Subject
	}
	return ref, session, nil
}

// replaced records a provider's "replaced" answer and denies.
func (b *Broker) replaced(ctx context.Context, external identity.ExternalAgentIdentity) error {
	if err := markReplaced(ctx, b.identities, external.ID, brokerActor, b.now()); err != nil {
		return deny(ReasonExternalIdentityReplaced, err)
	}
	return deny(ReasonExternalIdentityReplaced, ErrIdentityReplaced)
}

// Credential turns a session into a credential. Only the credential broker
// (#1118) calls it, during credentials:redeem, with the execution,
// generation and worker claim it verified. It re-checks the session, the
// binding epoch and the external identity's state, generation and epoch,
// intersects the request with the session's ceiling, refuses evidence the
// adapter or policy does not accept before any adapter call, and calls the
// adapter. An OBO failure is a denial; it never falls back to AssumeApp.
// Every call appends one TypeAgentSession event; an audit failure discards
// the credential.
func (b *Broker) Credential(ctx context.Context, claim SessionClaim, req CredentialRequest) (SessionCredential, error) {
	now := b.now()
	event := securityaudit.SecurityEvent{
		Type: securityaudit.TypeAgentSession, Time: now, Action: "agent_session.credential",
		PrincipalRef: brokerActor, ExecutionID: boundedOrEmpty(claim.ExecutionID), ResourceType: auditSessionType,
	}
	credential, err := b.credential(ctx, claim, req, now, &event)
	if err != nil {
		event.Effect = securityaudit.EffectDeny
		if reason, ok := ReasonOf(err); ok {
			event.Reasons = append(event.Reasons, string(reason))
		}
		if auditErr := b.sessions.AppendEvent(ctx, event); auditErr != nil {
			return SessionCredential{}, errors.Join(err, deny(ReasonAuditFailed, auditErr))
		}
		return SessionCredential{}, err
	}
	event.Effect = securityaudit.EffectAllow
	event.ActorFidelity = securityaudit.ActorFidelity(credential.Actor.Fidelity)
	if err := b.sessions.AppendEvent(ctx, event); err != nil {
		return SessionCredential{}, deny(ReasonAuditFailed, err)
	}
	return credential, nil
}

func (b *Broker) credential(ctx context.Context, claim SessionClaim, req CredentialRequest, now time.Time, event *securityaudit.SecurityEvent) (SessionCredential, error) {
	digest, err := claim.Ref.Digest()
	if err != nil || strings.TrimSpace(claim.ExecutionID) == "" || claim.Generation <= 0 ||
		strings.TrimSpace(claim.WorkerClaimRef) == "" || !boundedText(claim.WorkerClaimRef) {
		return SessionCredential{}, deny(ReasonInvalidRequest, nil)
	}
	session, err := b.sessions.ClaimSession(ctx, digest, func(s AgentSession) (AgentSession, error) {
		event.TenantProjectID, event.ResourceID = s.TenantProjectID, s.ID
		switch {
		case s.ExecutionID != claim.ExecutionID || s.Generation != claim.Generation:
			return AgentSession{}, deny(ReasonSessionClaimMismatch, nil)
		case s.State != SessionOpen:
			return AgentSession{}, deny(ReasonSessionNotOpen, nil)
		case !now.Before(s.ExpiresAt):
			return AgentSession{}, deny(ReasonSessionExpired, nil)
		case s.WorkerClaimRef != "" && s.WorkerClaimRef != claim.WorkerClaimRef:
			return AgentSession{}, deny(ReasonSessionClaimMismatch, nil)
		}
		// The first redemption binds the worker claim.
		s.WorkerClaimRef = claim.WorkerClaimRef
		return s, nil
	})
	if errors.Is(err, ErrSessionNotFound) {
		return SessionCredential{}, deny(ReasonSessionNotFound, nil)
	}
	if err != nil {
		if _, ok := ReasonOf(err); ok {
			return SessionCredential{}, err
		}
		return SessionCredential{}, deny(ReasonSessionNotFound, err)
	}
	event.AgentBindingID, event.MandateRef, event.DelegationRef = session.AgentBindingID, session.MandateID, session.DelegationID
	event.PolicyRevisionID, event.AuthorizationEpoch = session.PolicyRevisionID, session.AgentBindingEpoch
	event.AuthorityID, event.RevocationEpoch = "external_agent_identity:"+session.ExternalAgentIdentityID, session.ExternalEpoch
	event.Reasons = []string{profileReason(session.Profile), "evidence." + string(session.EvidenceKind)}

	binding, err := b.bindings.Get(ctx, session.AgentBindingID)
	if err != nil || binding.State != identity.BindingActive || binding.TenantProjectID != session.TenantProjectID {
		return SessionCredential{}, deny(ReasonBindingNotUsable, nil)
	}
	if binding.RevocationEpoch != session.AgentBindingEpoch {
		return SessionCredential{}, deny(ReasonBindingEpochStale, nil)
	}
	external, err := b.identities.GetExternalIdentity(ctx, session.ExternalAgentIdentityID)
	if err != nil || external.State != identity.ExternalIdentityActive {
		return SessionCredential{}, deny(ReasonExternalIdentityNotActive, nil)
	}
	if external.RevocationEpoch != session.ExternalEpoch || external.Ref.Generation != session.ExternalGeneration {
		return SessionCredential{}, deny(ReasonExternalIdentityStale, nil)
	}
	audience, scopes, err := narrow(session.Ceiling, req)
	if err != nil {
		return SessionCredential{}, err
	}
	provider, ok := b.registry.Provider(external.Ref.Provider)
	if !ok {
		return SessionCredential{}, deny(ReasonProviderNotConfigured, nil)
	}
	caps := provider.Capabilities()
	if err := b.checkEvidence(session.EvidenceKind, caps, binding.Mode); err != nil {
		return SessionCredential{}, err
	}
	ttl := session.ExpiresAt.Sub(now)
	if caps.MaxTTL > 0 && ttl > caps.MaxTTL {
		ttl = caps.MaxTTL
	}
	if ttl <= 0 || (caps.MinTTL > 0 && ttl < caps.MinTTL) {
		return SessionCredential{}, deny(ReasonTTLTooShort, nil)
	}
	spec := AssumeSpec{Ref: external.Ref, ProviderConfigKey: external.ProviderConfigKey, SessionID: session.ID,
		Audience: audience, Scopes: scopes, TTL: ttl}
	var credential SessionCredential
	switch session.EvidenceKind {
	case EvidenceVerified:
		credential, err = provider.AssumeOnBehalfOf(ctx, OBOSpec{AssumeSpec: spec, Evidence: VerifiedUserEvidence{
			Issuer: session.EvidenceIssuer, TenantID: session.EvidenceTenantID, ObjectID: session.EvidenceSubject,
			Audience: session.EvidenceAudience, DelegationID: session.DelegationID,
		}})
	case EvidenceAsserted:
		credential, err = provider.AssumeOnBehalfOf(ctx, OBOSpec{AssumeSpec: spec, Evidence: AssertedUserEvidence{
			Issuer: session.EvidenceIssuer, Subject: session.EvidenceSubject, MandateID: session.MandateID,
		}})
	default:
		credential, err = provider.AssumeApp(ctx, spec)
	}
	var interaction *InteractionRequired
	switch {
	case errors.Is(err, ErrIdentityReplaced):
		return SessionCredential{}, b.replaced(ctx, external)
	case errors.As(err, &interaction):
		return SessionCredential{}, deny(ReasonInteractionRequired, interaction)
	case err != nil:
		return SessionCredential{}, deny(ReasonProviderFailed, nil)
	case credential.Token == "":
		return SessionCredential{}, deny(ReasonProviderFailed, nil)
	}
	return credential, nil
}

// narrow intersects a request with the session's ceiling. The audience must
// be in the ceiling. Requested scopes must all be in it; none requested
// means the ceiling's scopes.
func narrow(ceiling Narrowing, req CredentialRequest) (string, []string, error) {
	if req.Audience == "" || !slices.Contains(ceiling.Audiences, req.Audience) {
		return "", nil, deny(ReasonOutsideCeiling, nil)
	}
	if len(req.Scopes) == 0 {
		return req.Audience, slices.Clone(ceiling.Scopes), nil
	}
	scopes := make([]string, 0, len(req.Scopes))
	for _, scope := range req.Scopes {
		if !slices.Contains(ceiling.Scopes, scope) {
			return "", nil, deny(ReasonOutsideCeiling, fmt.Errorf("scope outside the ceiling"))
		}
		if !slices.Contains(scopes, scope) {
			scopes = append(scopes, scope)
		}
	}
	return req.Audience, scopes, nil
}
