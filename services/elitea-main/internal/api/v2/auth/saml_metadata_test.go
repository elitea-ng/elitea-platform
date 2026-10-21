package auth

// What these tests hold in place.
//
// `gosaml2.SAMLServiceProvider.Metadata` fails "empty SP encryption
// certificate" whenever no service-provider key is attached, and a key is
// attached only when request signing is on. Before serviceProviderMetadata, an
// operator who left signing off got a 503 from the one URL the identity
// provider is told to import. The unsigned test is that fact; the signed test
// pins that the library's own descriptor (with its certificate) still serves.

import (
	"context"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	saml2 "github.com/russellhaering/gosaml2"
	"github.com/russellhaering/gosaml2/types"
	dsig "github.com/russellhaering/goxmldsig"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/identityproviders"
)

const (
	metadataEntityID = "https://elitea.example.com/saml"
	metadataNameID   = "urn:oasis:names:tc:SAML:1.1:nameid-format:emailAddress"
)

type fixedSAMLProviderSource struct {
	provider identityproviders.Provider
}

func (s fixedSAMLProviderSource) Enabled(context.Context, identityproviders.Kind) (identityproviders.Provider, error) {
	return s.provider, nil
}

// metadataHandler serves one cached runtime, so the handler is exercised
// without building one from a certificate chain and a vault.
func metadataHandler(signed bool) *SAMLHandler {
	document := identityproviders.SAMLDocument{
		SPEntityID:        metadataEntityID,
		ACSURL:            testACSURL,
		NameIDFormat:      metadataNameID,
		SignAuthnRequests: signed,
	}
	serviceProvider := &saml2.SAMLServiceProvider{
		AssertionConsumerServiceURL: testACSURL,
		ServiceProviderIssuer:       metadataEntityID,
		NameIdFormat:                metadataNameID,
		SignAuthnRequests:           signed,
		Clock:                       dsig.NewRealClock(),
	}
	if signed {
		serviceProvider.SPKeyStore = dsig.RandomKeyStoreForTest()
	}
	provider := identityproviders.Provider{Key: "corp", Revision: 1, SAML: &document}
	return &SAMLHandler{
		providers: fixedSAMLProviderSource{provider: provider},
		runtimeCache: map[string]*samlRuntime{"corp@1": {
			provider: serviceProvider,
			document: document,
			origin:   "test",
		}},
	}
}

func fetchMetadata(t *testing.T, handler *SAMLHandler) types.EntityDescriptor {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.Metadata(recorder, httptest.NewRequest(http.MethodGet, SAMLMetadataPath, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("metadata status = %d, body %q", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/samlmetadata+xml" {
		t.Fatalf("Content-Type = %q", got)
	}
	var descriptor types.EntityDescriptor
	if err := xml.Unmarshal(recorder.Body.Bytes(), &descriptor); err != nil {
		t.Fatalf("metadata is not XML: %v", err)
	}
	if descriptor.EntityID != metadataEntityID {
		t.Fatalf("entityID = %q", descriptor.EntityID)
	}
	sp := descriptor.SPSSODescriptor
	if sp == nil {
		t.Fatal("no SPSSODescriptor")
	}
	if len(sp.AssertionConsumerServices) != 1 ||
		sp.AssertionConsumerServices[0].Location != testACSURL ||
		sp.AssertionConsumerServices[0].Binding != saml2.BindingHttpPost {
		t.Fatalf("ACS = %+v", sp.AssertionConsumerServices)
	}
	if !sp.WantAssertionsSigned {
		t.Fatal("WantAssertionsSigned is false")
	}
	if len(sp.NameIDFormats) != 1 || strings.TrimSpace(sp.NameIDFormats[0]) != metadataNameID {
		t.Fatalf("NameIDFormat = %v", sp.NameIDFormats)
	}
	return descriptor
}

func TestMetadataIsServedForAnUnsignedProvider(t *testing.T) {
	descriptor := fetchMetadata(t, metadataHandler(false))
	sp := descriptor.SPSSODescriptor
	if sp.AuthnRequestsSigned {
		t.Fatal("AuthnRequestsSigned is true for a provider that does not sign")
	}
	if len(sp.KeyDescriptors) != 0 {
		t.Fatalf("an unsigned provider publishes %d KeyDescriptor(s)", len(sp.KeyDescriptors))
	}
}

func TestMetadataForASigningProviderCarriesItsCertificate(t *testing.T) {
	descriptor := fetchMetadata(t, metadataHandler(true))
	sp := descriptor.SPSSODescriptor
	if !sp.AuthnRequestsSigned {
		t.Fatal("AuthnRequestsSigned is false for a provider that signs")
	}
	var signing bool
	for _, key := range sp.KeyDescriptors {
		if key.Use == "signing" && len(key.KeyInfo.X509Data.X509Certificates) == 1 &&
			key.KeyInfo.X509Data.X509Certificates[0].Data != "" {
			signing = true
		}
	}
	if !signing {
		t.Fatalf("no signing certificate in %+v", sp.KeyDescriptors)
	}
}
