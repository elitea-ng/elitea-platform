package credentialgrant

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

func validProposal() Proposal {
	return Proposal{
		TenantProjectID:       7,
		AgentBindingID:        "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e0f01",
		ToolkitID:             11,
		CredentialReferenceID: "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e0f03",
		TargetBindingID:       "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e0f04",
		Capabilities:          []Capability{CapUse},
		OperationScopes:       []string{"jira.issue.read"},
	}
}

func TestProposalValidation(t *testing.T) {
	if err := validProposal().Validate(now); err != nil {
		t.Fatalf("valid proposal refused: %v", err)
	}
	past := now.Add(-time.Second)
	cases := map[string]func(*Proposal){
		"tenant":             func(p *Proposal) { p.TenantProjectID = 0 },
		"toolkit":            func(p *Proposal) { p.ToolkitID = 0 },
		"binding":            func(p *Proposal) { p.AgentBindingID = "x" },
		"credential":         func(p *Proposal) { p.CredentialReferenceID = "" },
		"target":             func(p *Proposal) { p.TargetBindingID = "y" },
		"no capability":      func(p *Proposal) { p.Capabilities = nil },
		"unknown capability": func(p *Proposal) { p.Capabilities = []Capability{"admin"} },
		"repeat capability":  func(p *Proposal) { p.Capabilities = []Capability{CapUse, CapUse} },
		"no operation":       func(p *Proposal) { p.OperationScopes = nil },
		"bad operation":      func(p *Proposal) { p.OperationScopes = []string{"jira.*"} },
		"bad resource":       func(p *Proposal) { p.ResourceScopes = []string{"a b"} },
		"unknown obligation": func(p *Proposal) { p.Obligations = []string{"notify.slack"} },
		"expired":            func(p *Proposal) { p.ExpiresAt = &past },
	}
	for name, mutate := range cases {
		p := validProposal()
		mutate(&p)
		if err := p.Validate(now); !errors.Is(err, ErrInvalidGrant) {
			t.Errorf("%s: Validate = %v, want ErrInvalidGrant", name, err)
		}
	}
}

func TestGrantTransitionsAndTwoPersonRule(t *testing.T) {
	g, err := NewGrant("0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e0f06", validProposal(), "100", now)
	if err != nil {
		t.Fatal(err)
	}
	if g.State != GrantProposed || g.Epoch != 0 || g.ActiveAt(now) {
		t.Fatalf("new grant = %+v", g)
	}
	if _, err := g.Transition(ActionApprove, "100", now); !errors.Is(err, ErrSelfApproval) {
		t.Fatalf("approval by the proposer = %v, want ErrSelfApproval", err)
	}
	if _, err := g.Transition(ActionSuspend, "200", now); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("suspend of proposed = %v", err)
	}
	active, err := g.Transition(ActionApprove, "200", now)
	if err != nil || active.State != GrantActive || active.Epoch != 1 || active.ApprovedBy != "200" || !active.ActiveAt(now) {
		t.Fatalf("approve = %+v, %v", active, err)
	}
	suspended, err := active.Transition(ActionSuspend, "100", now)
	if err != nil || suspended.State != GrantSuspended || suspended.Epoch != 2 {
		t.Fatalf("suspend = %+v, %v", suspended, err)
	}
	if _, err := suspended.Transition(ActionApprove, "100", now); !errors.Is(err, ErrSelfApproval) {
		t.Fatalf("reactivation by the proposer = %v", err)
	}
	revoked, err := suspended.Transition(ActionRevoke, "100", now)
	if err != nil || revoked.State != GrantRevoked || revoked.Epoch != 3 {
		t.Fatalf("revoke = %+v, %v", revoked, err)
	}
	for _, action := range Actions() {
		if _, err := revoked.Transition(action, "300", now); !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("%s from revoked = %v", action, err)
		}
	}
}

func TestCapabilitiesAreSeparatePermissions(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range Capabilities() {
		p := Permission(c)
		if !strings.HasPrefix(p, "models.credentials.governed.") || seen[p] {
			t.Errorf("capability %s maps to %q", c, p)
		}
		seen[p] = true
	}
	if !reflect.DeepEqual(Permissions(), []string{
		"models.credentials.governed.use", "models.credentials.governed.reveal", "models.credentials.governed.manage",
		"models.credentials.governed.probe", "models.credentials.governed.approve",
	}) {
		t.Errorf("Permissions() = %v", Permissions())
	}
	if Permission("admin") != "" {
		t.Error("an unknown capability maps to a permission")
	}
	g := CredentialUseGrant{Capabilities: []Capability{CapUse}}
	for _, c := range []Capability{CapReveal, CapManage, CapProbe, CapApprove} {
		if g.Allows(c) {
			t.Errorf("a use grant allows %s", c)
		}
	}
}

func TestOperationIDShape(t *testing.T) {
	for id, want := range map[OperationID]bool{
		"jira.issue.read": true, "github.pull_request.merge": true,
		"jira.issue": false, "Jira.issue.read": false, "jira.issue.read.extra": false, "jira.*.read": false, "": false,
	} {
		if got := id.WellFormed(); got != want {
			t.Errorf("%q WellFormed = %v, want %v", id, got, want)
		}
	}
	if EmptyRegistry.Known("jira.issue.read") {
		t.Error("the empty registry knows an operation")
	}
}

type memoryStore struct {
	grants map[string]CredentialUseGrant
}

func (m *memoryStore) CreateGrant(_ context.Context, g CredentialUseGrant) (CredentialUseGrant, error) {
	m.grants[g.ID] = g
	return g, nil
}

func (m *memoryStore) ListGrants(context.Context, ListFilter) ([]CredentialUseGrant, error) {
	return nil, nil
}

func (m *memoryStore) TransitionGrant(_ context.Context, tenant int64, id string, apply func(CredentialUseGrant) (CredentialUseGrant, error)) (CredentialUseGrant, error) {
	g, ok := m.grants[id]
	if !ok || g.TenantProjectID != tenant {
		return CredentialUseGrant{}, ErrGrantNotFound
	}
	next, err := apply(g)
	if err != nil {
		return CredentialUseGrant{}, err
	}
	m.grants[id] = next
	return next, nil
}

func TestServiceProposeApproveAndTenantScope(t *testing.T) {
	store := &memoryStore{grants: map[string]CredentialUseGrant{}}
	clock := now
	svc, err := NewService(ServiceConfig{Store: store, Now: func() time.Time { return clock }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewService(ServiceConfig{}); err == nil {
		t.Fatal("a service without a store was built")
	}
	expires := now.Add(time.Hour)
	p := validProposal()
	p.ExpiresAt = &expires
	g, err := svc.ProposeGrant(context.Background(), p, "100")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.TransitionGrant(context.Background(), 7, g.ID, ActionApprove, "100"); !errors.Is(err, ErrSelfApproval) {
		t.Fatalf("self approval = %v", err)
	}
	if _, err := svc.TransitionGrant(context.Background(), 8, g.ID, ActionApprove, "200"); !errors.Is(err, ErrGrantNotFound) {
		t.Fatalf("approval through another tenant = %v", err)
	}
	clock = expires
	if _, err := svc.TransitionGrant(context.Background(), 7, g.ID, ActionApprove, "200"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("approval of an expired grant = %v", err)
	}
	if _, err := svc.ListGrants(context.Background(), ListFilter{}); !errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("list without a tenant = %v", err)
	}
}
