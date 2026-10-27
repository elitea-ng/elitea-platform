package reconcile_test

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/agentidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/agentidentity/reconcile"
	identity "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// memStore is an in-memory reconcile.Store and
// agentidentity.ExternalIdentityStore over one clock. Every event is
// validated like the real appender would.
type memStore struct {
	mu         sync.Mutex
	now        func() time.Time
	identities map[string]identity.ExternalAgentIdentity
	bindings   map[string]int // active bindings per identity
	events     []securityaudit.SecurityEvent
	findings   []reconcile.Finding
	resolved   map[string]bool
	health     map[string]*memHealth
	seq        int
	// recordsErr, when set, is what Records answers: a store failure.
	recordsErr error
}

type memHealth struct {
	reconcile.ProviderHealth
	holder     string
	leaseUntil time.Time
}

func newMemStore(now func() time.Time) *memStore {
	return &memStore{now: now, identities: map[string]identity.ExternalAgentIdentity{}, bindings: map[string]int{},
		resolved: map[string]bool{}, health: map[string]*memHealth{}}
}

func (m *memStore) appendLocked(e securityaudit.SecurityEvent) error {
	m.seq++
	if e.ID == "" {
		e.ID = fmt.Sprintf("0192f5e2-7c3a-7b1e-8a9d-%012d", m.seq)
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

func (m *memStore) Identity(id string) identity.ExternalAgentIdentity {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.identities[id]
}

func (m *memStore) put(e identity.ExternalAgentIdentity) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.identities[e.ID] = e
	m.bindings[e.ID] = 1
}

// ---- reconcile.Store ----

func (m *memStore) ClaimPass(_ context.Context, provider, holder string, lease time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.health[provider]
	if !ok {
		h = &memHealth{ProviderHealth: reconcile.ProviderHealth{Provider: provider}}
		m.health[provider] = h
	}
	now := m.now()
	if now.Before(h.leaseUntil) {
		return false, nil
	}
	h.holder, h.leaseUntil = holder, now.Add(lease)
	return true, nil
}

func (m *memStore) Records(_ context.Context, provider string) ([]identity.ExternalAgentIdentity, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.recordsErr != nil {
		return nil, m.recordsErr
	}
	var out []identity.ExternalAgentIdentity
	for _, e := range m.identities {
		if e.Ref.Provider == provider {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *memStore) transitionLocked(id string, apply func(identity.ExternalAgentIdentity, int) (identity.ExternalAgentIdentity, error)) (identity.ExternalAgentIdentity, identity.ExternalAgentIdentity, error) {
	current, ok := m.identities[id]
	if !ok {
		return identity.ExternalAgentIdentity{}, identity.ExternalAgentIdentity{}, identity.ErrExternalIdentityNotFound
	}
	next, err := apply(current, m.bindings[id])
	if err != nil {
		return identity.ExternalAgentIdentity{}, identity.ExternalAgentIdentity{}, err
	}
	if next.RevocationEpoch != current.RevocationEpoch+1 || next.ID != current.ID {
		return identity.ExternalAgentIdentity{}, identity.ExternalAgentIdentity{}, errors.New("memstore: a transition must bump the epoch by one")
	}
	return current, next, nil
}

func changedEvent(e identity.ExternalAgentIdentity) securityaudit.SecurityEvent {
	return securityaudit.SecurityEvent{Type: securityaudit.TypeExternalIdentityChanged, Time: e.UpdatedAt,
		TenantProjectID: e.TenantProjectID, Action: "external_identity." + string(e.State), PrincipalRef: e.LastTransitionBy,
		AuthorityID: "external_agent_identity:" + e.ID, ResourceType: "external_agent_identity", ResourceID: e.ID,
		RevocationEpoch: e.RevocationEpoch}
}

func (m *memStore) ApplyDrift(_ context.Context, id string, change reconcile.DriftChange) (identity.ExternalAgentIdentity, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, next, err := m.transitionLocked(id, change.Apply)
	if err != nil {
		return identity.ExternalAgentIdentity{}, err
	}
	saved := append([]securityaudit.SecurityEvent(nil), m.events...)
	if err := m.appendLocked(changedEvent(next)); err != nil {
		return identity.ExternalAgentIdentity{}, err
	}
	if err := m.appendLocked(change.Event(next)); err != nil {
		m.events = saved
		return identity.ExternalAgentIdentity{}, err
	}
	m.identities[id] = next
	finding := change.Finding
	finding.RevocationEpoch = next.RevocationEpoch
	m.upsertFindingLocked(finding)
	return next, nil
}

func (m *memStore) upsertFindingLocked(f reconcile.Finding) bool {
	for i, open := range m.findings {
		if !m.resolved[open.ID] && open.Kind == f.Kind && open.Provider == f.Provider &&
			open.ProviderTenant == f.ProviderTenant && open.ExternalID == f.ExternalID {
			m.findings[i].LastSeenAt = f.LastSeenAt
			if f.RevocationEpoch > 0 {
				m.findings[i].RevocationEpoch = f.RevocationEpoch
			}
			return false
		}
	}
	m.seq++
	f.ID = fmt.Sprintf("0192f5e2-7c3a-7b1e-8a9d-f%011d", m.seq)
	m.findings = append(m.findings, f)
	return true
}

func (m *memStore) RecordOrphan(_ context.Context, f reconcile.Finding, event securityaudit.SecurityEvent) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	saved := m.findings
	if !m.upsertFindingLocked(f) {
		return false, nil
	}
	if err := m.appendLocked(event); err != nil {
		m.findings = saved
		return false, err
	}
	return true, nil
}

func (m *memStore) OpenFindings(_ context.Context, provider string, limit, offset int) ([]reconcile.Finding, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []reconcile.Finding
	for i := len(m.findings) - 1; i >= 0; i-- {
		f := m.findings[i]
		if !m.resolved[f.ID] && (provider == "" || f.Provider == provider) {
			out = append(out, f)
		}
	}
	if offset >= len(out) {
		return nil, nil
	}
	return out[offset:min(offset+limit, len(out))], nil
}

func (m *memStore) ResolveFindings(_ context.Context, ids []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range ids {
		m.resolved[id] = true
	}
	return nil
}

func (m *memStore) RecordPass(_ context.Context, provider string, ok bool, threshold int) (reconcile.ProviderHealth, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	h, found := m.health[provider]
	if !found {
		h = &memHealth{ProviderHealth: reconcile.ProviderHealth{Provider: provider}}
		m.health[provider] = h
	}
	now := m.now()
	h.LastPassAt = now
	if ok {
		h.ConsecutiveFailures, h.AdmissionDenied, h.LastSuccessAt = 0, false, now
	} else {
		h.ConsecutiveFailures++
		h.AdmissionDenied = h.ConsecutiveFailures >= threshold
	}
	return h.ProviderHealth, nil
}

func (m *memStore) Health(context.Context) ([]reconcile.ProviderHealth, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []reconcile.ProviderHealth
	for _, h := range m.health {
		out = append(out, h.ProviderHealth)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Provider < out[j].Provider })
	return out, nil
}

// ProviderAdmissionDenied makes the store the broker's health reader too.
func (m *memStore) ProviderAdmissionDenied(_ context.Context, provider string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.health[provider]
	return ok && h.AdmissionDenied, nil
}

// ---- agentidentity.ExternalIdentityStore, for the admin service ----

func (m *memStore) CreateExternalIdentity(_ context.Context, e identity.ExternalAgentIdentity) (identity.ExternalAgentIdentity, error) {
	m.put(e)
	return e, nil
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
	return nil, errors.New("memstore: not used")
}

func (m *memStore) TransitionExternalIdentity(_ context.Context, id string, apply func(identity.ExternalAgentIdentity, int) (identity.ExternalAgentIdentity, error)) (identity.ExternalAgentIdentity, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, next, err := m.transitionLocked(id, apply)
	if err != nil {
		return identity.ExternalAgentIdentity{}, err
	}
	if err := m.appendLocked(changedEvent(next)); err != nil {
		return identity.ExternalAgentIdentity{}, err
	}
	m.identities[id] = next
	return next, nil
}

func (m *memStore) ActiveBindingCount(_ context.Context, _ int64, id string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.bindings[id], nil
}

var (
	_ reconcile.Store                     = (*memStore)(nil)
	_ agentidentity.ExternalIdentityStore = (*memStore)(nil)
	_ agentidentity.ProviderAdmission     = (*memStore)(nil)
)
