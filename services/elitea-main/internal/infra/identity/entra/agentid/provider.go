// Package agentid is the Microsoft Entra Agent ID adapter of the agent
// identity provider port (ADR-0039 decision 11, issue #1152).
//
// One Provider serves one agent identity blueprint in one tenant. The
// blueprint authenticates with a federated credential only: the projected
// service account token, re-read on every request. Each Elitea logical
// agent gets one agent identity under the blueprint, created disabled and
// tagged with a back-reference to its ExternalAgentIdentity record.
//
// Tokens:
//
//   - T1 is the blueprint's token for one agent identity (client
//     credentials, fmi_path = the agent's client id). It impersonates the
//     agent, so it never leaves this package: AssumeApp and AssumeOnBehalfOf
//     use it as the agent's client assertion, and the obo package reaches it
//     only through the obo.AgentAssertionSource this package implements.
//   - AssumeApp answers an app-only resource token of the agent
//     (idtyp=app); AssumeOnBehalfOf the #1117 Agent OBO token (idtyp=user).
//     Both are validated before they are returned: tenant, agent client id,
//     parent blueprint and token type.
//
// Nothing here logs token text: logs carry `uti` and Entra's request and
// correlation ids. The adapter talks to the token endpoint and Graph over
// HTTP itself, as the obo package does; it does not use MSAL.
package agentid

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/singleflight"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/agentidentity"
	identity "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/identity/entra/delegationstore"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/identity/entra/obo"
)

const (
	// ProviderName is the registry key.
	ProviderName = "entra"
	// RefKind is the ExternalRef kind of an agent identity.
	RefKind = "entra.agent_identity"
	// maxTokenTTL bounds a session credential's reported lifetime; Entra
	// issues resource tokens for 60 to 90 minutes.
	maxTokenTTL = time.Hour
	minTokenTTL = time.Minute
)

// SessionReader reads the agent session a credential call names
// (*repos.AgentSessions). The adapter takes the execution, generation and
// delegation from it, never from the caller.
type SessionReader interface {
	SessionByID(ctx context.Context, id string) (agentidentity.AgentSession, error)
}

// DelegationCustody opens a delegation's user assertion (*delegationstore.Store).
type DelegationCustody interface {
	Load(ctx context.Context, r delegationstore.LoadRequest) (obo.Assertion, delegationstore.Delegation, error)
}

// Provider is the Entra Agent ID adapter.
type Provider struct {
	cfg       Config
	http      *http.Client
	logger    *slog.Logger
	now       func() time.Time
	audiences []string
	agents    *agentCache
	obo       *obo.AgentOBO

	graphMu     sync.Mutex
	graph       issued
	graphFlight singleflight.Group

	// created remembers the object this replica created per record, so a
	// retry inside Graph's index lag finds it by id (strongly consistent).
	createdMu sync.Mutex
	created   map[string]string
}

const maxCreatedMemo = 1024

// New validates cfg and builds the adapter. It reads no file and makes no
// call: the first token request happens on first use.
func New(cfg Config) (*Provider, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: defaultHTTPTimeout}
	}
	// A redirect is never followed: it would resend a credential.
	bounded := *client
	bounded.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if bounded.Timeout == 0 {
		bounded.Timeout = defaultHTTPTimeout
	}
	p := &Provider{cfg: cfg, http: &bounded, logger: cfg.Logger, now: cfg.Now, audiences: slices.Clone(cfg.assertionAudiences())}
	if p.logger == nil {
		p.logger = slog.Default()
	}
	if p.now == nil {
		p.now = time.Now
	}
	if p.cfg.IdentityLimit == 0 {
		p.cfg.IdentityLimit = DefaultIdentityLimit
	}
	if p.cfg.DisplayNamePrefix == "" {
		p.cfg.DisplayNamePrefix = DefaultDisplayNamePrefix
	}
	size := cfg.AgentCacheSize
	if size == 0 {
		size = defaultAgentCacheSize
	}
	p.agents = newAgentCache(size)
	exchanger, err := obo.NewAgentOBO(obo.AgentConfig{
		Authority: p.cfg.authority(), BlueprintAppID: cfg.BlueprintAppID, AssertionAudiences: p.audiences,
		AllowedTenants: []string{cfg.TenantID}, Assertions: p.AssertionSource(), HTTPClient: p.http, Now: p.now,
	})
	if err != nil {
		return nil, invalid("agent OBO: %v", err)
	}
	p.obo = exchanger
	return p, nil
}

// String keeps the configuration and every cached token out of %v.
func (p *Provider) String() string {
	return "agentid.Provider{tenant=" + p.cfg.TenantID + " blueprint=" + p.cfg.BlueprintAppID + "}"
}

// GoString keeps cached tokens out of %#v.
func (p *Provider) GoString() string { return p.String() }

// TenantID is the blueprint's tenant.
func (p *Provider) TenantID() string { return p.cfg.TenantID }

// BlueprintAppID is the blueprint's application id.
func (p *Provider) BlueprintAppID() string { return p.cfg.BlueprintAppID }

// AssertionAudiences are the user assertion audiences Agent OBO accepts.
func (p *Provider) AssertionAudiences() []string { return slices.Clone(p.audiences) }

func (p *Provider) Name() string { return ProviderName }

func (p *Provider) Capabilities() agentidentity.Capabilities {
	return agentidentity.Capabilities{
		AgentObject: true, WorkloadFederation: true, AppOnly: true, VerifiesUser: true,
		AcceptsAssertedUser: false,
		DelegationFidelity:  agentidentity.FidelityConfiguredAct,
		NarrowingKinds: []agentidentity.NarrowingKind{
			agentidentity.NarrowAudience, agentidentity.NarrowScope, agentidentity.NarrowResource,
		},
		MinTTL: minTokenTTL, MaxTTL: maxTokenTTL,
		RevocationGranularity: agentidentity.RevokeIdentity,
		IdentityLimit:         p.cfg.IdentityLimit,
	}
}

func (p *Provider) checkRef(ref identity.ExternalRef) error {
	if ref.Provider != ProviderName || ref.ProviderTenant != p.cfg.TenantID || ref.Kind != RefKind || !guidPattern.MatchString(ref.ID) {
		return ErrInvalidRequest
	}
	return nil
}

func (p *Provider) checkConfigKey(key string) error {
	if key != "" && key != p.cfg.BlueprintAppID {
		return fmt.Errorf("%w: provider_config_key must be empty or the blueprint app id", ErrInvalidRequest)
	}
	return nil
}

func (p *Provider) ref(object agentObject) identity.ExternalRef {
	return identity.ExternalRef{Provider: ProviderName, ProviderTenant: p.cfg.TenantID, Kind: RefKind, ID: object.ID}
}

// maxRecordCopies bounds the record-tag lookup: more copies than this of one
// record means something other than a retry race is creating them.
const maxRecordCopies = 10

// Provision creates the agent identity of one record, disabled. It is
// idempotent on the record id: an object of the blueprint that carries the
// record's back-reference tag is returned instead. An identity an
// administrator created under the blueprint with that tag is adopted the
// same way (delegated creation): Provision disables it and adds the
// elitea:managed tag, so Resolve, List and the token paths treat it as
// Elitea's from then on.
//
// Idempotency under Graph's consistency model: Graph offers no strongly
// consistent alternate key Elitea can set on an agent identity
// (uniqueName is an alternate key of applications, not of service
// principals), and the tags/any lookup is an advanced query served from an
// eventually consistent index. So a retry inside the index lag can create
// a second copy. Two measures make that harmless:
//
//   - this replica remembers the object it created per record and checks
//     it by id (a strongly consistent read) before the tag lookup;
//   - a lookup that finds several copies keeps one deterministically, the
//     lowest object id, and soft-deletes the rest. Any deterministic choice
//     is correct here, because Provision runs only while the record holds
//     no ref yet, and every replica picks the same winner. A copy the index
//     had not yet shown is left for the reconcile loop, which reports it as
//     an orphan.
func (p *Provider) Provision(ctx context.Context, spec agentidentity.ProvisionSpec) (agentidentity.ProvisionResult, error) {
	if _, err := uuid.Parse(spec.ExternalAgentIdentityID); err != nil || spec.TenantProjectID <= 0 || spec.ApplicationID <= 0 ||
		spec.ProviderTenant != p.cfg.TenantID {
		return agentidentity.ProvisionResult{}, ErrInvalidRequest
	}
	if spec.Mode != identity.IdentityModeDedicatedAgent {
		return agentidentity.ProvisionResult{}, ErrModeNotSupported
	}
	if err := p.checkConfigKey(spec.ProviderConfigKey); err != nil {
		return agentidentity.ProvisionResult{}, err
	}
	sponsor := p.cfg.SponsorGroups[spec.TenantProjectID]
	if sponsor == "" {
		sponsor = p.cfg.DefaultSponsorGroupID
	}
	if sponsor == "" {
		return agentidentity.ProvisionResult{}, ErrNoSponsor
	}
	record := strings.ToLower(spec.ExternalAgentIdentityID)
	recordTag := tagRecordPrefix + record
	if id, ok := p.createdFor(record); ok {
		object, err := p.getAgent(ctx, id)
		switch {
		case err == nil && object.BlueprintID == p.cfg.BlueprintAppID && object.recordTag() == recordTag:
			return p.adopt(ctx, object)
		case err != nil && !errors.Is(err, errGraphNotFound):
			return agentidentity.ProvisionResult{}, err
		}
		p.forgetCreated(record)
	}
	existing, err := p.findAgents(ctx, maxRecordCopies, "tags/any(t:t eq "+odataString(recordTag)+")")
	if err != nil {
		return agentidentity.ProvisionResult{}, err
	}
	if len(existing) > 0 {
		return p.keepOne(ctx, existing)
	}
	count, err := p.countAgents(ctx)
	if err != nil {
		return agentidentity.ProvisionResult{}, err
	}
	if count >= p.cfg.IdentityLimit {
		p.logger.LogAttrs(ctx, slog.LevelWarn, "entra agent id blueprint is at its identity limit",
			slog.String("blueprint", p.cfg.BlueprintAppID), slog.Int("count", count), slog.Int("limit", p.cfg.IdentityLimit))
		return agentidentity.ProvisionResult{}, ErrIdentityLimit
	}
	tags := []string{tagManaged, recordTag, tagProjectPrefix + strconv.FormatInt(spec.TenantProjectID, 10)}
	object, err := p.createAgent(ctx, p.cfg.DisplayNamePrefix+strconv.FormatInt(spec.ApplicationID, 10), sponsor, tags)
	if err != nil {
		return agentidentity.ProvisionResult{}, err
	}
	if !guidPattern.MatchString(object.ID) || object.BlueprintID != p.cfg.BlueprintAppID {
		return agentidentity.ProvisionResult{}, ErrUnavailable
	}
	// Created disabled, so it is not put in the token cache: the first
	// token call after Activate reads it from Graph.
	p.rememberCreated(record, object.ID)
	p.logger.LogAttrs(ctx, slog.LevelInfo, "entra agent identity created", slog.String("object_id", object.ID),
		slog.String("record", spec.ExternalAgentIdentityID))
	return agentidentity.ProvisionResult{Ref: p.ref(object)}, nil
}

// keepOne returns the lowest-id copy of a record and soft-deletes the
// others (see Provision). Every copy is this blueprint's: the lookup filters
// on it, and the check below refuses anything else.
func (p *Provider) keepOne(ctx context.Context, copies []agentObject) (agentidentity.ProvisionResult, error) {
	for _, object := range copies {
		if object.BlueprintID != p.cfg.BlueprintAppID || !guidPattern.MatchString(object.ID) {
			return agentidentity.ProvisionResult{}, fmt.Errorf("%w: a record-tagged object of another blueprint", ErrRejected)
		}
	}
	slices.SortFunc(copies, func(a, b agentObject) int { return strings.Compare(a.ID, b.ID) })
	for _, duplicate := range copies[1:] {
		p.agents.evict(duplicate.ID)
		if err := p.deleteAgent(ctx, duplicate.ID); err != nil && !errors.Is(err, errGraphNotFound) {
			return agentidentity.ProvisionResult{}, err
		}
		p.logger.LogAttrs(ctx, slog.LevelWarn, "entra agent identity duplicate removed",
			slog.String("object_id", duplicate.ID), slog.String("kept", copies[0].ID))
	}
	return p.adopt(ctx, copies[0])
}

// adopt makes an existing record-tagged object of the blueprint Elitea's:
// disabled until Activate, and tagged elitea:managed.
func (p *Provider) adopt(ctx context.Context, object agentObject) (agentidentity.ProvisionResult, error) {
	if object.BlueprintID != p.cfg.BlueprintAppID {
		return agentidentity.ProvisionResult{}, fmt.Errorf("%w: the object belongs to another blueprint", ErrRejected)
	}
	p.agents.evict(object.ID)
	if object.AccountEnabled || !object.managed() {
		if err := p.adoptPatch(ctx, object); err != nil {
			return agentidentity.ProvisionResult{}, err
		}
	}
	return agentidentity.ProvisionResult{Ref: p.ref(object)}, nil
}

func (p *Provider) createdFor(record string) (string, bool) {
	p.createdMu.Lock()
	defer p.createdMu.Unlock()
	id, ok := p.created[record]
	return id, ok
}

func (p *Provider) rememberCreated(record, id string) {
	p.createdMu.Lock()
	defer p.createdMu.Unlock()
	if p.created == nil || len(p.created) >= maxCreatedMemo {
		// The memo only covers retries inside the index lag; dropping it
		// whole when full costs at most a heal later.
		p.created = map[string]string{}
	}
	p.created[record] = id
}

func (p *Provider) forgetCreated(record string) {
	p.createdMu.Lock()
	defer p.createdMu.Unlock()
	delete(p.created, record)
}

func (p *Provider) Activate(ctx context.Context, ref identity.ExternalRef) error {
	return p.patchEnabled(ctx, ref, true)
}

// Suspend sets accountEnabled=false: Entra issues the agent no token from
// then on, and this replica drops its cached T1 at once.
func (p *Provider) Suspend(ctx context.Context, ref identity.ExternalRef) error {
	return p.patchEnabled(ctx, ref, false)
}

func (p *Provider) Resume(ctx context.Context, ref identity.ExternalRef) error {
	return p.patchEnabled(ctx, ref, true)
}

func (p *Provider) patchEnabled(ctx context.Context, ref identity.ExternalRef, enabled bool) error {
	if err := p.checkRef(ref); err != nil {
		return err
	}
	p.agents.evict(ref.ID)
	err := p.setAccountEnabled(ctx, ref.ID, enabled)
	if errors.Is(err, errGraphNotFound) {
		return agentidentity.ErrIdentityNotFound
	}
	return err
}

// Revoke soft-deletes the agent identity on retire. Entra keeps it in
// deleted items for 30 days; purging it is an operator action.
func (p *Provider) Revoke(ctx context.Context, spec agentidentity.RevokeSpec) error {
	if err := p.checkRef(spec.Ref); err != nil {
		return err
	}
	p.agents.evict(spec.Ref.ID)
	err := p.deleteAgent(ctx, spec.Ref.ID)
	if errors.Is(err, errGraphNotFound) {
		return nil
	}
	return err
}

// Resolve reads the agent identity. An object of another blueprint, or one
// whose back-reference tag is gone, is ErrIdentityReplaced. A deleted object
// with a live successor (same record tag or same display name) is
// ErrIdentityReplaced; without one, ErrIdentityNotFound.
func (p *Provider) Resolve(ctx context.Context, ref identity.ExternalRef) (agentidentity.Resolved, error) {
	if err := p.checkRef(ref); err != nil {
		return agentidentity.Resolved{}, err
	}
	object, err := p.getAgent(ctx, ref.ID)
	if errors.Is(err, errGraphNotFound) {
		p.agents.evict(ref.ID)
		return agentidentity.Resolved{}, p.goneOrReplaced(ctx, ref.ID)
	}
	if err != nil {
		return agentidentity.Resolved{}, err
	}
	if err := p.owned(object); err != nil {
		p.agents.evict(ref.ID)
		return agentidentity.Resolved{}, err
	}
	// Only an enabled identity may sit in the token cache.
	if object.AccountEnabled && guidPattern.MatchString(object.clientID()) {
		p.agents.put(object.ID, object.clientID())
	} else {
		p.agents.evict(object.ID)
	}
	return agentidentity.Resolved{Ref: ref, Enabled: object.AccountEnabled}, nil
}

// owned checks an object is this blueprint's and carries Elitea's tags.
func (p *Provider) owned(object agentObject) error {
	if object.BlueprintID != p.cfg.BlueprintAppID || !object.managed() || object.recordTag() == "" {
		return agentidentity.ErrIdentityReplaced
	}
	return nil
}

func (p *Provider) goneOrReplaced(ctx context.Context, id string) error {
	deleted, err := p.getDeletedAgent(ctx, id)
	switch {
	case errors.Is(err, errGraphNotFound):
		return agentidentity.ErrIdentityNotFound
	case err != nil:
		return err
	}
	var successors []agentObject
	if tag := deleted.recordTag(); tag != "" {
		if successors, err = p.findAgents(ctx, 1, "tags/any(t:t eq "+odataString(tag)+")"); err != nil {
			return err
		}
	}
	if len(successors) == 0 && deleted.DisplayName != "" {
		if successors, err = p.findAgents(ctx, 1, "displayName eq "+odataString(deleted.DisplayName)); err != nil {
			return err
		}
	}
	if len(successors) > 0 {
		return agentidentity.ErrIdentityReplaced
	}
	return agentidentity.ErrIdentityNotFound
}

// List pages the blueprint's Elitea-tagged agent identities, 100 per page.
// The page token is the Graph nextLink.
func (p *Provider) List(ctx context.Context, spec agentidentity.ListSpec) (agentidentity.ListPage, error) {
	if (spec.ProviderTenant != "" && spec.ProviderTenant != p.cfg.TenantID) || p.checkConfigKey(spec.ProviderConfigKey) != nil {
		return agentidentity.ListPage{}, nil
	}
	size := spec.PageSize
	if size <= 0 || size > graphPageSize {
		size = graphPageSize
	}
	page, err := p.listPage(ctx, spec.PageToken, size)
	if err != nil {
		return agentidentity.ListPage{}, err
	}
	out := agentidentity.ListPage{NextPageToken: page.NextLink}
	for _, object := range page.Value {
		if object.managed() && object.BlueprintID == p.cfg.BlueprintAppID && guidPattern.MatchString(object.ID) {
			out.Objects = append(out.Objects, agentidentity.Resolved{Ref: p.ref(object), Enabled: object.AccountEnabled})
		}
	}
	return out, nil
}

// agentClientID answers the client id of an agent identity this blueprint
// owns: from the LRU, else from Graph. A disabled or unowned identity is
// refused here, before any token request. The LRU holds only identities
// last read owned and enabled (see agentCache), so a hit is safe: an
// identity this process saw disabled is never there, and one disabled out
// of band since is refused by Entra (AADSTS7000112), which evicts it.
func (p *Provider) agentClientID(ctx context.Context, objectID string) (string, error) {
	if appID, ok := p.agents.appID(objectID); ok {
		return appID, nil
	}
	object, err := p.getAgent(ctx, objectID)
	if errors.Is(err, errGraphNotFound) {
		return "", agentidentity.ErrIdentityNotFound
	}
	if err != nil {
		return "", err
	}
	if err := p.owned(object); err != nil {
		return "", err
	}
	if !object.AccountEnabled {
		return "", ErrIdentityDisabled
	}
	if !guidPattern.MatchString(object.clientID()) {
		return "", ErrUnavailable
	}
	p.agents.put(object.ID, object.clientID())
	return object.clientID(), nil
}

// checkScopes refuses, before any Entra call, a scope outside
// allowed_resource_scopes or outside the session's audience.
func (p *Provider) checkScopes(spec agentidentity.AssumeSpec) error {
	if len(spec.Scopes) == 0 || spec.Audience == "" {
		return ErrScopeNotAllowed
	}
	for _, scope := range spec.Scopes {
		resource, ok := resourceOf(scope)
		if !ok || resource != strings.TrimSuffix(spec.Audience, "/") || !slices.Contains(p.cfg.AllowedResourceScopes, scope) {
			return ErrScopeNotAllowed
		}
	}
	return nil
}

// session reads the session and derives the client-request-id of every
// token request of this call.
func (p *Provider) session(ctx context.Context, spec agentidentity.AssumeSpec) (agentidentity.AgentSession, context.Context, error) {
	if spec.SessionID == "" {
		return agentidentity.AgentSession{}, ctx, ErrInvalidRequest
	}
	session, err := p.cfg.Sessions.SessionByID(ctx, spec.SessionID)
	if err != nil {
		return agentidentity.AgentSession{}, ctx, fmt.Errorf("%w: read the agent session", ErrUnavailable)
	}
	if session.ID != spec.SessionID || session.ExecutionID == "" {
		return agentidentity.AgentSession{}, ctx, ErrInvalidRequest
	}
	return session, withCorrelation(ctx, obo.CorrelationID(session.ExecutionID, session.Generation)), nil
}

// AssumeApp answers an app-only resource token issued to the agent
// identity.
func (p *Provider) AssumeApp(ctx context.Context, spec agentidentity.AssumeSpec) (agentidentity.SessionCredential, error) {
	if err := p.checkRef(spec.Ref); err != nil {
		return agentidentity.SessionCredential{}, err
	}
	if err := p.checkConfigKey(spec.ProviderConfigKey); err != nil {
		return agentidentity.SessionCredential{}, err
	}
	if err := p.checkScopes(spec); err != nil {
		return agentidentity.SessionCredential{}, err
	}
	// The client credentials grant takes exactly one <resource>/.default.
	if len(spec.Scopes) != 1 || !strings.HasSuffix(spec.Scopes[0], defaultScopeSuffix) {
		return agentidentity.SessionCredential{}, ErrScopeNotAllowed
	}
	_, ctx, err := p.session(ctx, spec)
	if err != nil {
		return agentidentity.SessionCredential{}, err
	}
	appID, err := p.agentClientID(ctx, spec.Ref.ID)
	if err != nil {
		return agentidentity.SessionCredential{}, err
	}
	token, err := p.agentToken(ctx, appID, spec.Scopes)
	if err != nil {
		return agentidentity.SessionCredential{}, err
	}
	claims, err := p.validate(token.token, appID, "app")
	if err != nil {
		return agentidentity.SessionCredential{}, err
	}
	p.logIssued(ctx, "app", spec.Ref.ID, claims, token.requestID, token.correlationID)
	agent := agentidentity.Principal{Kind: "agent", Ref: spec.Ref.ID}
	return agentidentity.SessionCredential{
		Token: token.token, TokenType: "Bearer", ExpiresAt: p.expiry(token.expiresAt, spec.TTL),
		Actor: agentidentity.NormalizedActor{Current: agent, Chain: []agentidentity.Principal{agent},
			Fidelity: agentidentity.FidelityConfiguredAct},
	}, nil
}

// AssumeOnBehalfOf answers the Agent ID OBO token of the agent acting for a
// verified user (#1117). Asserted evidence, and verified evidence whose
// audience is not the blueprint, are refused before any Entra call.
func (p *Provider) AssumeOnBehalfOf(ctx context.Context, spec agentidentity.OBOSpec) (agentidentity.SessionCredential, error) {
	var evidence agentidentity.VerifiedUserEvidence
	switch e := spec.Evidence.(type) {
	case agentidentity.VerifiedUserEvidence:
		evidence = e
	case *agentidentity.VerifiedUserEvidence:
		if e == nil {
			return agentidentity.SessionCredential{}, ErrInvalidRequest
		}
		evidence = *e
	case agentidentity.AssertedUserEvidence, *agentidentity.AssertedUserEvidence:
		return agentidentity.SessionCredential{}, ErrAssertedUserRefused
	default:
		return agentidentity.SessionCredential{}, ErrInvalidRequest
	}
	if err := p.checkRef(spec.Ref); err != nil {
		return agentidentity.SessionCredential{}, err
	}
	if err := p.checkConfigKey(spec.ProviderConfigKey); err != nil {
		return agentidentity.SessionCredential{}, err
	}
	if !slices.Contains(p.audiences, evidence.Audience) {
		return agentidentity.SessionCredential{}, ErrAudienceMismatch
	}
	if evidence.TenantID != p.cfg.TenantID || evidence.DelegationID == "" {
		return agentidentity.SessionCredential{}, ErrInvalidRequest
	}
	if err := p.checkScopes(spec.AssumeSpec); err != nil {
		return agentidentity.SessionCredential{}, err
	}
	if p.cfg.Delegations == nil {
		return agentidentity.SessionCredential{}, ErrDelegationUnavailable
	}
	session, ctx, err := p.session(ctx, spec.AssumeSpec)
	if err != nil {
		return agentidentity.SessionCredential{}, err
	}
	if session.DelegationID != evidence.DelegationID {
		return agentidentity.SessionCredential{}, ErrDelegationUnavailable
	}
	assertion, _, err := p.cfg.Delegations.Load(ctx, delegationstore.LoadRequest{
		DelegationID: evidence.DelegationID, TenantProjectID: session.TenantProjectID, AgentBindingID: session.AgentBindingID,
	})
	switch {
	case errors.Is(err, delegationstore.ErrDelegationNotUsable), errors.Is(err, delegationstore.ErrDelegationNotFound):
		return agentidentity.SessionCredential{}, interaction("interaction_required", ErrInteractionRequired)
	case err != nil:
		return agentidentity.SessionCredential{}, ErrUnavailable
	}
	if assertion.Audience != evidence.Audience || assertion.ObjectID != evidence.ObjectID || assertion.TenantID != evidence.TenantID {
		return agentidentity.SessionCredential{}, ErrAudienceMismatch
	}
	appID, err := p.agentClientID(ctx, spec.Ref.ID)
	if err != nil {
		return agentidentity.SessionCredential{}, err
	}
	failure := &t1Failure{}
	token, err := p.obo.Exchange(context.WithValue(ctx, t1ErrorKey{}, failure), assertion, obo.ExchangeRequest{
		DelegationID: evidence.DelegationID, Scopes: spec.Scopes, AgentID: appID,
		ExecutionID: session.ExecutionID, Generation: session.Generation,
	})
	if err != nil {
		return agentidentity.SessionCredential{}, oboError(err, failure.err)
	}
	claims, err := p.validate(token.Token, appID, "user")
	if err != nil {
		return agentidentity.SessionCredential{}, err
	}
	if claims.ObjectID != evidence.ObjectID {
		return agentidentity.SessionCredential{}, ErrTokenValidation
	}
	p.logIssued(ctx, "user", spec.Ref.ID, claims, "", correlationFrom(ctx))
	user := agentidentity.Principal{Kind: "user", Ref: evidence.ObjectID}
	agent := agentidentity.Principal{Kind: "agent", Ref: spec.Ref.ID}
	return agentidentity.SessionCredential{
		Token: token.Token, TokenType: "Bearer", ExpiresAt: p.expiry(token.ExpiresAt, spec.TTL),
		Actor: agentidentity.NormalizedActor{Subject: &user, Current: agent, Chain: []agentidentity.Principal{agent},
			Fidelity: agentidentity.FidelityConfiguredAct},
	}, nil
}

// oboError maps an obo package error. source is what the assertion source
// failed with, if it did.
func oboError(err, source error) error {
	var challenge *obo.ErrClaimsChallenge
	switch {
	case errors.Is(err, obo.ErrAgentAssertionUnavailable) && source != nil:
		return source
	case errors.As(err, &challenge):
		return interaction("claims_challenge", ErrClaimsChallenge)
	case errors.Is(err, obo.ErrInteractionRequired):
		return interaction("interaction_required", ErrInteractionRequired)
	case errors.Is(err, obo.ErrAudienceMismatch):
		return ErrAudienceMismatch
	case errors.Is(err, obo.ErrAgentConsentNotGranted):
		return ErrConsentNotGranted
	case errors.Is(err, obo.ErrUnavailable), errors.Is(err, obo.ErrAgentAssertionUnavailable):
		return ErrUnavailable
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	}
	return ErrRejected
}

// validate checks an issued token names this tenant, the agent as client,
// the blueprint as parent and the expected token type.
func (p *Provider) validate(token, agentAppID, idtyp string) (tokenClaims, error) {
	claims, ok := unverifiedClaims(token)
	if !ok || claims.TenantID != p.cfg.TenantID || claims.clientID() != agentAppID ||
		claims.ParentAZP != p.cfg.BlueprintAppID || claims.IDType != idtyp {
		return tokenClaims{}, ErrTokenValidation
	}
	return claims, nil
}

func (p *Provider) expiry(issuedExpiry time.Time, ttl time.Duration) time.Time {
	if ttl > 0 {
		if bound := p.now().Add(ttl).UTC(); bound.Before(issuedExpiry) {
			return bound
		}
	}
	return issuedExpiry
}

func (p *Provider) logIssued(ctx context.Context, idtyp, objectID string, claims tokenClaims, requestID, correlationID string) {
	p.logger.LogAttrs(ctx, slog.LevelInfo, "entra agent token issued", slog.String("idtyp", idtyp),
		slog.String("agent_object_id", objectID), slog.String("uti", claims.UTI),
		slog.String("request_id", requestID), slog.String("client_request_id", correlationID))
}

// AssertionSource is the obo.AgentAssertionSource for #1117 AgentOBO: T1
// for an agent identity of this blueprint and tenant. It is the only path
// by which T1 leaves this package, and its one caller is the obo package.
func (p *Provider) AssertionSource() obo.AgentAssertionSource { return assertionSource{p: p} }

type assertionSource struct{ p *Provider }

// AgentAssertion answers T1 only for this tenant and for an agent identity
// this blueprint owns and Elitea tagged.
func (s assertionSource) AgentAssertion(ctx context.Context, tenantID, agentID string) (string, error) {
	t1, err := s.p.assertionFor(ctx, tenantID, agentID)
	if err != nil {
		if failure, ok := ctx.Value(t1ErrorKey{}).(*t1Failure); ok {
			failure.err = err
		}
		return "", err
	}
	return t1, nil
}

func (p *Provider) assertionFor(ctx context.Context, tenantID, agentID string) (string, error) {
	if tenantID != p.cfg.TenantID || !guidPattern.MatchString(agentID) {
		return "", ErrInvalidRequest
	}
	if !p.agents.knownApp(agentID) {
		objects, err := p.findAgents(ctx, 1, "appId eq "+odataString(agentID))
		if err != nil {
			return "", err
		}
		if len(objects) != 1 || p.owned(objects[0]) != nil {
			return "", agentidentity.ErrIdentityNotFound
		}
		if !objects[0].AccountEnabled {
			return "", ErrIdentityDisabled
		}
		p.agents.put(objects[0].ID, objects[0].clientID())
	}
	return p.t1(ctx, agentID)
}

var _ agentidentity.Provider = (*Provider)(nil)
