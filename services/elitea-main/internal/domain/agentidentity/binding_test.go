package agentidentity

import (
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

type allowModes []IdentityMode

func (a allowModes) AllowsIdentityMode(mode IdentityMode) bool {
	for _, m := range a {
		if m == mode {
			return true
		}
	}
	return false
}

const testBindingID = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e0f11"

func validProposal(mode IdentityMode) Proposal {
	p := Proposal{
		TenantProjectID: 7, ApplicationID: 11, ApplicationVersionID: 13,
		DefinitionDigest: [32]byte{1, 2, 3}, Mode: mode,
		ExternalAgentIdentityID: "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e0f22",
	}
	if mode == IdentityModeRuntimeDomainBinding {
		p.WorkloadIdentity = "spiffe://elitea.local/runtime/worker"
	}
	return p
}

func bindingIn(t *testing.T, state BindingState) AgentIdentityBinding {
	t.Helper()
	b, err := NewBinding(testBindingID, validProposal(IdentityModeDedicatedAgent), allowModes(IdentityModes()), "rev-1", "100", time.Unix(0, 0))
	if err != nil {
		t.Fatalf("NewBinding: %v", err)
	}
	b.State = state
	b.RevocationEpoch = 5
	if state == BindingActive || state == BindingSuspended {
		b.ApprovedBy = "200"
	}
	return b
}

// Every (state, action) pair: the allowed ones move to the documented state
// and bump the epoch by exactly one; every other pair is ErrInvalidTransition
// and changes nothing.
func TestBindingTransitionTableIsComplete(t *testing.T) {
	allowed := map[BindingState]map[Action]BindingState{
		BindingProposed:  {ActionApprove: BindingActive, ActionRevoke: BindingRevoked},
		BindingActive:    {ActionSuspend: BindingSuspended, ActionRevoke: BindingRevoked},
		BindingSuspended: {ActionApprove: BindingActive, ActionRevoke: BindingRevoked},
		BindingRevoked:   {},
	}
	now := time.Unix(1000, 0).UTC()
	pairs := 0
	for _, state := range BindingStates() {
		for _, action := range Actions() {
			pairs++
			current := bindingIn(t, state)
			next, err := current.Transition(action, "300", "rev-2", now)
			want, ok := allowed[state][action]
			if !ok {
				if !errors.Is(err, ErrInvalidTransition) {
					t.Errorf("%s --%s--> err = %v, want ErrInvalidTransition", state, action, err)
				}
				continue
			}
			if err != nil {
				t.Errorf("%s --%s--> err = %v, want %s", state, action, err, want)
				continue
			}
			if next.State != want {
				t.Errorf("%s --%s--> %s, want %s", state, action, next.State, want)
			}
			if next.RevocationEpoch != current.RevocationEpoch+1 {
				t.Errorf("%s --%s--> epoch %d, want %d", state, action, next.RevocationEpoch, current.RevocationEpoch+1)
			}
			if !next.UpdatedAt.Equal(now) {
				t.Errorf("%s --%s--> updated_at not stamped", state, action)
			}
			if action == ActionApprove && (next.ApprovedBy != "300" || next.PolicyRevisionID != "rev-2") {
				t.Errorf("approval did not record approver and revision: %+v", next)
			}
			if action != ActionApprove && next.PolicyRevisionID != current.PolicyRevisionID {
				t.Errorf("%s changed the decision revision", action)
			}
		}
	}
	if pairs != 12 {
		t.Fatalf("covered %d pairs, want 12", pairs)
	}
	if _, err := NextState("bogus", ActionRevoke); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("unknown state revoked: %v", err)
	}
	if _, err := NextState(BindingProposed, "bogus"); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("unknown action allowed: %v", err)
	}
}

func TestBindingApprovalByTheProposerIsRefused(t *testing.T) {
	for _, state := range []BindingState{BindingProposed, BindingSuspended} {
		b := bindingIn(t, state)
		if _, err := b.Transition(ActionApprove, b.ProposedBy, "rev-1", time.Now()); !errors.Is(err, ErrSelfApproval) {
			t.Errorf("%s: approval by proposer = %v, want ErrSelfApproval", state, err)
		}
	}
	b := bindingIn(t, BindingActive)
	if _, err := b.Transition(ActionSuspend, b.ProposedBy, "", time.Now()); err != nil {
		t.Errorf("the proposer may suspend: %v", err)
	}
}

func TestNewBindingRefusesAModeThePolicyDoesNotAllow(t *testing.T) {
	for _, mode := range IdentityModes() {
		others := allowModes{}
		for _, m := range IdentityModes() {
			if m != mode {
				others = append(others, m)
			}
		}
		if _, err := NewBinding(testBindingID, validProposal(mode), others, "rev-1", "100", time.Now()); !errors.Is(err, ErrIdentityModeNotAllowed) {
			t.Errorf("%s with it not allowed: %v", mode, err)
		}
		if _, err := NewBinding(testBindingID, validProposal(mode), allowModes{}, "rev-1", "100", time.Now()); !errors.Is(err, ErrIdentityModeNotAllowed) {
			t.Errorf("%s under the default (empty) revision: %v", mode, err)
		}
		b, err := NewBinding(testBindingID, validProposal(mode), allowModes{mode}, "rev-1", "100", time.Now())
		if err != nil || b.State != BindingProposed || b.RevocationEpoch != 0 {
			t.Errorf("%s allowed: %+v %v", mode, b, err)
		}
	}
	if _, err := NewBinding(testBindingID, validProposal(IdentityModeEliteaNative), nil, "rev-1", "100", time.Now()); !errors.Is(err, ErrIdentityModeNotAllowed) {
		t.Errorf("nil policy: %v", err)
	}
}

func TestProposalValidation(t *testing.T) {
	cases := map[string]func(*Proposal){
		"tenant":            func(p *Proposal) { p.TenantProjectID = 0 },
		"application":       func(p *Proposal) { p.ApplicationID = -1 },
		"version":           func(p *Proposal) { p.ApplicationVersionID = 0 },
		"digest":            func(p *Proposal) { p.DefinitionDigest = [32]byte{} },
		"mode":              func(p *Proposal) { p.Mode = "personal" },
		"external id":       func(p *Proposal) { p.ExternalAgentIdentityID = "not-a-uuid" },
		"stray workload id": func(p *Proposal) { p.WorkloadIdentity = "spiffe://x" },
	}
	for name, mutate := range cases {
		p := validProposal(IdentityModeDedicatedAgent)
		mutate(&p)
		if err := p.Validate(); !errors.Is(err, ErrInvalidBinding) {
			t.Errorf("%s: %v", name, err)
		}
	}
	p := validProposal(IdentityModeRuntimeDomainBinding)
	p.WorkloadIdentity = "https://not-spiffe"
	if err := p.Validate(); !errors.Is(err, ErrInvalidBinding) {
		t.Errorf("runtime_domain_binding without spiffe id: %v", err)
	}
	p.ExternalAgentIdentityID = ""
	p.WorkloadIdentity = "spiffe://elitea.local/runtime"
	if err := p.Validate(); err != nil {
		t.Errorf("an external id is optional at proposal: %v", err)
	}
}

// The binding stores references only. A new field must be added here on
// purpose, after checking it holds no token, assertion, secret or key.
func TestBindingFieldSetHoldsNoSecretMaterial(t *testing.T) {
	assertFieldSet(t, reflect.TypeOf(AgentIdentityBinding{}), []string{
		"ApplicationID", "ApplicationVersionID", "ApprovedBy", "CreatedAt", "DefinitionDigest",
		"ExternalAgentIdentityID", "ID", "LastTransitionBy", "Mode", "PolicyRevisionID", "ProposedBy", "RevocationEpoch",
		"State", "TenantProjectID", "UpdatedAt", "WorkloadIdentity",
	})
}

func assertFieldSet(t *testing.T, typ reflect.Type, want []string) {
	t.Helper()
	var got []string
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		got = append(got, name)
		lower := strings.ToLower(name)
		for _, banned := range []string{"token", "secret", "password", "assertion", "credential", "key", "cipher"} {
			if strings.Contains(lower, banned) {
				t.Errorf("%s.%s looks like secret material", typ.Name(), name)
			}
		}
	}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s fields = %v, want %v", typ.Name(), got, want)
	}
}
