package provideroauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/authzcompose"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// DefaultStateTTL is how long a consent start stays redeemable. A test
// value, not a customer requirement (gate D5).
const DefaultStateTTL = 10 * time.Minute

const (
	maxStateBytes = 128
	maxCodeBytes  = 4096
)

var (
	// ErrTargetNotFound is a target binding that does not exist in the
	// tenant project, or is for another provider.
	ErrTargetNotFound = errors.New("provideroauth: no such target binding for the provider in the project")
	// ErrBindingNotUsable is an agent binding that is not active in the
	// tenant project.
	ErrBindingNotUsable = errors.New("provideroauth: the agent binding is not active in the project")
	// ErrStateInvalid is an unknown, expired, used or foreign state value.
	ErrStateInvalid = errors.New("provideroauth: the consent state is not valid")
	// ErrConsentDenied is the provider answering the callback with an error
	// (the user declined).
	ErrConsentDenied = errors.New("provideroauth: the provider did not grant consent")
)

// Consent runs the consent start and callback.
type Consent struct {
	grants    *Grants
	clients   map[string]*Client
	perTarget map[string]TargetAuthorizers
	targets   credentialgrant.TargetReader
	bindings  authzcompose.BindingGetter
	ttl       time.Duration
	now       func() time.Time
	random    func([]byte) error
}

// Authorizer is one authorization server's authorization request and code
// exchange. *Client is the authorizer of a provider with one OAuth
// application.
type Authorizer interface {
	AuthorizationURL(state, challenge string) string
	Exchange(ctx context.Context, code, verifier string) (TokenResponse, error)
}

// TargetAuthorizers serve a provider whose authorization server is the
// target's own (a remote MCP server discovered through RFC 9728, #1123).
// Begin resolves the authorizer at the start and may answer an opaque
// registration binding (a dynamically registered client); it is sealed
// with the PKCE verifier, never leaves Main, and Resume gets it back at the
// callback to rebuild the same authorizer.
type TargetAuthorizers interface {
	Provider() string
	RedirectURI() string
	Begin(ctx context.Context, target credentialgrant.TargetBinding) (authorizer Authorizer, binding string, err error)
	Resume(ctx context.Context, target credentialgrant.TargetBinding, binding string) (Authorizer, error)
}

// ConsentConfig is the consent flow's dependencies.
type ConsentConfig struct {
	Grants  *Grants
	Clients []*Client
	// PerTarget are the providers whose authorization server is per target;
	// a provider in both is configured twice.
	PerTarget []TargetAuthorizers
	Targets   credentialgrant.TargetReader
	Bindings  authzcompose.BindingGetter
	// StateTTL defaults to DefaultStateTTL.
	StateTTL time.Duration
}

// NewConsent builds the consent flow.
func NewConsent(cfg ConsentConfig) (*Consent, error) {
	if cfg.Grants == nil || cfg.Targets == nil || cfg.Bindings == nil {
		return nil, errors.New("provideroauth: grants, target and binding readers are required")
	}
	clients := make(map[string]*Client, len(cfg.Clients))
	for _, c := range cfg.Clients {
		if c == nil {
			return nil, errors.New("provideroauth: a nil provider client")
		}
		if _, dup := clients[c.Provider()]; dup {
			return nil, fmt.Errorf("provideroauth: provider %q is configured twice", c.Provider())
		}
		clients[c.Provider()] = c
	}
	perTarget := make(map[string]TargetAuthorizers, len(cfg.PerTarget))
	for _, p := range cfg.PerTarget {
		if p == nil {
			return nil, errors.New("provideroauth: a nil per-target authorizer")
		}
		_, dup := clients[p.Provider()]
		if _, twice := perTarget[p.Provider()]; dup || twice {
			return nil, fmt.Errorf("provideroauth: provider %q is configured twice", p.Provider())
		}
		if err := redirectURI(p.RedirectURI(), p.Provider()); err != nil {
			return nil, fmt.Errorf("provideroauth: provider %q redirect URI: %w", p.Provider(), err)
		}
		perTarget[p.Provider()] = p
	}
	ttl := cfg.StateTTL
	if ttl <= 0 {
		ttl = DefaultStateTTL
	}
	return &Consent{
		grants: cfg.Grants, clients: clients, perTarget: perTarget, targets: cfg.Targets, bindings: cfg.Bindings,
		ttl: ttl, now: time.Now, random: func(b []byte) error { _, err := rand.Read(b); return err },
	}, nil
}

// Configured reports whether provider has an OAuth application.
func (c *Consent) Configured(provider string) bool {
	_, ok := c.clients[provider]
	_, perTarget := c.perTarget[provider]
	return ok || perTarget
}

// redirectFor is the provider's exact redirect URI; false when it has no
// OAuth application.
func (c *Consent) redirectFor(provider string) (string, bool) {
	if client, ok := c.clients[provider]; ok {
		return client.RedirectURI(), true
	}
	if p, ok := c.perTarget[provider]; ok {
		return p.RedirectURI(), true
	}
	return "", false
}

// verifierBindingSeparator joins the PKCE verifier (base64url, which never
// holds it) and a per-target registration binding in the sealed value.
const verifierBindingSeparator = "\n"

// StartRequest is one consent start. UserID is the authenticated caller.
type StartRequest struct {
	UserID          int64
	Provider        string
	TenantProjectID int64
	AgentBindingID  string
	TargetBindingID string
}

// StartResult is where to send the user.
type StartResult struct {
	AuthorizationURL string
	ExpiresAt        time.Time
}

// Start checks the target and agent binding, stores the state server side
// and answers the provider authorization URL.
func (c *Consent) Start(ctx context.Context, r StartRequest) (StartResult, error) {
	if r.UserID <= 0 || r.TenantProjectID <= 0 || !validUUID(r.AgentBindingID) || !validUUID(r.TargetBindingID) ||
		!providerPattern.MatchString(r.Provider) {
		return StartResult{}, ErrInvalid
	}
	redirect, ok := c.redirectFor(r.Provider)
	if !ok {
		return StartResult{}, ErrProviderNotConfigured
	}
	target, err := c.checkSubjects(ctx, r.TenantProjectID, r.Provider, r.TargetBindingID, r.AgentBindingID)
	if err != nil {
		return StartResult{}, err
	}
	var authorizer Authorizer
	var binding string
	if client, ok := c.clients[r.Provider]; ok {
		authorizer = client
	} else if authorizer, binding, err = c.perTarget[r.Provider].Begin(ctx, target); err != nil {
		return StartResult{}, err
	}

	state, err := c.randomToken(32)
	if err != nil {
		return StartResult{}, err
	}
	verifier, err := c.randomToken(32)
	if err != nil {
		return StartResult{}, err
	}
	challenge := sha256.Sum256([]byte(verifier))
	expires := c.now().Add(c.ttl).UTC().Truncate(time.Microsecond)
	digest := stateDigest(state)
	aad := stateAAD{
		StateDigest: digest, Provider: r.Provider, TenantProjectID: r.TenantProjectID, UserID: r.UserID,
		AgentBindingID: r.AgentBindingID, TargetBindingID: r.TargetBindingID,
		RedirectURI: redirect, ExpiresAt: expires,
	}
	plaintext := verifier
	if binding != "" {
		plaintext += verifierBindingSeparator + binding
	}
	sealed := c.grants.sealer.seal(plaintext, aad)
	// Expired states are dead weight; a start removes old ones.
	if _, err := c.grants.pool.Exec(ctx, `
DELETE FROM elitea_identity.provider_oauth_consent_states
 WHERE expires_at < clock_timestamp() - interval '1 hour'`); err != nil {
		return StartResult{}, fmt.Errorf("provideroauth: prune consent states: %w", err)
	}
	if _, err := c.grants.pool.Exec(ctx, `
INSERT INTO elitea_identity.provider_oauth_consent_states
    (state_digest, provider_type, tenant_project_id, user_id, agent_binding_id, target_binding_id,
     redirect_uri, verifier_ciphertext, key_label, expires_at)
VALUES ($1, $2, $3, $4, $5::uuid, $6::uuid, $7, $8, $9, $10)`,
		digest[:], r.Provider, r.TenantProjectID, r.UserID, r.AgentBindingID, r.TargetBindingID,
		redirect, sealed, KeyLabel, expires); err != nil {
		return StartResult{}, fmt.Errorf("provideroauth: store the consent state: %w", err)
	}
	return StartResult{
		AuthorizationURL: authorizer.AuthorizationURL(state, base64.RawURLEncoding.EncodeToString(challenge[:])),
		ExpiresAt:        expires,
	}, nil
}

// checkSubjects answers ErrTargetNotFound unless the target binding exists
// in the project for the provider, and ErrBindingNotUsable unless the agent
// binding is active there.
func (c *Consent) checkSubjects(ctx context.Context, tenantProjectID int64, provider, targetBindingID, agentBindingID string) (credentialgrant.TargetBinding, error) {
	target, err := c.targets.TargetBinding(ctx, tenantProjectID, targetBindingID)
	switch {
	case errors.Is(err, credentialgrant.ErrTargetNotFound):
		return credentialgrant.TargetBinding{}, ErrTargetNotFound
	case err != nil:
		return credentialgrant.TargetBinding{}, fmt.Errorf("provideroauth: read the target binding: %w", err)
	}
	if target.TenantProjectID != tenantProjectID || target.ProviderType != provider || target.ID != targetBindingID {
		return credentialgrant.TargetBinding{}, ErrTargetNotFound
	}
	binding, err := c.bindings.GetUsable(ctx, agentBindingID)
	switch {
	case errors.Is(err, agentidentity.ErrBindingNotFound):
		return credentialgrant.TargetBinding{}, ErrBindingNotUsable
	case err != nil:
		return credentialgrant.TargetBinding{}, fmt.Errorf("provideroauth: read the agent binding: %w", err)
	}
	if binding.TenantProjectID != tenantProjectID || binding.State != agentidentity.BindingActive {
		return credentialgrant.TargetBinding{}, ErrBindingNotUsable
	}
	return target, nil
}

func (c *Consent) randomToken(n int) (string, error) {
	b := make([]byte, n)
	defer clear(b)
	if err := c.random(b); err != nil {
		return "", fmt.Errorf("provideroauth: random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// CallbackRequest is the provider's redirect back.
type CallbackRequest struct {
	Provider string
	State    string
	Code     string
	// ProviderError is the callback's `error` parameter; non-empty means
	// the user declined.
	ProviderError string
}

// CallbackResult names the stored grant. It holds no token.
type CallbackResult struct {
	GrantID         string
	Provider        string
	TenantProjectID int64
	AgentBindingID  string
	TargetBindingID string
}

// Authorize resolves the authenticated caller in the state's tenant
// project, after the state is consumed and before any code is exchanged.
// It answers the caller's resolved user id when the caller may still
// consent there; an error refuses the callback. The state's user must be
// that user.
type Authorize func(ctx context.Context, tenantProjectID int64) (userID int64, err error)

// Callback consumes the state (single use), checks it is unexpired, for
// this provider and the caller's, re-checks the target and the agent
// binding, exchanges the code with the PKCE
// verifier and the exact redirect URI, and seals the grant with its audit
// event.
func (c *Consent) Callback(ctx context.Context, r CallbackRequest, authorize Authorize) (CallbackResult, error) {
	if authorize == nil {
		return CallbackResult{}, errors.New("provideroauth: the callback needs an authorization check")
	}
	if !providerPattern.MatchString(r.Provider) || r.State == "" || len(r.State) > maxStateBytes {
		return CallbackResult{}, ErrStateInvalid
	}
	redirect, ok := c.redirectFor(r.Provider)
	if !ok {
		return CallbackResult{}, ErrProviderNotConfigured
	}
	digest := stateDigest(r.State)
	var (
		key         GrantKey
		redirectURI string
		sealed      []byte
		expires     time.Time
	)
	// Consumed whatever happens next: a state is single use even when the
	// rest of the callback fails.
	err := c.grants.pool.QueryRow(ctx, `
UPDATE elitea_identity.provider_oauth_consent_states
   SET consumed_at = clock_timestamp()
 WHERE state_digest = $1 AND consumed_at IS NULL
RETURNING provider_type, tenant_project_id, user_id, agent_binding_id::text, target_binding_id::text,
          redirect_uri, verifier_ciphertext, expires_at`, digest[:]).
		Scan(&key.Provider, &key.TenantProjectID, &key.SubjectUserID, &key.AgentBindingID, &key.TargetBindingID,
			&redirectURI, &sealed, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return CallbackResult{}, ErrStateInvalid
	}
	if err != nil {
		return CallbackResult{}, fmt.Errorf("provideroauth: consume the consent state: %w", err)
	}
	if !c.now().Before(expires) || key.Provider != r.Provider || redirectURI != redirect {
		return CallbackResult{}, ErrStateInvalid
	}
	opened, err := c.grants.sealer.open(sealed, stateAAD{
		StateDigest: digest, Provider: key.Provider, TenantProjectID: key.TenantProjectID, UserID: key.SubjectUserID,
		AgentBindingID: key.AgentBindingID, TargetBindingID: key.TargetBindingID,
		RedirectURI: redirectURI, ExpiresAt: expires.UTC(),
	})
	if err != nil {
		return CallbackResult{}, ErrStateInvalid
	}
	verifier, binding, _ := strings.Cut(opened, verifierBindingSeparator)
	if r.ProviderError != "" {
		return CallbackResult{}, ErrConsentDenied
	}
	if r.Code == "" || len(r.Code) > maxCodeBytes {
		return CallbackResult{}, ErrStateInvalid
	}
	caller, err := authorize(ctx, key.TenantProjectID)
	if err != nil {
		return CallbackResult{}, err
	}
	if caller != key.SubjectUserID {
		// Another user's state: refused, and consumed, so the link cannot
		// be replayed by its owner either.
		return CallbackResult{}, ErrStateInvalid
	}
	// The binding may have been suspended or the target removed since the
	// start: re-checked before the code is exchanged, so no grant is stored
	// for either.
	target, err := c.checkSubjects(ctx, key.TenantProjectID, key.Provider, key.TargetBindingID, key.AgentBindingID)
	if err != nil {
		return CallbackResult{}, err
	}
	var authorizer Authorizer
	if client, ok := c.clients[key.Provider]; ok {
		authorizer = client
	} else if authorizer, err = c.perTarget[key.Provider].Resume(ctx, target, binding); err != nil {
		return CallbackResult{}, err
	}
	tokens, err := authorizer.Exchange(ctx, r.Code, verifier)
	if err != nil {
		return CallbackResult{}, err
	}
	grantID, err := c.grants.save(ctx, consented{Key: key, Tokens: tokens, RedirectURI: redirectURI})
	if err != nil {
		return CallbackResult{}, err
	}
	return CallbackResult{
		GrantID: grantID, Provider: key.Provider, TenantProjectID: key.TenantProjectID,
		AgentBindingID: key.AgentBindingID, TargetBindingID: key.TargetBindingID,
	}, nil
}

// RecordRefusal appends a denied consent event outside any write, for a
// callback refused after its state was read. Best effort for the caller:
// it answers the append error.
func (c *Consent) RecordRefusal(ctx context.Context, userID int64, provider, reason string) error {
	if userID <= 0 || !providerPattern.MatchString(provider) {
		return ErrInvalid
	}
	return pgx.BeginFunc(ctx, c.grants.pool, func(tx pgx.Tx) error {
		return c.grants.audit.Append(ctx, tx, securityaudit.SecurityEvent{
			Type: securityaudit.TypeGrantChanged, Time: c.now().UTC(), Action: "provider_oauth_grant.consent",
			Effect: securityaudit.EffectDeny, Reasons: []string{reason, "provider." + provider},
			PrincipalRef: fmt.Sprintf("user:%d", userID), ResourceType: "provider_oauth_grant",
		})
	})
}
