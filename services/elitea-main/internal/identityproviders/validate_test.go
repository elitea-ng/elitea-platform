package identityproviders

// What these tests are for.
//
// Validate is the only thing standing between an operator's typing and a
// document the login path will act on. Every case below is a value that WOULD
// have been stored and acted on without it, and the failure each one prevents
// is named in its comment rather than left as "invalid input".

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"
)

func validOIDC() Provider {
	return Provider{
		Key:         "corporate",
		Kind:        KindOIDC,
		DisplayName: "Corporate SSO",
		OIDC: &OIDCDocument{
			Issuer:      "https://idp.example.com",
			ClientID:    "elitea",
			RedirectURI: "https://elitea.example.com/auth/oidc/callback",
			Scopes:      []string{"profile", "email"},
		},
	}
}

func validSAML(t *testing.T) Provider {
	t.Helper()
	return Provider{
		Key:         "corporate_saml",
		Kind:        KindSAML,
		DisplayName: "Corporate SAML",
		SAML: &SAMLDocument{
			IDPEntityID:     "https://idp.example.com/metadata",
			IDPSSOURL:       "https://idp.example.com/sso",
			IDPCertificates: []string{rsaCertificatePEM(t, 2048)},
			SPEntityID:      "https://elitea.example.com/saml",
			ACSURL:          "https://elitea.example.com/auth/saml/acs",
		},
	}
}

/* ── the refusals that keep a login off a plaintext channel ─────────────── */

// A plaintext issuer would send the authorization request, and every redirect
// that follows it, over a channel anybody on the path can rewrite.
func TestAPlaintextIssuerIsRefused(t *testing.T) {
	provider := validOIDC()
	provider.OIDC.Issuer = "http://idp.example.com"

	_, err := Validate(provider)
	requireFieldRefusal(t, err, "issuer")
}

// The end-to-end stack runs its identity provider on `oidc.localhost` over
// plain HTTP. Refusing that would mean this code could not be exercised by the
// journeys at all, and a loopback address is not reachable from another host.
func TestAPlaintextLoopbackIssuerIsAccepted(t *testing.T) {
	provider := validOIDC()
	provider.OIDC.Issuer = "http://oidc.localhost:8080"
	provider.OIDC.RedirectURI = "http://localhost:3000/auth/oidc/callback"

	if _, err := Validate(provider); err != nil {
		t.Fatalf("a loopback issuer was refused: %v", err)
	}
}

// `localhost.example.com` is a PUBLIC name. A suffix match that did not stop at
// a label boundary would accept it and federate a login in clear text.
func TestAPublicNameEndingInLocalhostIsRefused(t *testing.T) {
	provider := validOIDC()
	provider.OIDC.Issuer = "http://localhost.example.com"

	_, err := Validate(provider)
	requireFieldRefusal(t, err, "issuer")
}

// A fragment on a redirect URI is where an implicit-flow token lands. This
// service asks for no such token, and a URI shaped to receive one is refused.
func TestARedirectURIWithAFragmentIsRefused(t *testing.T) {
	provider := validOIDC()
	provider.OIDC.RedirectURI = "https://elitea.example.com/callback#token"

	_, err := Validate(provider)
	requireFieldRefusal(t, err, "redirect_uri")
}

/* ── the corrections with exactly one possible intent ───────────────────── */

// pylon's field is `metadata_endpoint` and holds the discovery URL. An operator
// moving a definition across has that URL in hand. Storing it as the issuer
// would make every id_token fail verification against `iss`.
func TestADiscoveryURLIsReducedToItsIssuer(t *testing.T) {
	provider := validOIDC()
	provider.OIDC.Issuer = "https://idp.example.com/.well-known/openid-configuration"

	stored, err := Validate(provider)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if stored.OIDC.Issuer != "https://idp.example.com" {
		t.Fatalf("issuer is %q, want the discovery path removed", stored.OIDC.Issuer)
	}
}

// A request without `openid` is not an OpenID Connect request: the provider
// returns no id_token and the login fails with a message about a missing token
// rather than a missing scope.
func TestOpenIDIsAlwaysRequested(t *testing.T) {
	stored, err := Validate(validOIDC())
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if stored.OIDC.Scopes[0] != "openid" {
		t.Fatalf("scopes are %v, want openid first", stored.OIDC.Scopes)
	}
}

// A scope carrying a space would be sent as two scopes, one of which the
// operator never authored.
func TestAScopeWithWhitespaceIsRefused(t *testing.T) {
	provider := validOIDC()
	provider.OIDC.Scopes = []string{"profile email"}

	_, err := Validate(provider)
	requireFieldRefusal(t, err, "scopes")
}

// The row must not carry a document its kind does not name. A reader switching
// on Kind is otherwise one refactor away from reading the other protocol's
// stale document.
func TestValidateClearsTheOtherKindsDocument(t *testing.T) {
	provider := validOIDC()
	provider.SAML = &SAMLDocument{IDPEntityID: "left over"}

	stored, err := Validate(provider)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if stored.SAML != nil {
		t.Fatalf("the SAML document survived on an OIDC provider")
	}
}

/* ── SAML trust anchors ─────────────────────────────────────────────────── */

// An unparseable certificate must be REFUSED, not skipped. A definition that
// silently keeps only some of its trust anchors verifies assertions today and
// stops the day the identity provider rotates to the key that was dropped.
func TestAnUnparseableCertificateIsRefusedNotSkipped(t *testing.T) {
	provider := validSAML(t)
	provider.SAML.IDPCertificates = []string{rsaCertificatePEM(t, 2048), "not a certificate"}

	_, err := Validate(provider)
	requireFieldRefusal(t, err, "idp_certificates")
}

// Identity providers publish certificates inside SAML metadata as a bare base64
// body. That is what an operator copying from metadata has in hand.
func TestABareBase64CertificateIsAccepted(t *testing.T) {
	armoured := rsaCertificatePEM(t, 2048)
	block, _ := pem.Decode([]byte(armoured))
	if block == nil {
		t.Fatalf("the fixture certificate is not PEM")
	}

	provider := validSAML(t)
	provider.SAML.IDPCertificates = []string{base64.StdEncoding.EncodeToString(block.Bytes)}

	stored, err := Validate(provider)
	if err != nil {
		t.Fatalf("a base64 certificate was refused: %v", err)
	}
	if !strings.HasPrefix(stored.SAML.IDPCertificates[0], "-----BEGIN CERTIFICATE-----") {
		t.Fatalf("the stored certificate was not re-armoured: %q", stored.SAML.IDPCertificates[0])
	}
}

// Copying two `<X509Certificate>` values out of identity provider metadata
// produces two PEM blocks with no blank line between them, and the web form
// splits entries on a blank line — so a rollover pair arrives as ONE entry.
// Keeping only the first was a silent drop: the deployment worked until the
// identity provider rotated onto the discarded key, and nothing local said why.
func TestSeveralPEMBlocksInOneEntryAreAllKept(t *testing.T) {
	first := rsaCertificatePEM(t, 2048)
	second := rsaCertificatePEM(t, 2048)

	provider := validSAML(t)
	provider.SAML.IDPCertificates = []string{first + second}

	stored, err := Validate(provider)
	if err != nil {
		t.Fatalf("two adjacent PEM blocks were refused: %v", err)
	}
	if len(stored.SAML.IDPCertificates) != 2 {
		t.Fatalf("stored %d trust anchors, want 2: a dropped anchor fails at the next key rollover",
			len(stored.SAML.IDPCertificates))
	}
}

// Trailing bytes that are not another certificate are refused, not ignored.
// They are most often a truncated paste.
func TestTrailingDataAfterACertificateIsRefused(t *testing.T) {
	provider := validSAML(t)
	provider.SAML.IDPCertificates = []string{rsaCertificatePEM(t, 2048) + "-----BEGIN CERTIF"}

	_, err := Validate(provider)
	requireFieldRefusal(t, err, "idp_certificates")
}

// The service provider certificate must be the ONE that matches the sealed key.
// Keeping whichever came first would publish a certificate the key cannot sign
// for, and every signed request would be rejected at the identity provider.
func TestASecondServiceProviderCertificateIsRefused(t *testing.T) {
	provider := validSAML(t)
	provider.SAML.SignAuthnRequests = true
	provider.SAML.SPCertificate = rsaCertificatePEM(t, 2048) + rsaCertificatePEM(t, 2048)

	_, err := Validate(provider)
	requireFieldRefusal(t, err, "sp_certificate")
}

// A 1024-bit RSA signing key is forgeable. Accepting it would make every
// assertion this deployment trusts forgeable with it.
func TestAWeakSigningKeyIsRefused(t *testing.T) {
	provider := validSAML(t)
	provider.SAML.IDPCertificates = []string{rsaCertificatePEM(t, 1024)}

	_, err := Validate(provider)
	requireFieldRefusal(t, err, "idp_certificates")
}

// An ECDSA P-256 anchor is as acceptable as RSA-2048, and the strength check
// must not refuse a whole key type by not knowing it.
func TestAnECDSACertificateIsAccepted(t *testing.T) {
	provider := validSAML(t)
	provider.SAML.IDPCertificates = []string{ecdsaCertificatePEM(t)}

	if _, err := Validate(provider); err != nil {
		t.Fatalf("an ECDSA anchor was refused: %v", err)
	}
}

// Signing an outbound request needs the certificate that matches the sealed
// key. Without it, the identity provider receives a signature it cannot check.
func TestSigningRequestsWithoutACertificateIsRefused(t *testing.T) {
	provider := validSAML(t)
	provider.SAML.SignAuthnRequests = true

	_, err := Validate(provider)
	requireFieldRefusal(t, err, "sp_certificate")
}

// A skew wide enough to matter stops being a correction for clock drift and
// becomes an extension of an assertion's lifetime.
func TestAnUnboundedClockSkewIsRefused(t *testing.T) {
	provider := validSAML(t)
	provider.SAML.ClockSkewSeconds = MaxClockSkewSeconds + 1

	_, err := Validate(provider)
	requireFieldRefusal(t, err, "clock_skew_seconds")
}

/* ── keys ───────────────────────────────────────────────────────────────── */

// The key is the URL segment of the admin surface and the lookup column. Both
// sides normalise, so an operator's spelling cannot produce a row they then
// cannot find.
func TestKeyNormalisationIsStable(t *testing.T) {
	for _, spelling := range []string{"Corporate Okta", "corporate-okta", "  CORPORATE__OKTA  "} {
		if got := NormalizeKey(spelling); got != "corporate_okta" {
			t.Fatalf("NormalizeKey(%q) = %q, want corporate_okta", spelling, got)
		}
	}
}

// A key of only punctuation normalises to nothing, and nothing is not a key.
func TestAnEmptyKeyIsRefused(t *testing.T) {
	provider := validOIDC()
	provider.Key = "---"

	_, err := Validate(provider)
	requireFieldRefusal(t, err, "key")
}

/* ── helpers ────────────────────────────────────────────────────────────── */

func requireFieldRefusal(t *testing.T, err error, field string) {
	t.Helper()
	var validation ValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("error is %v, want a ValidationError naming %q", err, field)
	}
	if validation.Field != field {
		t.Fatalf("refusal names field %q, want %q", validation.Field, field)
	}
}

func rsaCertificatePEM(t *testing.T, bits int) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	return selfSignedPEM(t, key.Public(), key)
}

func ecdsaCertificatePEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate ECDSA key: %v", err)
	}
	return selfSignedPEM(t, key.Public(), key)
}

func selfSignedPEM(t *testing.T, public, private any) string {
	t.Helper()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "identity provider"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}
