// Package runmandatetest holds in-memory doubles of runmandate's
// dependencies, so the entry points' tests drive the real Service.
package runmandatetest

import (
	"context"
	"sync"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/runmandate"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialbroker/epoch"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/mandate"
)

// Store is the mandates, bindings and epochs, and the recorded refusals.
type Store struct {
	mu       sync.Mutex
	Mandates map[string]mandate.Mandate
	Bindings map[string]agentidentity.AgentIdentityBinding
	refusals []runmandate.Refusal
	// Kinds is the policy's unattended kinds; nil allows both.
	Kinds []string
	// Epochs overrides the epoch read for a record id, as a transition
	// committed between the row read and the epoch read would.
	Epochs map[string]int64
}

// New builds an empty store.
func New() *Store {
	return &Store{Mandates: map[string]mandate.Mandate{}, Bindings: map[string]agentidentity.AgentIdentityBinding{}}
}

// Service builds the real runmandate.Service over s.
func (s *Store) Service() *runmandate.Service {
	service, err := runmandate.New(runmandate.Config{
		Mandates: mandatesOf{s}, Bindings: bindingsOf{s}, Epochs: epochsOf{s}, Policy: policyOf{s}, Refusals: s,
	})
	if err != nil {
		panic(err)
	}
	return service
}

// PutBinding stores an active binding of versionID in tenant.
func (s *Store) PutBinding(id string, tenant, versionID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Bindings[id] = agentidentity.AgentIdentityBinding{
		ID: id, TenantProjectID: tenant, ApplicationVersionID: versionID,
		State: agentidentity.BindingActive, RevocationEpoch: 1,
	}
}

// PutMandate stores an active mandate of kind on binding, valid for an hour.
// subject is 0 for none.
func (s *Store) PutMandate(id string, tenant int64, kind mandate.Kind, binding string, subject int64, scopes ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := mandate.Mandate{
		ID: id, TenantProjectID: tenant, Kind: kind, AgentBindingID: binding,
		OperationScopes: []string{"jira.issue.read"}, ResourceScopes: append([]string{}, scopes...),
		NotBefore: time.Now().Add(-time.Hour), ExpiresAt: time.Now().Add(time.Hour),
		State: mandate.StateActive, RevocationEpoch: 2,
	}
	if subject > 0 {
		m.SubjectUserID = &subject
	}
	s.Mandates[id] = m
}

// Update changes a stored mandate.
func (s *Store) Update(id string, change func(*mandate.Mandate)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.Mandates[id]
	change(&m)
	s.Mandates[id] = m
}

// Refusals is what RecordRunRefusal received.
func (s *Store) Refusals() []runmandate.Refusal {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]runmandate.Refusal(nil), s.refusals...)
}

// RecordRunRefusal implements runmandate.RefusalRecorder.
func (s *Store) RecordRunRefusal(_ context.Context, refusal runmandate.Refusal) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refusals = append(s.refusals, refusal)
	return nil
}

type mandatesOf struct{ s *Store }

func (m mandatesOf) Get(_ context.Context, id string) (mandate.Mandate, error) {
	m.s.mu.Lock()
	defer m.s.mu.Unlock()
	found, ok := m.s.Mandates[id]
	if !ok {
		return mandate.Mandate{}, mandate.ErrMandateNotFound
	}
	return found, nil
}

type bindingsOf struct{ s *Store }

func (b bindingsOf) GetUsable(_ context.Context, id string) (agentidentity.AgentIdentityBinding, error) {
	b.s.mu.Lock()
	defer b.s.mu.Unlock()
	found, ok := b.s.Bindings[id]
	if !ok {
		return agentidentity.AgentIdentityBinding{}, agentidentity.ErrBindingNotFound
	}
	return found, nil
}

// epochsOf answers as epoch.PostgresStore does: the stored epoch of an
// active record, Revoked otherwise.
type epochsOf struct{ s *Store }

func (e epochsOf) Read(_ context.Context, keys []epoch.SecurityEpoch) ([]epoch.SecurityEpoch, error) {
	e.s.mu.Lock()
	defer e.s.mu.Unlock()
	now := time.Now()
	out := make([]epoch.SecurityEpoch, len(keys))
	for i, key := range keys {
		value := epoch.Revoked
		switch key.Kind {
		case epoch.KindMandate:
			if m, ok := e.s.Mandates[key.ID]; ok && m.State == mandate.StateActive && now.Before(m.ExpiresAt) {
				value = m.RevocationEpoch
			}
		case epoch.KindBinding:
			if b, ok := e.s.Bindings[key.ID]; ok && b.State == agentidentity.BindingActive {
				value = b.RevocationEpoch
			}
		case epoch.KindDelegation:
			value = 0
		}
		if override, ok := e.s.Epochs[key.ID]; ok {
			value = override
		}
		out[i] = epoch.SecurityEpoch{Kind: key.Kind, ID: key.ID, Value: value}
	}
	return out, nil
}

type policyOf struct{ s *Store }

func (p policyOf) AllowsUnattendedMandateKind(kind string) bool {
	if p.s.Kinds == nil {
		return kind == string(mandate.KindAutonomous) || kind == string(mandate.KindDelegatedBackground)
	}
	for _, allowed := range p.s.Kinds {
		if allowed == kind {
			return true
		}
	}
	return false
}
