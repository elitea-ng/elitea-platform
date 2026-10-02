package identityproviders

// Validation of an authored provider document.
//
// The rule this file exists to enforce is the specification's: "insecure
// defaults fail validation". A definition that would federate logins over a
// plaintext channel, trust an unparseable certificate, or ask for tokens
// without `openid` is refused at the write, not discovered at the first login.
//
// Every refusal NAMES THE FIELD. Silently dropping or correcting a field an
// operator believes they set is the failure mode this whole surface exists to
// remove — it is the same defect as a form that saves into a void, moved one
// layer down.
//
// Normalisation and refusal are one pass on purpose. Validate returns the
// document it would store, so the caller cannot store a different one from the
// one that was checked.

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// ValidationError names the field that was refused and why.
//
// It is a distinct type so the HTTP layer can answer 400 with the field name
// and keep every other failure a 503. A caller that rendered every error as a
// bad request would report a database outage as the operator's typing mistake.
type ValidationError struct {
	Field  string
	Reason string
}

func (e ValidationError) Error() string {
	return fmt.Sprintf("%s: %s", e.Field, e.Reason)
}

func invalid(field, reason string) error {
	return ValidationError{Field: field, Reason: reason}
}

// wellKnownSuffix is the discovery path pylon's `metadata_endpoint` carries.
//
// An operator moving a definition across from the reference page has that URL
// in hand, and typing it here is the obvious mistake. The issuer is what an
// id_token's `iss` claim is compared against, so storing the discovery URL
// would make every token fail verification with a message about the issuer that
// named neither field. Accepting the spelling and storing the issuer is a
// correction the operator cannot get wrong; it is documented on OIDCDocument.
const wellKnownSuffix = "/.well-known/openid-configuration"

// Validate checks and normalises one authored provider.
//
// It returns the provider AS IT WOULD BE STORED. The caller writes the returned
// value, never the input.
func Validate(provider Provider) (Provider, error) {
	provider.Key = NormalizeKey(provider.Key)
	if provider.Key == "" {
		return Provider{}, invalid("key", "a provider key is required")
	}
	provider.DisplayName = strings.TrimSpace(provider.DisplayName)
	if provider.DisplayName == "" {
		return Provider{}, invalid("display_name", "a display name is required")
	}

	switch provider.Kind {
	case KindOIDC:
		if provider.OIDC == nil {
			return Provider{}, invalid("oidc", "an OIDC provider needs an OIDC configuration")
		}
		document, err := validateOIDC(*provider.OIDC)
		if err != nil {
			return Provider{}, err
		}
		provider.OIDC = &document
		provider.SAML = nil
	case KindSAML:
		if provider.SAML == nil {
			return Provider{}, invalid("saml", "a SAML provider needs a SAML configuration")
		}
		document, err := validateSAML(*provider.SAML)
		if err != nil {
			return Provider{}, err
		}
		provider.SAML = &document
		provider.OIDC = nil
	default:
		return Provider{}, invalid("kind", "the provider kind must be oidc or saml")
	}
	return provider, nil
}

func validateOIDC(document OIDCDocument) (OIDCDocument, error) {
	issuer := strings.TrimSpace(document.Issuer)
	if issuer == "" {
		return OIDCDocument{}, invalid("issuer", "an issuer URL is required")
	}
	// See wellKnownSuffix. The discovery URL is accepted and reduced.
	issuer = strings.TrimSuffix(strings.TrimSuffix(issuer, "/"), wellKnownSuffix)
	issuer = strings.TrimSuffix(issuer, "/")
	if err := validateFederationURL("issuer", issuer); err != nil {
		return OIDCDocument{}, err
	}
	document.Issuer = issuer

	document.ClientID = strings.TrimSpace(document.ClientID)
	if document.ClientID == "" {
		return OIDCDocument{}, invalid("client_id", "a client identifier is required")
	}

	document.RedirectURI = strings.TrimSpace(document.RedirectURI)
	if document.RedirectURI == "" {
		return OIDCDocument{}, invalid("redirect_uri", "a redirect URI is required")
	}
	if err := validateFederationURL("redirect_uri", document.RedirectURI); err != nil {
		return OIDCDocument{}, err
	}

	scopes, err := normalizeScopes(document.Scopes)
	if err != nil {
		return OIDCDocument{}, err
	}
	document.Scopes = scopes

	domains, err := NormalizeLoginDomains(document.LoginDomains)
	if err != nil {
		return OIDCDocument{}, err
	}
	document.LoginDomains = domains

	return document, nil
}

// normalizeScopes de-duplicates, preserves the operator's order, and guarantees
// `openid`.
//
// `openid` is ADDED rather than demanded. A request without it is not an
// OpenID Connect request: the provider returns no id_token, and the login fails
// with a message about a missing token rather than about a missing scope. There
// is no deployment for which leaving it out is correct, so this is a correction
// with exactly one possible intent — unlike every refusal above it.
func normalizeScopes(raw []string) ([]string, error) {
	seen := map[string]bool{}
	scopes := make([]string, 0, len(raw)+1)
	for _, scope := range raw {
		scope = strings.TrimSpace(scope)
		if scope == "" {
			continue
		}
		// A scope carrying a space would be sent as two scopes, one of which
		// the operator never authored.
		if strings.ContainsAny(scope, " \t\r\n") {
			return nil, invalid("scopes", "a scope must not contain whitespace")
		}
		if seen[scope] {
			continue
		}
		seen[scope] = true
		scopes = append(scopes, scope)
	}
	if !seen["openid"] {
		scopes = append([]string{"openid"}, scopes...)
	}
	return scopes, nil
}

func normalizeClockSkew(seconds int) (int, error) {
	if seconds == 0 {
		return DefaultClockSkewSeconds, nil
	}
	if seconds < 0 || seconds > MaxClockSkewSeconds {
		return 0, invalid("clock_skew_seconds",
			fmt.Sprintf("the clock skew must be between 0 and %d seconds", MaxClockSkewSeconds))
	}
	return seconds, nil
}

func validateSAML(document SAMLDocument) (SAMLDocument, error) {
	document.IDPEntityID = strings.TrimSpace(document.IDPEntityID)
	if document.IDPEntityID == "" {
		return SAMLDocument{}, invalid("idp_entity_id", "an identity provider entity ID is required")
	}

	document.IDPSSOURL = strings.TrimSpace(document.IDPSSOURL)
	if document.IDPSSOURL == "" {
		return SAMLDocument{}, invalid("idp_sso_url", "a single sign-on URL is required")
	}
	if err := validateFederationURL("idp_sso_url", document.IDPSSOURL); err != nil {
		return SAMLDocument{}, err
	}

	document.IDPSLOURL = strings.TrimSpace(document.IDPSLOURL)
	if document.IDPSLOURL != "" {
		if err := validateFederationURL("idp_slo_url", document.IDPSLOURL); err != nil {
			return SAMLDocument{}, err
		}
	}

	certificates, err := normalizeCertificates("idp_certificates", document.IDPCertificates, true)
	if err != nil {
		return SAMLDocument{}, err
	}
	document.IDPCertificates = certificates

	document.SPEntityID = strings.TrimSpace(document.SPEntityID)
	if document.SPEntityID == "" {
		return SAMLDocument{}, invalid("sp_entity_id", "a service provider entity ID is required")
	}
	if strings.ContainsAny(document.SPEntityID, " \t\r\n") {
		return SAMLDocument{}, invalid("sp_entity_id", "an entity ID must not contain whitespace")
	}

	document.ACSURL = strings.TrimSpace(document.ACSURL)
	if document.ACSURL == "" {
		return SAMLDocument{}, invalid("acs_url", "an assertion consumer service URL is required")
	}
	if err := validateFederationURL("acs_url", document.ACSURL); err != nil {
		return SAMLDocument{}, err
	}

	document.NameIDFormat = strings.TrimSpace(document.NameIDFormat)
	if document.NameIDFormat != "" && !knownNameIDFormats[document.NameIDFormat] {
		return SAMLDocument{}, invalid("name_id_format", "the NameID format is not one this service requests")
	}

	document.EmailAttribute = strings.TrimSpace(document.EmailAttribute)
	document.NameAttribute = strings.TrimSpace(document.NameAttribute)

	// The service-provider certificate is OPTIONAL and is only meaningful with
	// a sealed key. It is validated when present so a malformed certificate is
	// refused here rather than published in metadata the identity provider
	// then cannot read.
	document.SPCertificate = strings.TrimSpace(document.SPCertificate)
	if document.SPCertificate != "" {
		certificate, err := normalizeOneCertificate("sp_certificate", document.SPCertificate)
		if err != nil {
			return SAMLDocument{}, err
		}
		document.SPCertificate = certificate
	}
	if document.SignAuthnRequests && document.SPCertificate == "" {
		return SAMLDocument{}, invalid("sp_certificate",
			"signing authentication requests needs the service provider certificate that matches the sealed key")
	}

	skew, err := normalizeClockSkew(document.ClockSkewSeconds)
	if err != nil {
		return SAMLDocument{}, err
	}
	document.ClockSkewSeconds = skew

	domains, err := NormalizeLoginDomains(document.LoginDomains)
	if err != nil {
		return SAMLDocument{}, err
	}
	document.LoginDomains = domains
	return document, nil
}

// MaxLoginDomains bounds the routing list of one provider. The sign-in page
// compares a typed domain against every entry, so the list stays small.
const MaxLoginDomains = 100

// NormalizeLoginDomains returns the stored form of a provider's login domains.
//
// Each entry is trimmed, lower-cased and stripped of a leading "@", because an
// operator often copies "@corp.com" from an address. An entry that is not a
// host name with at least two labels is REFUSED, not dropped: a domain that
// silently left the list would send its users to the wrong provider. Duplicate
// entries collapse to one. Nil and empty input return nil.
func NormalizeLoginDomains(raw []string) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	seen := map[string]bool{}
	domains := make([]string, 0, len(raw))
	for _, entry := range raw {
		domain := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(entry)), "@")
		if domain == "" {
			continue
		}
		if !IsLoginDomain(domain) {
			return nil, invalid("login_domains",
				fmt.Sprintf("%q is not a domain name such as example.com", entry))
		}
		if seen[domain] {
			continue
		}
		seen[domain] = true
		domains = append(domains, domain)
	}
	if len(domains) > MaxLoginDomains {
		return nil, invalid("login_domains",
			fmt.Sprintf("a provider can list at most %d login domains", MaxLoginDomains))
	}
	if len(domains) == 0 {
		return nil, nil
	}
	return domains, nil
}

// IsLoginDomain reports whether a lower-case value is a host name of two or
// more labels. Each label is 1 to 63 characters of [a-z0-9-], and does not
// start or end with a hyphen. An internationalised domain must be given in its
// ASCII (punycode) form.
func IsLoginDomain(value string) bool {
	if len(value) == 0 || len(value) > 253 {
		return false
	}
	labels := strings.Split(value, ".")
	if len(labels) < 2 {
		return false
	}
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, character := range label {
			switch {
			case character >= 'a' && character <= 'z', character >= '0' && character <= '9', character == '-':
			default:
				return false
			}
		}
	}
	return true
}

// knownNameIDFormats are the formats this service will request.
//
// The list is closed because the request carries the value verbatim: an
// unlisted string is a format the identity provider answers with an error the
// operator would have to read at the provider to understand.
var knownNameIDFormats = map[string]bool{
	"urn:oasis:names:tc:SAML:1.1:nameid-format:unspecified":                true,
	"urn:oasis:names:tc:SAML:1.1:nameid-format:emailAddress":               true,
	"urn:oasis:names:tc:SAML:2.0:nameid-format:persistent":                 true,
	"urn:oasis:names:tc:SAML:2.0:nameid-format:transient":                  true,
	"urn:oasis:names:tc:SAML:2.0:nameid-format:entity":                     true,
	"urn:oasis:names:tc:SAML:1.1:nameid-format:X509SubjectName":            true,
	"urn:oasis:names:tc:SAML:1.1:nameid-format:WindowsDomainQualifiedName": true,
}

// normalizeCertificates parses every entry and returns it re-encoded as PEM.
//
// An UNPARSEABLE entry is refused, never skipped. A definition that silently
// keeps only some of its trust anchors verifies assertions today and stops
// verifying them the day the identity provider rotates to the key that was
// dropped — a failure with no local evidence of its cause.
//
// A bare base64 body with no PEM armour is accepted. Identity providers publish
// certificates that way inside SAML metadata, so it is what an operator copying
// from metadata has in hand.
func normalizeCertificates(field string, raw []string, required bool) ([]string, error) {
	certificates := make([]string, 0, len(raw))
	for _, entry := range raw {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		// One entry may hold SEVERAL certificates. Copying two
		// `<X509Certificate>` values out of an identity provider's metadata
		// produces two PEM blocks with no blank line between them, and the web
		// form splits entries on a blank line — so a rollover pair arrives here
		// as one string. Reading only the first was a silent drop: the second
		// anchor vanished, and the deployment kept working until the identity
		// provider rotated onto the key that had been discarded.
		parsed, err := parseCertificates(entry)
		if err != nil {
			return nil, invalid(field, err.Error())
		}
		for _, certificate := range parsed {
			if err := checkCertificateStrength(field, certificate); err != nil {
				return nil, err
			}
			certificates = append(certificates, string(pem.EncodeToMemory(&pem.Block{
				Type:  "CERTIFICATE",
				Bytes: certificate.Raw,
			})))
		}
	}
	if required && len(certificates) == 0 {
		return nil, invalid(field, "at least one signing certificate is required")
	}
	return certificates, nil
}

// normalizeOneCertificate is normalizeCertificates for a field that may hold
// exactly one certificate.
//
// It refuses a second rather than keeping the first. The service provider
// certificate is published in metadata and must be the one that matches the
// sealed private key; silently keeping whichever came first would publish a
// certificate the key cannot sign for, and the identity provider would reject
// every signed request with an error naming none of this.
func normalizeOneCertificate(field, raw string) (string, error) {
	certificates, err := normalizeCertificates(field, []string{raw}, true)
	if err != nil {
		return "", err
	}
	if len(certificates) != 1 {
		return "", invalid(field, "give exactly one certificate: this field names the one that matches the sealed key")
	}
	return certificates[0], nil
}

func parseCertificates(entry string) ([]*x509.Certificate, error) {
	rest := []byte(entry)
	var certificates []*x509.Certificate
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("expected a CERTIFICATE block, found %q", block.Type)
		}
		certificate, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("a certificate could not be parsed: %w", err)
		}
		certificates = append(certificates, certificate)
	}
	if len(certificates) > 0 {
		// Trailing bytes that are not another PEM block are REFUSED, not
		// ignored. They are most often a truncated paste, and ignoring them is
		// the same silent drop this function was rewritten to remove.
		if len(bytes.TrimSpace(rest)) > 0 {
			return nil, fmt.Errorf("the value carries trailing data that is not a certificate")
		}
		return certificates, nil
	}
	// A bare base64 body, as SAML metadata carries it. Re-armour and parse
	// through the same path so there is one decoder, not two.
	compact := strings.Join(strings.Fields(entry), "")
	block, _ := pem.Decode([]byte("-----BEGIN CERTIFICATE-----\n" + compact + "\n-----END CERTIFICATE-----\n"))
	if block == nil {
		return nil, fmt.Errorf("the value is neither PEM nor base64 certificate data")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("a certificate could not be parsed: %w", err)
	}
	return []*x509.Certificate{certificate}, nil
}

// checkCertificateStrength refuses a key too small to be a trust anchor.
//
// An expired certificate is NOT refused here. Expiry is checked where it
// matters — at signature verification, against the signing time — and refusing
// it at authoring time would stop an operator loading a rollover key ahead of
// its validity window, which is exactly when they should load it.
func checkCertificateStrength(field string, certificate *x509.Certificate) error {
	switch key := certificate.PublicKey.(type) {
	case *rsa.PublicKey:
		if key.N.BitLen() < 2048 {
			return invalid(field, "an RSA signing key must be at least 2048 bits")
		}
	case *ecdsa.PublicKey:
		if key.Curve.Params().BitSize < 256 {
			return invalid(field, "an ECDSA signing key must be at least 256 bits")
		}
	default:
		return invalid(field, "the certificate carries a public key type this service cannot verify with")
	}
	return nil
}

// validateFederationURL refuses a URL that would carry a login over a channel
// an attacker on the path can read or rewrite.
//
// HTTPS is required. The ONE exception is a loopback host, because the
// end-to-end stack runs its identity provider on `oidc.localhost` over plain
// HTTP and refusing that would mean the tests could not exercise this code at
// all. A loopback address is not reachable from another machine, so the
// exception grants no exposure off the host.
//
// A fragment is refused outright. Neither an issuer, a redirect URI nor a SAML
// endpoint has any use for one, and a fragment on a redirect URI is where an
// implicit-flow token would land.
func validateFederationURL(field, raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return invalid(field, "the value is not a valid URL")
	}
	if !parsed.IsAbs() || parsed.Host == "" {
		return invalid(field, "the URL must be absolute and name a host")
	}
	if parsed.Fragment != "" || strings.Contains(raw, "#") {
		return invalid(field, "the URL must not carry a fragment")
	}
	switch strings.ToLower(parsed.Scheme) {
	case "https":
		return nil
	case "http":
		if isLoopbackHost(parsed.Hostname()) {
			return nil
		}
		return invalid(field, "the URL must use https: a plaintext federation endpoint exposes the login")
	default:
		return invalid(field, "the URL must use https")
	}
}

// isLoopbackHost reports whether the host cannot be reached from another
// machine.
//
// `localhost` and any `*.localhost` name are included because RFC 6761 reserves
// them for loopback, and the end-to-end stack's identity provider is
// `oidc.localhost`. A name that merely CONTAINS "localhost" is not enough —
// `localhost.example.com` is a public name — so the suffix match is on a label
// boundary.
//
// THE HOST IS NEVER RESOLVED. Deciding this by DNS would make the exception
// depend on what a name pointed at during the one second the operator pressed
// save: `http://idp.example.com` whose record answered 127.0.0.1 would be
// accepted and then federate logins in clear text to a public host the moment
// the record changed. It would also make authoring a provider perform a lookup
// on an operator-supplied name. Only literals are accepted.
func isLoopbackHost(host string) bool {
	host = strings.ToLower(strings.Trim(host, "[]"))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
