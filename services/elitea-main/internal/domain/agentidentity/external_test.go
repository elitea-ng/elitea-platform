package agentidentity

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

const testExternalID = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e0f22"

func validExternalRequest() ExternalIdentityRequest {
	return ExternalIdentityRequest{TenantProjectID: 7, ApplicationID: 11, Mode: IdentityModeDedicatedAgent,
		Provider: "entra", ProviderTenant: "72f988bf-86f1-41af-91ab-2d7cd011db47", ProviderConfigKey: "blueprint-a"}
}

// Every (state, action) pair: the allowed ones move to the documented state
// and bump the epoch by one; every other pair is refused.
func TestExternalTransitionTableIsComplete(t *testing.T) {
	nonTerminal := []ExternalIdentityState{ExternalIdentityProvisioning, ExternalIdentityPendingActivation,
		ExternalIdentityActive, ExternalIdentitySuspended, ExternalIdentityRetiring}
	allowed := map[ExternalIdentityState]map[ExternalAction]ExternalIdentityState{}
	for _, s := range nonTerminal {
		allowed[s] = map[ExternalAction]ExternalIdentityState{ExternalActionReplace: ExternalIdentityReplaced}
		if s != ExternalIdentityRetiring {
			allowed[s][ExternalActionRetire] = ExternalIdentityRetiring
		}
	}
	allowed[ExternalIdentityProvisioning][ExternalActionProvision] = ExternalIdentityPendingActivation
	allowed[ExternalIdentityPendingActivation][ExternalActionActivate] = ExternalIdentityActive
	allowed[ExternalIdentityActive][ExternalActionSuspend] = ExternalIdentitySuspended
	allowed[ExternalIdentitySuspended][ExternalActionResume] = ExternalIdentityActive
	allowed[ExternalIdentityRetiring][ExternalActionCompleteRetire] = ExternalIdentityRetired

	ref := ExternalRef{Provider: "entra", ProviderTenant: validExternalRequest().ProviderTenant, Kind: "entra.agent_identity", ID: "oid-1"}
	for _, state := range ExternalIdentityStateValues() {
		for _, action := range ExternalActions() {
			e, err := NewExternalIdentity(testExternalID, validExternalRequest(), allowModes(IdentityModes()), "100", time.Unix(0, 0))
			if err != nil {
				t.Fatal(err)
			}
			e.State, e.RevocationEpoch = state, 4
			next, err := e.Transition(action, "200", ref, "", time.Unix(1, 0))
			want, ok := allowed[state][action]
			if !ok {
				if !errors.Is(err, ErrExternalTransition) {
					t.Errorf("%s --%s--> allowed (%v)", state, action, err)
				}
				continue
			}
			if err != nil || next.State != want || next.RevocationEpoch != 5 || next.LastTransitionBy != "200" {
				t.Errorf("%s --%s--> %s epoch %d (%v), want %s epoch 5", state, action, next.State, next.RevocationEpoch, err, want)
			}
			if action == ExternalActionProvision && next.Ref != ref {
				t.Errorf("provision did not record the ref: %+v", next.Ref)
			}
			if action != ExternalActionProvision && next.Ref != e.Ref {
				t.Errorf("%s changed the ref", action)
			}
		}
	}
}

func TestProvisionRefusesARefForAnotherProviderOrTenant(t *testing.T) {
	e, err := NewExternalIdentity(testExternalID, validExternalRequest(), allowModes(IdentityModes()), "100", time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range []ExternalRef{
		{Provider: "okta", ProviderTenant: e.Ref.ProviderTenant, Kind: "okta.app", ID: "x"},
		{Provider: "entra", ProviderTenant: "other", Kind: "entra.agent_identity", ID: "x"},
		{Provider: "entra", ProviderTenant: e.Ref.ProviderTenant, Kind: "entra.agent_identity"},
	} {
		if _, err := e.Transition(ExternalActionProvision, "100", ref, "", time.Unix(1, 0)); !errors.Is(err, ErrInvalidExternalIdentity) {
			t.Errorf("ref %+v accepted: %v", ref, err)
		}
	}
}

func TestNewExternalIdentityValidates(t *testing.T) {
	bad := []func(*ExternalIdentityRequest){
		func(r *ExternalIdentityRequest) { r.TenantProjectID = 0 },
		func(r *ExternalIdentityRequest) { r.ApplicationID = 0 },
		func(r *ExternalIdentityRequest) { r.Mode = "shared" },
		func(r *ExternalIdentityRequest) { r.Provider = "Entra" },
		func(r *ExternalIdentityRequest) { r.ProviderTenant = "has space" },
		func(r *ExternalIdentityRequest) { r.ProviderTenant = "" },
		func(r *ExternalIdentityRequest) { r.ProviderConfigKey = "secret=abc def" },
	}
	for i, mutate := range bad {
		r := validExternalRequest()
		mutate(&r)
		if _, err := NewExternalIdentity(testExternalID, r, allowModes(IdentityModes()), "100", time.Unix(0, 0)); !errors.Is(err, ErrInvalidExternalIdentity) {
			t.Errorf("case %d accepted: %v", i, err)
		}
	}
	if _, err := NewExternalIdentity(testExternalID, validExternalRequest(), allowModes(nil), "100", time.Unix(0, 0)); !errors.Is(err, ErrIdentityModeNotAllowed) {
		t.Errorf("a mode the policy does not allow: %v", err)
	}
}

// The record holds references only. Adding a field is a deliberate change
// to this list.
func TestExternalAgentIdentityFieldSet(t *testing.T) {
	want := []string{"ID", "TenantProjectID", "ApplicationID", "Mode", "Ref", "ProviderConfigKey", "AgentUserRef",
		"OwnerRef", "State", "RevocationEpoch", "CreatedBy", "LastTransitionBy", "CreatedAt", "UpdatedAt"}
	typ := reflect.TypeOf(ExternalAgentIdentity{})
	var got []string
	for i := 0; i < typ.NumField(); i++ {
		got = append(got, typ.Field(i).Name)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fields = %v", got)
	}
}
