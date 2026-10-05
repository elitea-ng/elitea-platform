// Package identityproviders owns the authored definition of how this
// deployment federates a login.
//
// # What a provider is here
//
// One row of `elitea_auth.identity_providers` (shared migration 0095) is one
// TYPED PROVIDER REVISION: the whole document a login path needs for one
// protocol, plus a reference to the one secret that document must not carry.
// The configuration provenance specification requires this shape by name — "one
// typed OIDC provider revision owns all values; insecure defaults fail
// validation", and the same sentence for SAML — because pylon spreads response
// type, scopes, endpoints, client authentication and verification behaviour
// across `auth_oidc/routes/login.py` at three separate call sites, and a flat
// settings document reproduces that scattering rather than removing it.
//
// # What is deliberately NOT authorable
//
// A configuration surface is where an operator gets to choose values the server
// later acts on. Some of those choices have exactly one safe answer, and
// offering them as fields is how a deployment ends up insecure by
// configuration rather than by defect:
//
//   - The OIDC response type is `code`. The implicit and hybrid flows return
//     tokens through the browser's address bar; there is no field for them.
//   - PKCE is S256 and mandatory. There is no "enable PKCE" switch, and no
//     `plain` challenge method.
//   - A SAML assertion must be signed and its signature must use SHA-256 or
//     better. There is no `want_assertions_signed` field to turn off, and no
//     way to author SHA-1.
//   - Audience, Destination and `InResponseTo` are checked on every SAML
//     response against values derived from the document. They are not
//     separately relaxable.
//
// Validate refuses what is authorable but unsafe — a plaintext issuer, an
// unparseable certificate, a scope set with no `openid` — and everything else
// is fixed in the verifier, where an operator cannot reach it.
//
// # Secrets are references
//
// `SecretRef` names an entry in the global vault's hidden bucket. The plaintext
// never enters this package: the admin write path seals it, and the login path
// asks the vault for it by name. See `internal/api/v2/secrets/admin_hidden.go`
// for why the HIDDEN bucket specifically.
package identityproviders

import (
	"errors"
	"strings"
	"time"
)

// Kind selects the protocol and, with it, the shape of a provider's document.
type Kind string

const (
	KindOIDC Kind = "oidc"
	KindSAML Kind = "saml"
)

// ErrUnknownKind reports a kind no login path in this service implements.
//
// It is returned rather than defaulted. A definition of an unknown kind is a
// definition nothing can honour, and silently treating it as OIDC would federate
// logins through a protocol the operator did not choose.
var ErrUnknownKind = errors.New("identityproviders: unknown provider kind")

// ErrNotFound reports that no row carries the key.
var ErrNotFound = errors.New("identityproviders: no such identity provider")

// ErrNoPool reports that the store was built without a database pool. It is a
// composition failure, not a request failure, and callers answer 503 rather
// than treating it as "no providers are configured" — the second reading turns
// a broken deployment into a silently unfederated one.
var ErrNoPool = errors.New("identityproviders: no database pool")

// Provider is one authored definition.
//
// Exactly one of OIDC and SAML is set, and which one is decided by Kind.
// Validate clears the other, so a row cannot carry a document its kind does not
// name — a reader that switched on Kind would otherwise be one refactor away
// from reading a stale document of the other protocol.
type Provider struct {
	Key         string
	Kind        Kind
	DisplayName string
	Enabled     bool
	Revision    int
	SecretRef   string
	OIDC        *OIDCDocument
	SAML        *SAMLDocument
	UpdatedAt   time.Time
}

// OIDCDocument is the complete OpenID Connect definition.
//
// Every field is non-secret. The client secret is in the vault, named by
// Provider.SecretRef.
type OIDCDocument struct {
	// Issuer is the identity provider's issuer identifier. Discovery is
	// performed against `<issuer>/.well-known/openid-configuration`, so this is
	// the issuer and NOT the discovery document's own URL. pylon's field is
	// named `metadata_endpoint` and holds the latter; the admin surface accepts
	// either spelling and stores the issuer, because the issuer is what an
	// id_token's `iss` claim is compared against.
	Issuer string `json:"issuer"`

	// ClientID is the client identifier registered with the provider.
	ClientID string `json:"client_id"`

	// RedirectURI is the callback this deployment serves. It must be one the
	// provider has registered, and it is echoed in the token exchange, so a
	// mismatch fails at the provider rather than here.
	RedirectURI string `json:"redirect_uri"`

	// Scopes always contains `openid`. Validate adds it if the operator left it
	// out, because a request without it is not an OpenID Connect request at all
	// and the provider would return no id_token.
	Scopes []string `json:"scopes"`

	// RequireEmailVerified decides whether an ABSENT `email_verified` claim
	// stops a first login. An explicit `false` is always refused, whatever this
	// says. Many providers omit the claim, so requiring it is a deployment
	// choice and not a default.
	RequireEmailVerified bool `json:"require_email_verified"`

	// LoginDomains are the email domains whose users sign in through this
	// provider. See SAMLDocument.LoginDomains.
	LoginDomains []string `json:"login_domains,omitempty"`

	// AdoptSCIMUsers lets this provider's FIRST login adopt an account a SCIM
	// client provisioned. See SAMLDocument.AdoptSCIMUsers.
	AdoptSCIMUsers bool `json:"adopt_scim_users"`
}

// Two fields that a reader might expect here are absent on purpose, because
// nothing in this service would read them:
//
//   - AN AUTHORIZATION TRANSPORT. pylon offers a POST binding to the
//     authorization endpoint through a self-submitting form. This service
//     performs the GET redirect binding, which OpenID Connect Core requires
//     every provider to support; the form belongs to the browserauth plane,
//     which is not mounted. A `get`/`post` field here would be a control with
//     one working setting.
//   - A CLOCK SKEW. The ID token verifier this service uses performs its own
//     expiry, issued-at and not-before checks and exposes no tolerance to
//     configure. A skew field would be collected, stored, and never consulted.
//     SAMLDocument DOES carry one, because this service verifies those
//     conditions itself and applies it.

// SAMLDocument is the complete SAML 2.0 service-provider definition.
//
// IDPCertificates are the identity provider's PUBLIC signing certificates, so
// they belong in this document. The service provider's PRIVATE key does not,
// and is named by Provider.SecretRef.
type SAMLDocument struct {
	// IDPEntityID is the identity provider's entity identifier. A response
	// whose Issuer is not this value is refused.
	IDPEntityID string `json:"idp_entity_id"`

	// IDPSSOURL is where an authentication request is sent.
	IDPSSOURL string `json:"idp_sso_url"`

	// IDPSLOURL is the single-logout endpoint. Empty means this provider does
	// not federate logout, and the local session is simply cleared.
	IDPSLOURL string `json:"idp_slo_url"`

	// IDPCertificates are PEM-encoded X.509 certificates whose public keys may
	// have signed an assertion. More than one is normal during a key rollover.
	// Validate parses every one; an unparseable certificate is refused rather
	// than skipped, because a definition that silently keeps only some of its
	// trust anchors fails at the worst moment.
	IDPCertificates []string `json:"idp_certificates"`

	// SPEntityID is this deployment's entity identifier. It is the value an
	// assertion's AudienceRestriction must name.
	SPEntityID string `json:"sp_entity_id"`

	// ACSURL is this deployment's assertion consumer service. It is the value a
	// response's Destination must name.
	ACSURL string `json:"acs_url"`

	// NameIDFormat is requested in the authentication request. Empty requests
	// no particular format and accepts what the provider sends.
	NameIDFormat string `json:"name_id_format"`

	// EmailAttribute and NameAttribute name the assertion attributes carrying
	// the person's address and display name. Empty falls back to the common
	// SAML 2.0 attribute names and to the NameID when it is an email address.
	EmailAttribute string `json:"email_attribute"`
	NameAttribute  string `json:"name_attribute"`

	// SignAuthnRequests turns on signing of the outbound authentication
	// request. It requires a sealed service-provider key, which Validate does
	// not check because the key is not in this document — the admin write path
	// checks it, where the vault is in reach.
	SignAuthnRequests bool `json:"sign_authn_requests"`

	// SPCertificate is the PUBLIC certificate matching the sealed private key.
	// It is published in this deployment's service-provider metadata so the
	// identity provider can verify a signed request.
	SPCertificate string `json:"sp_certificate"`

	// ClockSkewSeconds is the tolerance applied to assertion time conditions.
	ClockSkewSeconds int `json:"clock_skew_seconds"`

	// LoginDomains are the email domains whose users sign in through this
	// provider. The sign-in page reads them for home-realm discovery: a person
	// types a work address, and the page sends them to the provider that lists
	// its domain. Empty means the provider is reached only by its own button.
	//
	// The list is ROUTING, not authorisation. A person whose address is not
	// listed can still press the provider's button, and the provider decides.
	// Validate stores each entry lower-cased and refuses one that is not a
	// host name.
	LoginDomains []string `json:"login_domains,omitempty"`

	// AdoptSCIMUsers lets this provider's FIRST login adopt an account a SCIM
	// client provisioned (one with an `elitea_auth.scim_users` record and no
	// federated link yet). It is OFF by default.
	//
	// A first login joins an existing account by the asserted address
	// (internal/api/v2/auth/oidc.go joinAccountByEmail). A SAML assertion
	// states nothing about whether that address is verified, and an OIDC
	// provider may omit `email_verified`. So without this flag ANY configured
	// provider — a GitHub connector behind Dex, a social login — that asserts a
	// directory-provisioned address would take that person's account. The
	// operator turns it on for the provider PAIRED with the SCIM client (Entra
	// ID's SAML or OIDC app for Entra's provisioning), and leaves it off for
	// every other.
	AdoptSCIMUsers bool `json:"adopt_scim_users"`
}

// MaxClockSkewSeconds bounds the tolerance either kind may author.
//
// Five minutes is the widest value with a defensible reason: it covers hosts
// whose clocks drift between NTP corrections. Beyond it, the tolerance stops
// being a correction for drift and becomes an extension of a credential's
// lifetime.
const MaxClockSkewSeconds = 300

// DefaultClockSkewSeconds applies when the operator authored none.
const DefaultClockSkewSeconds = 60

// ParseKind resolves the stored string.
func ParseKind(raw string) (Kind, error) {
	switch Kind(strings.ToLower(strings.TrimSpace(raw))) {
	case KindOIDC:
		return KindOIDC, nil
	case KindSAML:
		return KindSAML, nil
	default:
		return "", ErrUnknownKind
	}
}

// NormalizeKey reduces an operator's spelling to the stored slug.
//
// It is applied on BOTH sides of every lookup — on write and on read — so a
// caller cannot store `Corporate Okta` and then fail to find it by
// `corporate_okta`. Lower case, and any run of characters outside [a-z0-9]
// collapses to one underscore.
func NormalizeKey(raw string) string {
	var builder strings.Builder
	lastUnderscore := false
	for _, character := range strings.ToLower(strings.TrimSpace(raw)) {
		switch {
		case character >= 'a' && character <= 'z', character >= '0' && character <= '9':
			builder.WriteRune(character)
			lastUnderscore = false
		default:
			if !lastUnderscore && builder.Len() > 0 {
				builder.WriteByte('_')
				lastUnderscore = true
			}
		}
	}
	return strings.Trim(builder.String(), "_")
}

// ClockSkew returns the SAML tolerance as a duration.
//
// Only SAML has one. See the note under OIDCDocument for why OIDC does not.
func (p Provider) ClockSkew() time.Duration {
	seconds := DefaultClockSkewSeconds
	if p.SAML != nil && p.SAML.ClockSkewSeconds > 0 {
		seconds = p.SAML.ClockSkewSeconds
	}
	return time.Duration(seconds) * time.Second
}
