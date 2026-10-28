package eliteanative

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/agentidentity"
	identity "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity"
)

// Errors of the provider. Every one is returned before anything is signed.
// The broker maps them to provider_failed; none carries a claim value.
var (
	// ErrUnsupported: verified user evidence. An Elitea assertion cannot
	// prove a user, so delegated_verified is never satisfied here.
	ErrUnsupported = errors.New("elitea-native agent identity: unsupported: an Elitea assertion cannot prove a user")
	// ErrUnknownRelyingParty: the audience names no configured relying
	// party.
	ErrUnknownRelyingParty = errors.New("elitea-native agent identity: unknown relying party")
	// ErrIdentityNotActive: the record is not active (suspended, retiring,
	// retired, replaced) or not an Elitea-native record.
	ErrIdentityNotActive = errors.New("elitea-native agent identity: the identity is not active")
	// ErrStaleEpoch: the record's revocation epoch moved since the session
	// read it.
	ErrStaleEpoch = errors.New("elitea-native agent identity: the identity epoch is stale")
	// ErrInvalidRequest: the spec lacks the run context an assertion states.
	ErrInvalidRequest = errors.New("elitea-native agent identity: invalid assume request")
)

// IdentityReader reads the external agent identity record, the only state
// an Elitea principal has. repos.ExternalAgentIdentities implements it.
type IdentityReader interface {
	GetExternalIdentity(ctx context.Context, id string) (identity.ExternalAgentIdentity, error)
}

// Provider is the agentidentity.Provider of mode elitea_native. It is safe
// for concurrent use.
type Provider struct {
	issuer     string
	jwksURI    string
	tenant     string
	ring       *KeyRing
	identities IdentityReader
	parties    map[string]RelyingParty // by exact audience
	maxTTL     time.Duration
	now        func() time.Time
	newID      func() (string, error)
}

// Option configures a Provider.
type Option func(*Provider)

// WithClock replaces time.Now, for tests.
func WithClock(now func() time.Time) Option {
	return func(p *Provider) {
		if now != nil {
			p.now = now
		}
	}
}

// New builds the provider. cfg must be validated (ParseConfig).
func New(cfg Config, ring *KeyRing, identities IdentityReader, opts ...Option) (*Provider, error) {
	if ring == nil || identities == nil {
		return nil, errors.New("elitea-native agent identity: the key ring and the identity reader are required")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	p := &Provider{issuer: cfg.Issuer(), jwksURI: cfg.JWKSURI(), tenant: cfg.Tenant, ring: ring, identities: identities,
		parties: make(map[string]RelyingParty, len(cfg.RelyingParties)),
		now:     func() time.Time { return time.Now().UTC() },
		newID: func() (string, error) {
			id, err := uuid.NewRandom()
			return id.String(), err
		}}
	held := ring.Algorithms()
	for _, rp := range cfg.RelyingParties {
		if !slices.Contains(held, rp.Algorithm) {
			return nil, invalid("relying party %q uses %s but the key ring has no %s key", rp.ID, rp.Algorithm, rp.Algorithm)
		}
		p.parties[rp.Audience] = rp
		p.maxTTL = max(p.maxTTL, rp.MaxTTL)
	}
	for _, opt := range opts {
		opt(p)
	}
	return p, nil
}

func (p *Provider) Name() string { return ProviderName }

// Capabilities: app-only, and a user Elitea asserts (stated as asserted,
// never verified). No IdP object, no workload federation of its own.
func (p *Provider) Capabilities() agentidentity.Capabilities {
	return agentidentity.Capabilities{
		AppOnly:               true,
		AcceptsAssertedUser:   true,
		DelegationFidelity:    agentidentity.FidelityAttributed,
		NarrowingKinds:        []agentidentity.NarrowingKind{agentidentity.NarrowAudience},
		MinTTL:                MinTTL,
		MaxTTL:                p.maxTTL,
		RevocationGranularity: agentidentity.RevokeIdentity,
	}
}

// Provision creates the Elitea principal: the record itself. No external
// call is made; the ref's id is the record id.
func (p *Provider) Provision(_ context.Context, spec agentidentity.ProvisionSpec) (agentidentity.ProvisionResult, error) {
	if spec.Mode != identity.IdentityModeEliteaNative {
		return agentidentity.ProvisionResult{}, ErrIdentityNotActive
	}
	if _, err := uuid.Parse(spec.ExternalAgentIdentityID); err != nil {
		return agentidentity.ProvisionResult{}, ErrInvalidRequest
	}
	return agentidentity.ProvisionResult{Ref: identity.ExternalRef{
		Provider: ProviderName, ProviderTenant: spec.ProviderTenant, Kind: RefKind,
		ID: spec.ExternalAgentIdentityID, Generation: 1,
	}}, nil
}

// Activate has nothing to activate outside Elitea.
func (p *Provider) Activate(context.Context, identity.ExternalRef) error { return nil }

// Resolve checks the local record.
func (p *Provider) Resolve(ctx context.Context, ref identity.ExternalRef) (agentidentity.Resolved, error) {
	record, err := p.identities.GetExternalIdentity(ctx, ref.ID)
	switch {
	case errors.Is(err, identity.ErrExternalIdentityNotFound):
		return agentidentity.Resolved{}, agentidentity.ErrIdentityNotFound
	case err != nil:
		return agentidentity.Resolved{}, err
	case record.Ref.Provider != ProviderName || record.Ref.ID != ref.ID || record.Ref.Generation != ref.Generation:
		return agentidentity.Resolved{}, agentidentity.ErrIdentityReplaced
	case record.State == identity.ExternalIdentityRetired || record.State == identity.ExternalIdentityReplaced:
		return agentidentity.Resolved{}, agentidentity.ErrIdentityNotFound
	}
	enabled := record.State == identity.ExternalIdentityActive || record.State == identity.ExternalIdentityPendingActivation
	return agentidentity.Resolved{Ref: record.Ref, Enabled: enabled, OwnerRef: record.OwnerRef}, nil
}

// AssumeApp signs one app-only assertion.
func (p *Provider) AssumeApp(ctx context.Context, spec agentidentity.AssumeSpec) (agentidentity.SessionCredential, error) {
	return p.assume(ctx, spec, nil)
}

// AssumeOnBehalfOf signs an assertion that states an asserted user. Verified
// evidence is ErrUnsupported: the assertion cannot prove a user.
func (p *Provider) AssumeOnBehalfOf(ctx context.Context, spec agentidentity.OBOSpec) (agentidentity.SessionCredential, error) {
	switch evidence := spec.Evidence.(type) {
	case agentidentity.AssertedUserEvidence:
		if evidence.Subject == "" {
			return agentidentity.SessionCredential{}, ErrInvalidRequest
		}
		return p.assume(ctx, spec.AssumeSpec, &AssertedUser{Subject: evidence.Subject, Evidence: EvidenceAsserted})
	case agentidentity.VerifiedUserEvidence, *agentidentity.VerifiedUserEvidence:
		return agentidentity.SessionCredential{}, ErrUnsupported
	}
	return agentidentity.SessionCredential{}, ErrInvalidRequest
}

// Suspend acts through the record's epoch: the service suspends the record
// first, and the next sign re-reads it and refuses. Outstanding assertions
// expire within the relying party's max_ttl.
func (p *Provider) Suspend(context.Context, identity.ExternalRef) error { return nil }

// Resume acts through the record, as Suspend does.
func (p *Provider) Resume(context.Context, identity.ExternalRef) error { return nil }

// Revoke acts through the record, as Suspend does.
func (p *Provider) Revoke(context.Context, agentidentity.RevokeSpec) error { return nil }

// List answers no objects: an Elitea principal is its record, so there is
// never an orphan. The reconcile loop resolves every record instead.
func (p *Provider) List(context.Context, agentidentity.ListSpec) (agentidentity.ListPage, error) {
	return agentidentity.ListPage{}, nil
}

// EvidenceAsserted is the only evidence value elitea_user ever carries.
const EvidenceAsserted = "asserted"

// AssertedUser is the elitea_user claim. A relying party must not treat it
// as a verified user.
type AssertedUser struct {
	Subject  string `json:"sub"`
	Evidence string `json:"evidence"`
}

// Claims is the assertion body. No email, display name or token appears.
type Claims struct {
	Issuer          string        `json:"iss"`
	Subject         string        `json:"sub"`
	Audience        string        `json:"aud"`
	IssuedAt        int64         `json:"iat"`
	NotBefore       int64         `json:"nbf"`
	Expiry          int64         `json:"exp"`
	ID              string        `json:"jti"`
	SubjectProfile  string        `json:"sub_profile"`
	AgentInstanceID string        `json:"agent_instance_id"`
	Tenant          string        `json:"elitea_tenant"`
	Project         string        `json:"elitea_project"`
	Binding         string        `json:"elitea_binding"`
	VersionDigest   string        `json:"elitea_version_digest"`
	PolicyRevision  string        `json:"elitea_policy_revision"`
	Epoch           int64         `json:"elitea_epoch"`
	User            *AssertedUser `json:"elitea_user,omitempty"`
}

// TokenTypeJWT is SessionCredential.TokenType (RFC 8693 section 3).
const TokenTypeJWT = "urn:ietf:params:oauth:token-type:jwt"

func (p *Provider) assume(ctx context.Context, spec agentidentity.AssumeSpec, user *AssertedUser) (agentidentity.SessionCredential, error) {
	now := p.now().UTC()
	// Every check runs before anything is signed.
	party, ok := p.parties[spec.Audience]
	if !ok {
		return agentidentity.SessionCredential{}, ErrUnknownRelyingParty
	}
	run := spec.Run
	if spec.Ref.Provider != ProviderName || spec.Ref.ID == "" || run.ExternalAgentIdentityID != spec.Ref.ID ||
		run.TenantProjectID <= 0 || run.ExecutionID == "" || run.Generation <= 0 || run.AgentBindingID == "" ||
		run.PolicyRevisionID == "" || run.SessionExpiresAt.IsZero() || spec.TTL <= 0 {
		return agentidentity.SessionCredential{}, ErrInvalidRequest
	}
	record, err := p.identities.GetExternalIdentity(ctx, spec.Ref.ID)
	switch {
	case err != nil:
		return agentidentity.SessionCredential{}, ErrIdentityNotActive
	case record.Ref.Provider != ProviderName || record.Ref.ID != spec.Ref.ID || record.Ref.Generation != spec.Ref.Generation:
		return agentidentity.SessionCredential{}, agentidentity.ErrIdentityReplaced
	case record.State != identity.ExternalIdentityActive || record.Mode != identity.IdentityModeEliteaNative ||
		record.TenantProjectID != run.TenantProjectID:
		return agentidentity.SessionCredential{}, ErrIdentityNotActive
	case record.RevocationEpoch != run.ExternalEpoch:
		return agentidentity.SessionCredential{}, ErrStaleEpoch
	}
	// The lifetime is the shortest of the request, the relying party's
	// max_ttl and the session's remaining life, in whole seconds.
	ttl := min(spec.TTL, party.MaxTTL, run.SessionExpiresAt.Sub(now))
	seconds := int64(ttl / time.Second)
	if seconds < int64(MinTTL/time.Second) {
		return agentidentity.SessionCredential{}, ErrInvalidRequest
	}
	jti, err := p.newID()
	if err != nil {
		return agentidentity.SessionCredential{}, err
	}
	issued := now.Unix()
	claims := Claims{
		Issuer: p.issuer, Subject: "agent:" + record.ID, Audience: party.Audience,
		IssuedAt: issued, NotBefore: issued, Expiry: issued + seconds, ID: jti,
		SubjectProfile:  "ai_agent",
		AgentInstanceID: run.ExecutionID + ":" + strconv.FormatInt(run.Generation, 10),
		Tenant:          p.tenant, Project: strconv.FormatInt(run.TenantProjectID, 10),
		Binding: run.AgentBindingID, VersionDigest: hex.EncodeToString(run.VersionDigest[:]),
		PolicyRevision: run.PolicyRevisionID, Epoch: record.RevocationEpoch, User: user,
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return agentidentity.SessionCredential{}, err
	}
	token, err := p.ring.sign(ctx, party.Algorithm, now, func(kid string) ([]byte, error) {
		return json.Marshal(struct {
			Algorithm string `json:"alg"`
			KeyID     string `json:"kid"`
			Type      string `json:"typ"`
		}{string(party.Algorithm), kid, TokenType})
	}, payload)
	if err != nil {
		return agentidentity.SessionCredential{}, err
	}
	agent := agentidentity.Principal{Kind: "agent", Ref: claims.Subject}
	actor := agentidentity.NormalizedActor{Current: agent, Chain: []agentidentity.Principal{agent}, Fidelity: agentidentity.FidelityAttributed}
	if user != nil {
		actor.Subject = &agentidentity.Principal{Kind: "user", Ref: user.Subject}
	}
	return agentidentity.SessionCredential{Token: token, TokenType: TokenTypeJWT,
		ExpiresAt: time.Unix(claims.Expiry, 0).UTC(), Actor: actor}, nil
}

// WellKnown answers the public documents of this provider's issuer.
func (p *Provider) WellKnown() *WellKnown {
	return &WellKnown{ring: p.ring, issuer: p.issuer, jwksURI: p.jwksURI, algorithms: p.ring.Algorithms(), now: p.now}
}

var _ agentidentity.Provider = (*Provider)(nil)
