package oidc

// Entra mode (ADR-0039 decision 4, issue #1117).
//
// A stored OIDC provider marked `entra: true` is a Microsoft Entra ID
// application. With credential governance on, its sign-in is held to the
// Entra rules here; with the flag off the marker is ignored and nothing in
// this file runs.
//
//   - The issuer is the v2 issuer of one tenant,
//     <authority>/<tid>/v2.0, and that tid must be in the provider's
//     allow-list. "common", "organizations" and "consumers" are never an
//     accepted token issuer.
//   - `tid` and `oid` are required. The governed identity is (tid, oid),
//     never upn, email or preferred_username.
//   - The access token retained as a delegation assertion must be signed,
//     unexpired, issued by an allowed tenant, carry delegated scopes (`scp`)
//     and name the middle tier as `aud`. An ID token (no `scp`), an app-only
//     token (`idtyp=app`, no `scp`) and a Graph token (`aud` is Graph) are
//     refused.

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	coreoidc "github.com/coreos/go-oidc/v3/oidc"
)

// EntraDefaultAuthority is the public-cloud Entra authority.
const EntraDefaultAuthority = "https://login.microsoftonline.com"

// EntraClientCapabilities is the `claims` request parameter that declares
// the cp1 client capability, so Entra issues CAE-capable tokens and answers
// with claims challenges instead of silent revocation.
const EntraClientCapabilities = `{"access_token":{"xms_cc":{"values":["cp1"]}}}`

const (
	maxEntraTenants    = 64
	maxEntraAssertion  = 16 << 10
	minAssertionTTL    = time.Minute
	entraIssuerSuffix  = "/v2.0"
	entraMultiTenantID = "{tenantid}"
)

var entraGUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

var (
	// ErrEntraTenantNotAllowed refuses a token from a tenant outside the
	// allow-list, or from a multi-tenant alias issuer.
	ErrEntraTenantNotAllowed = fmt.Errorf("%w: Entra tenant not allowed", ErrAssertionRejected)
	// ErrEntraIdentityClaims refuses a token without a usable tid or oid.
	ErrEntraIdentityClaims = fmt.Errorf("%w: Entra tid and oid are required", ErrAssertionRejected)
	// ErrEntraNotAnAssertion refuses a token that is not a delegated
	// middle-tier access token: an ID token, an app-only token or a token
	// for another resource.
	ErrEntraNotAnAssertion = fmt.Errorf("%w: not a middle-tier delegated access token", ErrAssertionRejected)
)

// EntraPolicy is the Entra mode of one stored provider.
type EntraPolicy struct {
	authority string
	tenants   []string
	clientID  string
	apiScope  string
	audiences []string
}

// NewEntraPolicy builds the policy. issuer is the provider's configured
// issuer; its scheme and host are the authority. apiScope is the middle-tier
// API scope; empty means api://<clientID>/.default.
func NewEntraPolicy(issuer, clientID string, tenants []string, apiScope string) (EntraPolicy, error) {
	parsed, err := url.Parse(issuer)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
		!validConfigurationText(clientID) || len(tenants) == 0 || len(tenants) > maxEntraTenants {
		return EntraPolicy{}, ErrInvalidConfiguration
	}
	for _, tid := range tenants {
		if !entraGUID.MatchString(tid) {
			return EntraPolicy{}, ErrInvalidConfiguration
		}
	}
	if apiScope == "" {
		apiScope = "api://" + clientID + "/.default"
	}
	if !validConfigurationText(apiScope) || strings.ContainsAny(apiScope, " \t") {
		return EntraPolicy{}, ErrInvalidConfiguration
	}
	return EntraPolicy{
		authority: parsed.Scheme + "://" + parsed.Host,
		tenants:   slices.Clone(tenants),
		clientID:  clientID,
		apiScope:  apiScope,
		audiences: []string{clientID, "api://" + clientID},
	}, nil
}

// Authority is <scheme>://<host> of the configured issuer.
func (p EntraPolicy) Authority() string { return p.authority }

// ClientID is the middle-tier application id.
func (p EntraPolicy) ClientID() string { return p.clientID }

// APIScope is the middle-tier API scope requested at sign-in.
func (p EntraPolicy) APIScope() string { return p.apiScope }

// AllowedTenants is the tenant allow-list.
func (p EntraPolicy) AllowedTenants() []string { return slices.Clone(p.tenants) }

// AssertionAudiences are the accepted middle-tier assertion audiences.
func (p EntraPolicy) AssertionAudiences() []string { return slices.Clone(p.audiences) }

// IssuerFor is the v2 issuer of tid.
func (p EntraPolicy) IssuerFor(tid string) string {
	return p.authority + "/" + tid + entraIssuerSuffix
}

// IssuerTemplate is the issuer Entra's multi-tenant discovery documents
// publish. It is used only to accept that discovery document; no token with
// it as `iss` is accepted.
func (p EntraPolicy) IssuerTemplate() string {
	return p.authority + "/" + entraMultiTenantID + entraIssuerSuffix
}

// EntraIdentity is the governed key of a user.
type EntraIdentity struct {
	TenantID string
	ObjectID string
}

// EntraClaims are the claims the Entra checks read.
type EntraClaims struct {
	Issuer   string `json:"iss"`
	TenantID string `json:"tid"`
	ObjectID string `json:"oid"`
	Scope    string `json:"scp"`
	IDType   string `json:"idtyp"`
}

// VerifyIdentity checks iss, tid and oid of a signature-verified token.
func (p EntraPolicy) VerifyIdentity(claims EntraClaims) (EntraIdentity, error) {
	if !entraGUID.MatchString(claims.TenantID) || !entraGUID.MatchString(claims.ObjectID) {
		return EntraIdentity{}, ErrEntraIdentityClaims
	}
	if !slices.Contains(p.tenants, claims.TenantID) || claims.Issuer != p.IssuerFor(claims.TenantID) {
		return EntraIdentity{}, ErrEntraTenantNotAllowed
	}
	return EntraIdentity{TenantID: claims.TenantID, ObjectID: claims.ObjectID}, nil
}

// VerifiedEntraAssertion is a middle-tier access token that passed every
// check. Token is the raw value for custody only.
type VerifiedEntraAssertion struct {
	Token     string
	Identity  EntraIdentity
	Issuer    string
	Audience  string
	Scopes    []string
	ExpiresAt time.Time
}

// String keeps the token out of %v and %s.
func (a VerifiedEntraAssertion) String() string {
	return fmt.Sprintf("oidc.VerifiedEntraAssertion{tid=%s oid=%s aud=%s token=[redacted]}",
		a.Identity.TenantID, a.Identity.ObjectID, a.Audience)
}

// GoString keeps the token out of %#v.
func (a VerifiedEntraAssertion) GoString() string { return a.String() }

// EntraAssertionVerifier validates access tokens as OBO assertions.
type EntraAssertionVerifier struct {
	policy EntraPolicy
	keySet coreoidc.KeySet
	now    func() time.Time
}

// NewEntraAssertionVerifier builds the verifier over the provider's key set.
func NewEntraAssertionVerifier(policy EntraPolicy, keySet coreoidc.KeySet, now func() time.Time) (*EntraAssertionVerifier, error) {
	if policy.clientID == "" || keySet == nil {
		return nil, ErrInvalidConfiguration
	}
	if now == nil {
		now = time.Now
	}
	return &EntraAssertionVerifier{policy: policy, keySet: keySet, now: now}, nil
}

// Verify checks signature, exp, iss, tid, oid, aud and delegated scopes.
func (v *EntraAssertionVerifier) Verify(ctx context.Context, raw string) (VerifiedEntraAssertion, error) {
	if raw == "" || len(raw) > maxEntraAssertion {
		return VerifiedEntraAssertion{}, ErrEntraNotAnAssertion
	}
	verifier := coreoidc.NewVerifier("", v.keySet, &coreoidc.Config{
		SkipClientIDCheck:    true,
		SkipIssuerCheck:      true,
		SupportedSigningAlgs: []string{"RS256"},
		Now:                  v.now,
	})
	token, err := verifier.Verify(ctx, raw)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return VerifiedEntraAssertion{}, ctxErr
		}
		if isProviderUnavailable(err) {
			return VerifiedEntraAssertion{}, ErrProviderUnavailable
		}
		return VerifiedEntraAssertion{}, ErrAssertionRejected
	}
	var claims EntraClaims
	if err := token.Claims(&claims); err != nil {
		return VerifiedEntraAssertion{}, ErrAssertionRejected
	}
	identity, err := v.policy.VerifyIdentity(claims)
	if err != nil {
		return VerifiedEntraAssertion{}, err
	}
	scopes := strings.Fields(claims.Scope)
	if len(scopes) == 0 || claims.IDType == "app" || len(token.Audience) != 1 ||
		!slices.Contains(v.policy.audiences, token.Audience[0]) {
		return VerifiedEntraAssertion{}, ErrEntraNotAnAssertion
	}
	if token.Expiry.Before(v.now().Add(minAssertionTTL)) {
		return VerifiedEntraAssertion{}, ErrAssertionRejected
	}
	return VerifiedEntraAssertion{
		Token:     raw,
		Identity:  identity,
		Issuer:    claims.Issuer,
		Audience:  token.Audience[0],
		Scopes:    scopes,
		ExpiresAt: token.Expiry.UTC(),
	}, nil
}

// IsEntraRejection reports whether err is one of the Entra refusals.
func IsEntraRejection(err error) bool {
	return errors.Is(err, ErrEntraTenantNotAllowed) || errors.Is(err, ErrEntraIdentityClaims) ||
		errors.Is(err, ErrEntraNotAnAssertion)
}
