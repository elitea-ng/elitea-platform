package governance

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialgovernance/profile"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/mandate"
)

type memoryBindings struct {
	mu   sync.Mutex
	rows map[string]agentidentity.AgentIdentityBinding
}

func (m *memoryBindings) CreateBinding(_ context.Context, b agentidentity.AgentIdentityBinding) (agentidentity.AgentIdentityBinding, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows[b.ID] = b
	return b, nil
}

func (m *memoryBindings) Get(_ context.Context, id string) (agentidentity.AgentIdentityBinding, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.rows[id]
	if !ok {
		return b, agentidentity.ErrBindingNotFound
	}
	return b, nil
}

func (m *memoryBindings) ListBindings(context.Context, agentidentity.ListFilter) ([]agentidentity.AgentIdentityBinding, error) {
	return nil, nil
}

func (m *memoryBindings) TransitionBinding(_ context.Context, id string, apply func(agentidentity.AgentIdentityBinding) (agentidentity.AgentIdentityBinding, error)) (agentidentity.AgentIdentityBinding, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.rows[id]
	if !ok {
		return b, agentidentity.ErrBindingNotFound
	}
	next, err := apply(b)
	if err != nil {
		return next, err
	}
	m.rows[id] = next
	return next, nil
}

// memoryMandates mirrors the store contract: CreateMandate and
// TransitionMandate read the binding (and the delegation) and hand them to
// the service's check, as the PostgreSQL store does under its locks.
type memoryMandates struct {
	mu          sync.Mutex
	rows        map[string]mandate.Mandate
	bindings    *memoryBindings
	delegations map[string]mandate.Delegation
}

func (m *memoryMandates) CreateMandate(ctx context.Context, v mandate.Mandate, check func(agentidentity.AgentIdentityBinding, *mandate.Delegation) error) (mandate.Mandate, error) {
	binding, err := m.bindings.Get(ctx, v.AgentBindingID)
	if errors.Is(err, agentidentity.ErrBindingNotFound) {
		return mandate.Mandate{}, mandate.ErrBindingNotUsable
	}
	if err != nil {
		return mandate.Mandate{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var delegation *mandate.Delegation
	if v.DelegationID != nil {
		if d, ok := m.delegations[*v.DelegationID]; ok {
			delegation = &d
		}
	}
	if err := check(binding, delegation); err != nil {
		return mandate.Mandate{}, err
	}
	m.rows[v.ID] = v
	return v, nil
}

func (m *memoryMandates) Get(_ context.Context, id string) (mandate.Mandate, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.rows[id]
	if !ok {
		return v, mandate.ErrMandateNotFound
	}
	return v, nil
}

func (m *memoryMandates) ListMandates(context.Context, mandate.ListFilter) ([]mandate.Mandate, error) {
	return nil, nil
}

func (m *memoryMandates) TransitionMandate(ctx context.Context, id string, apply func(mandate.Mandate, agentidentity.AgentIdentityBinding) (mandate.Mandate, error)) (mandate.Mandate, error) {
	m.mu.Lock()
	v, ok := m.rows[id]
	m.mu.Unlock()
	if !ok {
		return v, mandate.ErrMandateNotFound
	}
	binding, err := m.bindings.Get(ctx, v.AgentBindingID)
	if err != nil {
		return mandate.Mandate{}, err
	}
	next, err := apply(v, binding)
	if err != nil {
		return next, err
	}
	m.mu.Lock()
	m.rows[id] = next
	m.mu.Unlock()
	return next, nil
}

type externalStates map[string]agentidentity.ExternalIdentityState

func (e externalStates) ExternalIdentityState(_ context.Context, _ int64, id string) (agentidentity.ExternalIdentityState, error) {
	state, ok := e[id]
	if !ok {
		return "", errors.New("unknown")
	}
	return state, nil
}

type mutableRevision struct {
	mu       sync.Mutex
	revision profile.PolicyRevision
	err      error
}

func (r *mutableRevision) ActivePolicyRevision(context.Context) (profile.PolicyRevision, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.revision, r.err
}

func (r *mutableRevision) set(modes []agentidentity.IdentityMode, ttl time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.revision = profile.PolicyRevision{ID: "rev-test", Document: profile.PolicyDocument{
		Schema: profile.PolicySchemaV1, AllowedIdentityModes: modes, MaxMandateTTLSeconds: int64(ttl / time.Second),
		AuditBacklogAlertThreshold: 1,
	}}
}

const externalID = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e0f22"

type fixture struct {
	service   *Service
	bindings  *memoryBindings
	mandates  *memoryMandates
	revisions *mutableRevision
	external  externalStates
	now       time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{revisions: &mutableRevision{}, external: externalStates{externalID: agentidentity.ExternalIdentityActive},
		now: time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)}
	f.revisions.set(agentidentity.IdentityModes(), 24*time.Hour)
	ids := []string{
		"0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e0a01", "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e0a02",
		"0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e0a03", "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e0a04",
	}
	next := 0
	f.bindings = &memoryBindings{rows: map[string]agentidentity.AgentIdentityBinding{}}
	f.mandates = &memoryMandates{rows: map[string]mandate.Mandate{}, bindings: f.bindings, delegations: map[string]mandate.Delegation{}}
	service, err := New(Config{
		Revisions:          f.revisions,
		Bindings:           f.bindings,
		Mandates:           f.mandates,
		ExternalIdentities: f.external,
		Now:                func() time.Time { return f.now },
		NewID: func() (string, error) {
			id := ids[next%len(ids)]
			next++
			return id, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.service = service
	return f
}

func proposal(external string) agentidentity.Proposal {
	return agentidentity.Proposal{
		TenantProjectID: 7, ApplicationID: 1, ApplicationVersionID: 2, DefinitionDigest: [32]byte{9},
		Mode: agentidentity.IdentityModeDedicatedAgent, ExternalAgentIdentityID: external,
	}
}

func TestNewRefusesMissingDependencies(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("New with no dependencies succeeded")
	}
}

func TestProposeBindingAppliesTheActiveRevision(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.revisions.set(nil, time.Hour) // the default revision: no mode allowed
	if _, err := f.service.ProposeBinding(ctx, proposal(externalID), "100"); !errors.Is(err, agentidentity.ErrIdentityModeNotAllowed) {
		t.Fatalf("propose under the default revision: %v", err)
	}
	f.revisions.mu.Lock()
	f.revisions.err = errors.New("db down")
	f.revisions.mu.Unlock()
	if _, err := f.service.ProposeBinding(ctx, proposal(externalID), "100"); !errors.Is(err, ErrPolicyUnavailable) {
		t.Fatalf("propose without a revision: %v", err)
	}
}

func TestApproveBindingChecksModeExternalIdentityAndApprover(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	b, err := f.service.ProposeBinding(ctx, proposal(externalID), "100")
	if err != nil || b.PolicyRevisionID != "rev-test" {
		t.Fatalf("propose: %+v %v", b, err)
	}
	if _, err := f.service.TransitionBinding(ctx, b.ID, agentidentity.ActionApprove, "100"); !errors.Is(err, agentidentity.ErrSelfApproval) {
		t.Fatalf("self approval: %v", err)
	}
	f.external[externalID] = agentidentity.ExternalIdentitySuspended
	if _, err := f.service.TransitionBinding(ctx, b.ID, agentidentity.ActionApprove, "200"); !errors.Is(err, agentidentity.ErrExternalIdentityNotActive) {
		t.Fatalf("approve with a suspended external identity: %v", err)
	}
	f.external[externalID] = agentidentity.ExternalIdentityActive
	f.revisions.set([]agentidentity.IdentityMode{agentidentity.IdentityModeEliteaNative}, time.Hour)
	if _, err := f.service.TransitionBinding(ctx, b.ID, agentidentity.ActionApprove, "200"); !errors.Is(err, agentidentity.ErrIdentityModeNotAllowed) {
		t.Fatalf("approve after the mode left the revision: %v", err)
	}
	f.revisions.set(agentidentity.IdentityModes(), time.Hour)
	active, err := f.service.TransitionBinding(ctx, b.ID, agentidentity.ActionApprove, "200")
	if err != nil || active.State != agentidentity.BindingActive || active.RevocationEpoch != 1 || active.ApprovedBy != "200" {
		t.Fatalf("approve: %+v %v", active, err)
	}
	// Suspend never depends on policy.
	f.revisions.mu.Lock()
	f.revisions.err = errors.New("db down")
	f.revisions.mu.Unlock()
	suspended, err := f.service.TransitionBinding(ctx, b.ID, agentidentity.ActionSuspend, "100")
	if err != nil || suspended.State != agentidentity.BindingSuspended || suspended.RevocationEpoch != 2 {
		t.Fatalf("suspend without a revision: %+v %v", suspended, err)
	}
}

func TestBindingWithoutExternalIdentityCannotActivate(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	b, err := f.service.ProposeBinding(ctx, proposal(""), "100")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.TransitionBinding(ctx, b.ID, agentidentity.ActionApprove, "200"); !errors.Is(err, agentidentity.ErrExternalIdentityNotActive) {
		t.Fatalf("approve with no external identity: %v", err)
	}
}

// The wiring until #1151: nothing activates.
func TestFailClosedExternalIdentitiesBlockEveryActivation(t *testing.T) {
	f := newFixture(t)
	f.service.external = agentidentity.FailClosedExternalIdentities{}
	ctx := context.Background()
	b, err := f.service.ProposeBinding(ctx, proposal(externalID), "100")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.TransitionBinding(ctx, b.ID, agentidentity.ActionApprove, "200"); !errors.Is(err, agentidentity.ErrExternalIdentityNotActive) {
		t.Fatalf("fail-closed wiring let a binding activate: %v", err)
	}
}

func TestMandateIssueAndApprovalFollowTheBinding(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	b, err := f.service.ProposeBinding(ctx, proposal(externalID), "100")
	if err != nil {
		t.Fatal(err)
	}
	req := mandate.Request{
		TenantProjectID: 7, Kind: mandate.KindAutonomous, AgentBindingID: b.ID,
		OperationScopes: []string{"github.issues.read"}, ExpiresAt: f.now.Add(2 * time.Hour),
	}
	other := req
	other.TenantProjectID = 8
	if _, err := f.service.IssueMandate(ctx, other, "100"); !errors.Is(err, mandate.ErrBindingNotUsable) {
		t.Fatalf("mandate for another tenant's binding: %v", err)
	}
	tooLong := req
	tooLong.ExpiresAt = f.now.Add(25 * time.Hour)
	if _, err := f.service.IssueMandate(ctx, tooLong, "100"); !errors.Is(err, mandate.ErrTTLExceedsPolicy) {
		t.Fatalf("mandate beyond policy max: %v", err)
	}
	m, err := f.service.IssueMandate(ctx, req, "100")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if _, err := f.service.TransitionMandate(ctx, m.ID, mandate.ActionApprove, "200"); !errors.Is(err, mandate.ErrBindingNotUsable) {
		t.Fatalf("approve a mandate of a proposed binding: %v", err)
	}
	if _, err := f.service.TransitionBinding(ctx, b.ID, agentidentity.ActionApprove, "200"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.TransitionMandate(ctx, m.ID, mandate.ActionApprove, "100"); !errors.Is(err, mandate.ErrSelfApproval) {
		t.Fatalf("issuer approval: %v", err)
	}
	f.revisions.set(agentidentity.IdentityModes(), time.Hour)
	if _, err := f.service.TransitionMandate(ctx, m.ID, mandate.ActionApprove, "200"); !errors.Is(err, mandate.ErrTTLExceedsPolicy) {
		t.Fatalf("approve after the revision shortened the max: %v", err)
	}
	f.revisions.set(agentidentity.IdentityModes(), 24*time.Hour)
	active, err := f.service.TransitionMandate(ctx, m.ID, mandate.ActionApprove, "200")
	if err != nil || active.State != mandate.StateActive || active.RevocationEpoch != 1 {
		t.Fatalf("approve: %+v %v", active, err)
	}
	if _, err := f.service.TransitionBinding(ctx, b.ID, agentidentity.ActionRevoke, "100"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.IssueMandate(ctx, req, "100"); !errors.Is(err, mandate.ErrBindingNotUsable) {
		t.Fatalf("mandate for a revoked binding: %v", err)
	}
}

func TestIssueMandateRefusesADelegationOfAnotherTenantOrBinding(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	b, err := f.service.ProposeBinding(ctx, proposal(externalID), "100")
	if err != nil {
		t.Fatal(err)
	}
	const (
		sameScope   = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e0d01"
		otherTenant = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e0d02"
		otherBind   = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e0d03"
		revoked     = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e0d04"
		missing     = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e0d05"
	)
	live := func(id string, tenant int64, binding string, state mandate.State) mandate.Delegation {
		return mandate.Delegation{ID: id, TenantProjectID: tenant, AgentBindingID: binding, State: state, ExpiresAt: f.now.Add(time.Hour)}
	}
	f.mandates.delegations[sameScope] = live(sameScope, 7, b.ID, mandate.StateActive)
	f.mandates.delegations[otherTenant] = live(otherTenant, 8, b.ID, mandate.StateActive)
	f.mandates.delegations[otherBind] = live(otherBind, 7, "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e0fff", mandate.StateActive)
	f.mandates.delegations[revoked] = live(revoked, 7, b.ID, mandate.StateRevoked)

	subject := int64(5)
	request := func(delegation string) mandate.Request {
		return mandate.Request{
			TenantProjectID: 7, Kind: mandate.KindDelegatedBackground, AgentBindingID: b.ID,
			SubjectUserID: &subject, DelegationID: &delegation,
			OperationScopes: []string{"github.issues.read"}, ExpiresAt: f.now.Add(time.Hour),
		}
	}
	for _, id := range []string{otherTenant, otherBind, revoked, missing} {
		if _, err := f.service.IssueMandate(ctx, request(id), "100"); !errors.Is(err, mandate.ErrDelegationNotUsable) {
			t.Errorf("delegation %s: %v, want ErrDelegationNotUsable", id, err)
		}
	}
	if len(f.mandates.rows) != 0 {
		t.Fatalf("a refused mandate was written: %d rows", len(f.mandates.rows))
	}
	m, err := f.service.IssueMandate(ctx, request(sameScope), "100")
	if err != nil || m.DelegationID == nil || *m.DelegationID != sameScope || m.LastTransitionBy != "100" {
		t.Fatalf("issue with a delegation of the same scope: %+v %v", m, err)
	}
}

// The approval check reads the binding the store hands it (locked in the
// mandate's transaction), not a second, unlocked read.
func TestMandateApprovalReadsTheBindingTheStoreLocked(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	b, err := f.service.ProposeBinding(ctx, proposal(externalID), "100")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.TransitionBinding(ctx, b.ID, agentidentity.ActionApprove, "200"); err != nil {
		t.Fatal(err)
	}
	m, err := f.service.IssueMandate(ctx, mandate.Request{
		TenantProjectID: 7, Kind: mandate.KindAutonomous, AgentBindingID: b.ID,
		OperationScopes: []string{"a"}, ExpiresAt: f.now.Add(time.Hour),
	}, "100")
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []agentidentity.BindingState{agentidentity.BindingSuspended, agentidentity.BindingRevoked, agentidentity.BindingProposed} {
		f.bindings.mu.Lock()
		row := f.bindings.rows[b.ID]
		row.State = state
		f.bindings.rows[b.ID] = row
		f.bindings.mu.Unlock()
		if _, err := f.service.TransitionMandate(ctx, m.ID, mandate.ActionApprove, "200"); !errors.Is(err, mandate.ErrBindingNotUsable) {
			t.Errorf("approve with the binding %s: %v", state, err)
		}
	}
	// Suspend and revoke never depend on the binding's state.
	revoked, err := f.service.TransitionMandate(ctx, m.ID, mandate.ActionRevoke, "300")
	if err != nil || revoked.State != mandate.StateRevoked || revoked.LastTransitionBy != "300" {
		t.Fatalf("revoke: %+v %v", revoked, err)
	}
}
