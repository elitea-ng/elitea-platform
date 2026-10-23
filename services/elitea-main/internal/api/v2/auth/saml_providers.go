package auth

// Where the SAML login path gets its configuration, and how the service
// provider is built from it.
//
// The precedence, the cache and the restart rule are the SAME as OIDC's, and
// oidc_providers.go states them once. The difference here is that there is no
// environment fallback: SAML never had one. A deployment federates SAML only
// through an authored provider, so "no enabled row" means "this deployment does
// not do SAML" and the routes say so.
//
// # Why an XML signature library and not our own
//
// Verifying a SAML assertion means verifying an XML digital signature, and that
// means exclusive canonicalisation, reference resolution and the whole family
// of signature-wrapping attacks that follow from getting either wrong. Those
// attacks work by presenting a document where the element that was SIGNED and
// the element that is READ are different — and a hand-written verifier that
// checks "there is a valid signature somewhere in this document" is exactly the
// shape that falls to them.
//
// `github.com/russellhaering/gosaml2` is a reviewed, fuzzed implementation of
// that verification, and this file uses it rather than reproducing it. What is
// NOT delegated is stated in saml.go: the library reports an expired assertion
// and a wrong audience as WARNINGS, and a caller that does not read them
// accepts both.

import (
	"context"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"

	saml2 "github.com/russellhaering/gosaml2"
	dsig "github.com/russellhaering/goxmldsig"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/identityproviders"
)

// errNoSAMLProvider reports that this deployment federates no SAML logins.
var errNoSAMLProvider = errors.New("auth: no SAML identity provider is configured")

// samlRuntime is one usable service provider: everything the metadata, login
// and assertion-consumer paths need, and nothing they do not.
type samlRuntime struct {
	provider *saml2.SAMLServiceProvider
	document identityproviders.SAMLDocument

	// clockSkewSeconds is applied by THIS package when it re-checks the
	// assertion's time conditions. The library has no tolerance of its own; see
	// saml.go:conditionsAcceptable.
	clockSkewSeconds int

	// origin names where this runtime came from, for the log line only.
	origin string
}

// runtime resolves the service provider this request must use.
func (h *SAMLHandler) runtime(ctx context.Context) (*samlRuntime, error) {
	if h.providers == nil {
		return nil, errNoSAMLProvider
	}
	provider, err := h.providers.Enabled(ctx, identityproviders.KindSAML)
	switch {
	case err == nil:
	case errors.Is(err, identityproviders.ErrNotFound),
		identityproviders.IsSchemaMissing(err):
		// A table that does not exist holds no rows. This deployment federates
		// no SAML logins, which is what the routes then say.
		return nil, errNoSAMLProvider
	default:
		// An unreadable table is NOT "no provider". Reporting it as the second
		// would present a configured deployment as one that federates nothing.
		return nil, fmt.Errorf("read the enabled identity provider: %w", err)
	}
	return h.runtimeFor(ctx, provider)
}

// runtimeFor returns the cached runtime for one authored revision, building it
// on a miss. The cache key is `provider_key@revision`; see oidc_providers.go.
func (h *SAMLHandler) runtimeFor(
	ctx context.Context,
	provider identityproviders.Provider,
) (*samlRuntime, error) {
	if provider.SAML == nil {
		return nil, fmt.Errorf("the enabled provider %q carries no SAML document", provider.Key)
	}
	cacheKey := fmt.Sprintf("%s@%d", provider.Key, provider.Revision)

	h.runtimeMu.Lock()
	cached, ok := h.runtimeCache[cacheKey]
	h.runtimeMu.Unlock()
	if ok {
		return cached, nil
	}

	built, err := h.buildRuntime(ctx, provider)
	if err != nil {
		return nil, err
	}

	h.runtimeMu.Lock()
	defer h.runtimeMu.Unlock()
	if existing, ok := h.runtimeCache[cacheKey]; ok {
		return existing, nil
	}
	// One entry. A superseded revision is never used again.
	h.runtimeCache = map[string]*samlRuntime{cacheKey: built}
	return built, nil
}

func (h *SAMLHandler) buildRuntime(
	ctx context.Context,
	provider identityproviders.Provider,
) (*samlRuntime, error) {
	document := provider.SAML

	certificates, err := parseCertificateChain(document.IDPCertificates)
	if err != nil {
		return nil, fmt.Errorf("provider %q: %w", provider.Key, err)
	}
	if len(certificates) == 0 {
		// Validate refuses a document with no anchor, so this can only be a row
		// written before it. It is fatal rather than a provider that trusts
		// every signature, which is what an empty store would mean.
		return nil, fmt.Errorf("provider %q has no identity provider signing certificate", provider.Key)
	}

	serviceProvider := &saml2.SAMLServiceProvider{
		IdentityProviderSSOURL:      document.IDPSSOURL,
		IdentityProviderSLOURL:      document.IDPSLOURL,
		IdentityProviderIssuer:      document.IDPEntityID,
		AssertionConsumerServiceURL: document.ACSURL,
		ServiceProviderIssuer:       document.SPEntityID,
		// The audience an assertion must name is THIS deployment. The library
		// reports a mismatch as a warning; saml.go refuses on it.
		AudienceURI:         document.SPEntityID,
		IDPCertificateStore: &dsig.MemoryX509CertificateStore{Roots: certificates},
		NameIdFormat:        document.NameIDFormat,
		// Never true. There is no field for it on the document and no code path
		// that sets it: an unsigned assertion is an assertion anybody can write.
		SkipSignatureValidation: false,
		SignAuthnRequests:       document.SignAuthnRequests,
		Clock:                   dsig.NewRealClock(),
	}

	if document.SignAuthnRequests {
		keyStore, err := h.signingKeyStore(ctx, provider)
		if err != nil {
			return nil, err
		}
		serviceProvider.SPKeyStore = keyStore
	}

	return &samlRuntime{
		provider:         serviceProvider,
		document:         *document,
		clockSkewSeconds: document.ClockSkewSeconds,
		origin:           fmt.Sprintf("provider %s revision %d", provider.Key, provider.Revision),
	}, nil
}

// signingKeyStore reads the sealed service-provider private key and pairs it
// with the published certificate.
//
// A failure here is FATAL for the runtime rather than a service provider that
// sends unsigned requests. The identity provider is configured to require a
// signature when this is on; an unsigned request would be refused there, with a
// message naming neither this vault nor this key.
func (h *SAMLHandler) signingKeyStore(
	ctx context.Context,
	provider identityproviders.Provider,
) (dsig.X509KeyStore, error) {
	if h.secretSource == nil || provider.SecretRef == "" {
		return nil, fmt.Errorf("provider %q signs its requests but holds no sealed private key", provider.Key)
	}
	material, err := h.secretSource.LookupAdminHiddenSecret(ctx, provider.SecretRef)
	if err != nil {
		return nil, fmt.Errorf("read the service provider private key for %q: %w", provider.Key, err)
	}
	certificate, err := tls.X509KeyPair(
		[]byte(provider.SAML.SPCertificate), []byte(material))
	if err != nil {
		// The most likely cause by far is that the sealed key does not match
		// the published certificate, which is worth saying plainly: the
		// operator has one of the two wrong, and no error from the identity
		// provider would ever tell them which.
		return nil, fmt.Errorf(
			"provider %q: the sealed private key and the published certificate are not a pair", provider.Key)
	}
	if _, ok := certificate.PrivateKey.(*rsa.PrivateKey); !ok {
		// goxmldsig signs with RSA. An ECDSA key would be accepted by
		// X509KeyPair and then fail at the first signature.
		return nil, fmt.Errorf("provider %q: the service provider signing key must be RSA", provider.Key)
	}
	return dsig.TLSCertKeyStore(certificate), nil
}

// parseCertificateChain decodes the stored trust anchors.
//
// An unparseable entry is an ERROR, not a skipped one. Validate stores only
// re-encoded, parsed certificates, so reaching this with a bad entry means the
// row was written by something else — and a store that silently kept the
// readable subset would verify assertions until the identity provider rotated
// to the key that was dropped.
func parseCertificateChain(entries []string) ([]*x509.Certificate, error) {
	certificates := make([]*x509.Certificate, 0, len(entries))
	for _, entry := range entries {
		block, _ := pem.Decode([]byte(entry))
		if block == nil {
			return nil, errors.New("a stored signing certificate is not PEM")
		}
		certificate, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, errors.New("a stored signing certificate could not be parsed")
		}
		certificates = append(certificates, certificate)
	}
	return certificates, nil
}

// HasEnabledSAMLProvider reports whether a stored, enabled SAML provider exists.
//
// The composition root calls it at boot to decide whether to MOUNT the SAML
// browser routes. A read failure is returned, not swallowed: a deployment that
// cannot read its provider table must not start silently unfederated.
func HasEnabledSAMLProvider(ctx context.Context, providers IdentityProviderSource) (bool, error) {
	if providers == nil {
		return false, nil
	}
	_, err := providers.Enabled(ctx, identityproviders.KindSAML)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, identityproviders.ErrNotFound):
		return false, nil
	default:
		// See HasEnabledOIDCProvider: a missing table reaches the caller as an
		// error so the composition root can name it in a log line.
		return false, err
	}
}
