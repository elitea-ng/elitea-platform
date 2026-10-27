package api

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/agentidentity/eliteanative"
	v2skills "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/skills"
	identity "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/secretstore"
)

type noIdentities struct{}

func (noIdentities) GetExternalIdentity(context.Context, string) (identity.ExternalAgentIdentity, error) {
	return identity.ExternalAgentIdentity{}, identity.ErrExternalIdentityNotFound
}

func testAgentAssertionWellKnown(t *testing.T) *eliteanative.WellKnown {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := eliteanative.NewLocalSigner("es-1", eliteanative.ES256,
		secretstore.NewRuntimeSecret(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})))
	if err != nil {
		t.Fatal(err)
	}
	ring, err := eliteanative.NewKeyRing([]eliteanative.RingKey{{Signer: signer}}, time.Hour, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg := eliteanative.Config{
		Origin: "https://elitea.example.com", RotationOverlap: time.Hour,
		Keys: []eliteanative.KeySpec{{KeyID: "es-1", Algorithm: eliteanative.ES256, File: "/k.pem"}},
		RelyingParties: []eliteanative.RelyingParty{{ID: "aws", Audience: "sts.amazonaws.com", Algorithm: eliteanative.ES256,
			MaxTTL: eliteanative.DefaultTTL, ClaimsProfile: eliteanative.ProfileAWS}},
	}
	provider, err := eliteanative.New(cfg, ring, noIdentities{})
	if err != nil {
		t.Fatal(err)
	}
	return provider.WellKnown()
}

var agentAssertionRoutes = []string{
	"GET " + eliteanative.JWKSPath,
	"GET " + eliteanative.MetadataPath,
}

func newAgentAssertionRouter(wellKnown *eliteanative.WellKnown) chi.Router {
	return NewRouter(RouterConfig{
		SkillsRepo:              struct{ v2skills.Repository }{},
		AuthValidator:           testTokenValidator{user: authenticatedTestUser()},
		PrincipalValidator:      testPrincipalValidator{},
		AgentAssertionWellKnown: wellKnown,
	})
}

func anonymousStatus(router http.Handler, path string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	return recorder
}

// Flag off (or no relying party): neither route exists, and both answer
// exactly what an unknown well-known path answers.
func TestAgentAssertionRoutesAreUnmountedWhenTheDependencyIsNil(t *testing.T) {
	router := NewRouter(RouterConfig{
		SkillsRepo: struct{ v2skills.Repository }{}, AuthValidator: testTokenValidator{user: authenticatedTestUser()},
		PrincipalValidator: testPrincipalValidator{},
	})
	routes := walkRoutes(t, router)
	for _, route := range agentAssertionRoutes {
		if _, ok := routes[route]; ok {
			t.Errorf("%s is mounted with AgentAssertionWellKnown nil", route)
		}
	}
	unknown := anonymousStatus(router, "/.well-known/no-such-document-1153").Code
	for _, path := range []string{eliteanative.JWKSPath, eliteanative.MetadataPath} {
		if got := anonymousStatus(router, path).Code; got != http.StatusNotFound || got != unknown {
			t.Errorf("GET %s = %d, want 404 like an unknown path (%d)", path, got, unknown)
		}
	}
}

// Flag on: both routes answer anonymously with public documents.
func TestAgentAssertionRoutesServeAnonymously(t *testing.T) {
	router := newAgentAssertionRouter(testAgentAssertionWellKnown(t))
	routes := walkRoutes(t, router)
	for _, route := range agentAssertionRoutes {
		if _, ok := routes[route]; !ok {
			t.Errorf("%s is not mounted", route)
		}
	}
	jwks := anonymousStatus(router, eliteanative.JWKSPath)
	var set struct {
		Keys []map[string]any `json:"keys"`
	}
	if jwks.Code != http.StatusOK || json.Unmarshal(jwks.Body.Bytes(), &set) != nil || len(set.Keys) != 1 || set.Keys[0]["kid"] != "es-1" {
		t.Fatalf("JWKS = %d %s", jwks.Code, jwks.Body.String())
	}
	meta := anonymousStatus(router, eliteanative.MetadataPath)
	var doc map[string]any
	if meta.Code != http.StatusOK || json.Unmarshal(meta.Body.Bytes(), &doc) != nil ||
		doc["issuer"] != "https://elitea.example.com/agent-issuer" || doc["jwks_uri"] != "https://elitea.example.com"+eliteanative.JWKSPath {
		t.Fatalf("metadata = %d %s", meta.Code, meta.Body.String())
	}
	for _, path := range []string{eliteanative.JWKSPath, eliteanative.MetadataPath} {
		if _, public := isPublic(path); !public {
			t.Errorf("%s is not listed in publicByDesign", path)
		}
	}
}
