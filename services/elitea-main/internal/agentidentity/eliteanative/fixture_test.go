package eliteanative_test

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/agentidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/agentidentity/eliteanative"
	identity "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/secretstore"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

const (
	tenantProject = int64(7)
	bindingID     = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e0a01"
	externalID    = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e0e01"
	executionID   = "exec-1"
	workerClaim   = "spiffe://elitea.local/worker/w1#claim-1"
	origin        = "https://elitea.example.com"
	awsAudience   = "sts.amazonaws.com"
	vaultAudience = "https://vault.example.com/v1/auth/jwt"
)

var fixedNow = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

func ecPEM(t *testing.T) ([]byte, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pkcs8(t, key), key
}

func rsaPEM(t *testing.T) ([]byte, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return pkcs8(t, key), key
}

func pkcs8(t *testing.T, key any) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

func localSigner(t *testing.T, kid string, alg eliteanative.Algorithm, material []byte) *eliteanative.LocalSigner {
	t.Helper()
	s, err := eliteanative.NewLocalSigner(kid, alg, secretstore.NewRuntimeSecret(material))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// countingSigner is a KMS-shaped KeySigner: it counts Sign calls, so a test
// proves a refusal happened before anything was signed.
type countingSigner struct {
	eliteanative.KeySigner
	calls atomic.Int64
}

func (c *countingSigner) Sign(ctx context.Context, digest []byte) ([]byte, error) {
	c.calls.Add(1)
	return c.KeySigner.Sign(ctx, digest)
}

func (c *countingSigner) Public() crypto.PublicKey { return c.KeySigner.Public() }

func baseConfig() eliteanative.Config {
	return eliteanative.Config{
		Origin: origin, RotationOverlap: eliteanative.DefaultRotationOverlap,
		Keys: []eliteanative.KeySpec{
			{KeyID: "es-1", Algorithm: eliteanative.ES256, File: "/keys/es-1.pem"},
			{KeyID: "rs-1", Algorithm: eliteanative.RS256, File: "/keys/rs-1.pem"},
		},
		RelyingParties: []eliteanative.RelyingParty{
			{ID: "aws-prod", Audience: awsAudience, Algorithm: eliteanative.ES256, MaxTTL: eliteanative.DefaultTTL, ClaimsProfile: eliteanative.ProfileAWS},
			{ID: "vault", Audience: vaultAudience, Algorithm: eliteanative.RS256, MaxTTL: 2 * time.Minute, ClaimsProfile: eliteanative.ProfileVaultJWT},
		},
	}
}

// memStore is the broker's binding, identity and session stores.
type memStore struct {
	mu         sync.Mutex
	bindings   map[string]identity.AgentIdentityBinding
	identities map[string]identity.ExternalAgentIdentity
	sessions   map[[32]byte]agentidentity.AgentSession
	events     []securityaudit.SecurityEvent
}

func newMemStore() *memStore {
	digest := [32]byte{0xab, 0xcd}
	return &memStore{
		bindings: map[string]identity.AgentIdentityBinding{bindingID: {
			ID: bindingID, TenantProjectID: tenantProject, ApplicationID: 11, ApplicationVersionID: 13,
			DefinitionDigest: digest, Mode: identity.IdentityModeEliteaNative, ExternalAgentIdentityID: externalID,
			State: identity.BindingActive, RevocationEpoch: 2,
		}},
		identities: map[string]identity.ExternalAgentIdentity{externalID: {
			ID: externalID, TenantProjectID: tenantProject, ApplicationID: 11, Mode: identity.IdentityModeEliteaNative,
			Ref:   identity.ExternalRef{Provider: eliteanative.ProviderName, ProviderTenant: "elitea", Kind: eliteanative.RefKind, ID: externalID, Generation: 1},
			State: identity.ExternalIdentityActive, RevocationEpoch: 3, OwnerRef: "user:42",
		}},
		sessions: map[[32]byte]agentidentity.AgentSession{},
	}
}

func (m *memStore) setIdentity(change func(*identity.ExternalAgentIdentity)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec := m.identities[externalID]
	change(&rec)
	m.identities[externalID] = rec
}

func (m *memStore) Get(_ context.Context, id string) (identity.AgentIdentityBinding, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.bindings[id]
	if !ok {
		return identity.AgentIdentityBinding{}, identity.ErrBindingNotFound
	}
	return b, nil
}

func (m *memStore) ActiveForDefinition(context.Context, int64, int64, [32]byte) (identity.AgentIdentityBinding, error) {
	return identity.AgentIdentityBinding{}, identity.ErrBindingNotFound
}

func (m *memStore) GetExternalIdentity(_ context.Context, id string) (identity.ExternalAgentIdentity, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.identities[id]
	if !ok {
		return identity.ExternalAgentIdentity{}, identity.ErrExternalIdentityNotFound
	}
	return e, nil
}

func (m *memStore) CreateExternalIdentity(context.Context, identity.ExternalAgentIdentity) (identity.ExternalAgentIdentity, error) {
	panic("unused")
}

func (m *memStore) ListExternalIdentities(context.Context, identity.ExternalListFilter) ([]identity.ExternalAgentIdentity, error) {
	panic("unused")
}

func (m *memStore) TransitionExternalIdentity(context.Context, string, func(identity.ExternalAgentIdentity, int) (identity.ExternalAgentIdentity, error)) (identity.ExternalAgentIdentity, error) {
	panic("unused")
}

func (m *memStore) ActiveBindingCount(context.Context, int64, string) (int, error) { panic("unused") }

func (m *memStore) CreateSession(_ context.Context, s agentidentity.AgentSession, digest [32]byte, e securityaudit.SecurityEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[digest] = s
	m.events = append(m.events, e)
	return nil
}

func (m *memStore) ClaimSession(_ context.Context, digest [32]byte, check func(agentidentity.AgentSession) (agentidentity.AgentSession, error)) (agentidentity.AgentSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[digest]
	if !ok {
		return agentidentity.AgentSession{}, agentidentity.ErrSessionNotFound
	}
	next, err := check(s)
	if err != nil {
		return agentidentity.AgentSession{}, err
	}
	m.sessions[digest] = next
	return next, nil
}

func (m *memStore) AppendEvent(_ context.Context, e securityaudit.SecurityEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, e)
	return nil
}

type allowModes struct{}

func (allowModes) AllowsIdentityMode(identity.IdentityMode) bool { return true }

type assertedAllowed struct{ allowModes }

func (assertedAllowed) AllowsAssertedUser(identity.IdentityMode) bool { return true }

// fixture is the real broker over the real provider.
type fixture struct {
	store    *memStore
	provider *eliteanative.Provider
	broker   *agentidentity.Broker
	es, rs   *countingSigner
	now      *time.Time
}

func newFixture(t *testing.T, cfg eliteanative.Config, policy agentidentity.SessionPolicy) *fixture {
	t.Helper()
	esPEM, _ := ecPEM(t)
	rsPEMBytes, _ := rsaPEM(t)
	f := &fixture{store: newMemStore(), now: new(time.Time)}
	*f.now = fixedNow
	f.es = &countingSigner{KeySigner: localSigner(t, "es-1", eliteanative.ES256, esPEM)}
	f.rs = &countingSigner{KeySigner: localSigner(t, "rs-1", eliteanative.RS256, rsPEMBytes)}
	ring, err := eliteanative.NewKeyRing([]eliteanative.RingKey{{Signer: f.es}, {Signer: f.rs}}, cfg.RotationOverlap, 16*time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	clock := func() time.Time { return *f.now }
	f.provider, err = eliteanative.New(cfg, ring, f.store, eliteanative.WithClock(clock))
	if err != nil {
		t.Fatal(err)
	}
	registry, err := agentidentity.NewRegistry(f.provider)
	if err != nil {
		t.Fatal(err)
	}
	if policy == nil {
		policy = agentidentity.DenyAssertedUser{ModePolicy: allowModes{}}
	}
	f.broker, err = agentidentity.NewBroker(agentidentity.BrokerConfig{
		Registry: registry, Policy: policy, Bindings: f.store, Identities: f.store, Sessions: f.store, Now: clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) signs() int64 { return f.es.calls.Load() + f.rs.calls.Load() }

func admission(expires time.Duration) agentidentity.SessionAdmission {
	return agentidentity.SessionAdmission{
		TenantProjectID: tenantProject, ExecutionID: executionID, Generation: 4,
		AgentBindingID: bindingID, AgentBindingEpoch: 2, Profile: agentidentity.ProfileAutonomousAttributed,
		Ceiling:   agentidentity.Narrowing{Audiences: []string{awsAudience, vaultAudience, "https://not-configured.example.com"}},
		ExpiresAt: fixedNow.Add(expires), PolicyRevisionID: "2026-10-r1",
	}
}

func claim(ref agentidentity.SessionRef) agentidentity.SessionClaim {
	return agentidentity.SessionClaim{Ref: ref, ExecutionID: executionID, Generation: 4, WorkerClaimRef: workerClaim}
}

// open opens a session and answers a credential for audience.
func (f *fixture) credential(t *testing.T, in agentidentity.SessionAdmission, audience string) (agentidentity.SessionCredential, error) {
	t.Helper()
	ref, err := f.broker.OpenSession(context.Background(), in)
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	return f.broker.Credential(context.Background(), claim(ref), agentidentity.CredentialRequest{Audience: audience})
}
