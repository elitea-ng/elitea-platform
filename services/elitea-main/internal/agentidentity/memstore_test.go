package agentidentity_test

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/agentidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialgovernance/profile"
	identity "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// memStore is an in-memory BindingReader, ExternalIdentityStore and
// SessionStore. Every event it appends is validated like the real appender
// would.
type memStore struct {
	mu         sync.Mutex
	bindings   map[string]identity.AgentIdentityBinding
	identities map[string]identity.ExternalAgentIdentity
	sessions   map[[32]byte]agentidentity.AgentSession
	events     []securityaudit.SecurityEvent
}

func newMemStore() *memStore {
	return &memStore{
		bindings:   map[string]identity.AgentIdentityBinding{},
		identities: map[string]identity.ExternalAgentIdentity{},
		sessions:   map[[32]byte]agentidentity.AgentSession{},
	}
}

func (m *memStore) appendLocked(e securityaudit.SecurityEvent) error {
	if e.ID == "" {
		e.ID = "0192f5e2-7c3a-7b1e-8a9d-000000000000"
	}
	if e.Source == "" {
		e.Source = "elitea-main/test"
	}
	if err := e.Validate(); err != nil {
		return err
	}
	m.events = append(m.events, e)
	return nil
}

func (m *memStore) Events() []securityaudit.SecurityEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]securityaudit.SecurityEvent(nil), m.events...)
}

// ---- BindingReader ----

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

// ---- ExternalIdentityStore ----

func (m *memStore) CreateExternalIdentity(_ context.Context, e identity.ExternalAgentIdentity) (identity.ExternalAgentIdentity, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, other := range m.identities {
		if !other.State.Terminal() && other.TenantProjectID == e.TenantProjectID && other.ApplicationID == e.ApplicationID &&
			other.Ref.Provider == e.Ref.Provider && other.Ref.ProviderTenant == e.Ref.ProviderTenant {
			return identity.ExternalAgentIdentity{}, identity.ErrDuplicateExternalIdentity
		}
	}
	m.identities[e.ID] = e
	return e, m.appendLocked(securityaudit.SecurityEvent{Type: securityaudit.TypeExternalIdentityChanged,
		Time: e.UpdatedAt, TenantProjectID: e.TenantProjectID, Action: "external_identity.create"})
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

func (m *memStore) ListExternalIdentities(context.Context, identity.ExternalListFilter) ([]identity.ExternalAgentIdentity, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []identity.ExternalAgentIdentity{}
	for _, e := range m.identities {
		out = append(out, e)
	}
	return out, nil
}

func (m *memStore) activeBindingsLocked(tenant int64, id string) int {
	n := 0
	for _, b := range m.bindings {
		if b.TenantProjectID == tenant && b.ExternalAgentIdentityID == id && b.State == identity.BindingActive {
			n++
		}
	}
	return n
}

func (m *memStore) ActiveBindingCount(_ context.Context, tenant int64, id string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.activeBindingsLocked(tenant, id), nil
}

func (m *memStore) TransitionExternalIdentity(_ context.Context, id string, apply func(identity.ExternalAgentIdentity, int) (identity.ExternalAgentIdentity, error)) (identity.ExternalAgentIdentity, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	current, ok := m.identities[id]
	if !ok {
		return identity.ExternalAgentIdentity{}, identity.ErrExternalIdentityNotFound
	}
	next, err := apply(current, m.activeBindingsLocked(current.TenantProjectID, id))
	if err != nil {
		return identity.ExternalAgentIdentity{}, err
	}
	if next.RevocationEpoch != current.RevocationEpoch+1 {
		return identity.ExternalAgentIdentity{}, errors.New("epoch must bump by one")
	}
	m.identities[id] = next
	return next, m.appendLocked(securityaudit.SecurityEvent{Type: securityaudit.TypeExternalIdentityChanged,
		Time: next.UpdatedAt, TenantProjectID: next.TenantProjectID, Action: "external_identity." + string(next.State),
		RevocationEpoch: next.RevocationEpoch})
}

// ---- SessionStore ----

func (m *memStore) CreateSession(_ context.Context, s agentidentity.AgentSession, digest [32]byte, event securityaudit.SecurityEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.appendLocked(event); err != nil {
		return err
	}
	m.sessions[digest] = s
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
	return m.appendLocked(e)
}

// allowAll is a policy revision document that allows every mode.
func allowAllRevision() profile.PolicyRevision {
	return profile.PolicyRevision{ID: "rev-1", Document: profile.PolicyDocument{AllowedIdentityModes: identity.IdentityModes()}}
}

type assertedAllowed struct{ identity.ModePolicy }

func (assertedAllowed) AllowsAssertedUser(identity.IdentityMode) bool { return true }

var fixedNow = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
