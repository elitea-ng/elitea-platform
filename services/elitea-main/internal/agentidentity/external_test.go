package agentidentity_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/agentidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/agentidentity/fakeprovider"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialgovernance/profile"
	identity "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity/governance"
)

type externalFixture struct {
	store    *memStore
	provider *fakeprovider.Provider
	service  *agentidentity.ExternalIdentityService
}

func newExternalFixture(t *testing.T, revision profile.PolicyRevision, providers ...agentidentity.Provider) *externalFixture {
	t.Helper()
	f := &externalFixture{store: newMemStore(), provider: fakeprovider.New("fake")}
	if providers == nil {
		providers = []agentidentity.Provider{f.provider}
	}
	registry, err := agentidentity.NewRegistry(providers...)
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{"0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e0f01", "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e0f02"}
	f.service, err = agentidentity.NewExternalIdentityService(agentidentity.ExternalIdentityConfig{
		Revisions: governance.FixedRevision(revision), Store: f.store, Registry: registry,
		Now: func() time.Time { return fixedNow },
		NewID: func() (string, error) {
			id := ids[0]
			ids = ids[1:]
			return id, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func externalRequest() identity.ExternalIdentityRequest {
	return identity.ExternalIdentityRequest{TenantProjectID: tenantID, ApplicationID: 11,
		Mode: identity.IdentityModeDedicatedAgent, Provider: "fake", ProviderTenant: "t1", ProviderConfigKey: "blueprint-1"}
}

func (f *externalFixture) bindActive(id string) {
	f.store.bindings[bindingA] = identity.AgentIdentityBinding{ID: bindingA, TenantProjectID: tenantID,
		ApplicationID: 11, ExternalAgentIdentityID: id, State: identity.BindingActive, RevocationEpoch: 1}
}

// Acceptance: provisioning leaves the identity in pending_activation, and
// activation without an approved binding fails.
func TestProvisioningNeverActivatesAndActivationNeedsAnApprovedBinding(t *testing.T) {
	f := newExternalFixture(t, allowAllRevision())
	ctx := context.Background()
	created, err := f.service.Create(ctx, externalRequest(), "100")
	if err != nil || created.State != identity.ExternalIdentityProvisioning || created.RevocationEpoch != 0 {
		t.Fatalf("Create = %+v, %v", created, err)
	}
	if len(f.provider.Calls()) != 0 {
		t.Fatal("Create called the provider")
	}
	provisioned, err := f.service.Provision(ctx, created.ID, "100")
	if err != nil || provisioned.State != identity.ExternalIdentityPendingActivation || provisioned.RevocationEpoch != 1 ||
		provisioned.Ref.ID == "" || provisioned.Ref.Kind != "fake.agent" {
		t.Fatalf("Provision = %+v, %v", provisioned, err)
	}
	if _, err := f.service.Activate(ctx, created.ID, "200"); !errors.Is(err, identity.ErrNoApprovedBinding) {
		t.Fatalf("Activate without a binding = %v", err)
	}
	if slices.Contains(f.provider.Calls(), "Activate") {
		t.Fatal("a refused activation reached the provider")
	}
	f.bindActive(created.ID)
	active, err := f.service.Activate(ctx, created.ID, "200")
	if err != nil || active.State != identity.ExternalIdentityActive || active.RevocationEpoch != 2 {
		t.Fatalf("Activate = %+v, %v", active, err)
	}
}

// Resume returns an identity to active, so it needs an approved binding
// as Activate does: suspended, its bindings revoked, it stays suspended.
func TestResumeNeedsAnApprovedBindingLikeActivation(t *testing.T) {
	f := newExternalFixture(t, allowAllRevision())
	ctx := context.Background()
	created, _ := f.service.Create(ctx, externalRequest(), "100")
	_, _ = f.service.Provision(ctx, created.ID, "100")
	f.bindActive(created.ID)
	if _, err := f.service.Activate(ctx, created.ID, "200"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Suspend(ctx, created.ID, "200"); err != nil {
		t.Fatal(err)
	}
	delete(f.store.bindings, bindingA)
	if _, err := f.service.Resume(ctx, created.ID, "200"); !errors.Is(err, identity.ErrNoApprovedBinding) {
		t.Fatalf("Resume without a binding = %v", err)
	}
	if slices.Contains(f.provider.Calls(), "Resume") {
		t.Fatal("a refused resume reached the provider")
	}
}

func TestExternalIdentityLifeCycleBumpsTheEpochOnEveryTransition(t *testing.T) {
	f := newExternalFixture(t, allowAllRevision())
	ctx := context.Background()
	created, _ := f.service.Create(ctx, externalRequest(), "100")
	_, _ = f.service.Provision(ctx, created.ID, "100")
	f.bindActive(created.ID)
	steps := []struct {
		run   func(context.Context, string, string) (identity.ExternalAgentIdentity, error)
		state identity.ExternalIdentityState
		epoch int64
	}{
		{f.service.Activate, identity.ExternalIdentityActive, 2},
		{f.service.Suspend, identity.ExternalIdentitySuspended, 3},
		{f.service.Resume, identity.ExternalIdentityActive, 4},
		// retire is two transitions: retiring, then retired.
		{f.service.Retire, identity.ExternalIdentityRetired, 6},
	}
	for _, step := range steps {
		got, err := step.run(ctx, created.ID, "200")
		if err != nil || got.State != step.state || got.RevocationEpoch != step.epoch {
			t.Fatalf("got %s epoch %d (%v), want %s epoch %d", got.State, got.RevocationEpoch, err, step.state, step.epoch)
		}
	}
	if _, err := f.service.Resume(ctx, created.ID, "200"); !errors.Is(err, identity.ErrExternalTransition) {
		t.Fatalf("a retired identity resumed: %v", err)
	}
	if want := []string{"Provision", "Activate", "Suspend", "Resume", "Revoke"}; !slices.Equal(f.provider.Calls(), want) {
		t.Fatalf("calls = %v, want %v", f.provider.Calls(), want)
	}
}

// Stopping never depends on the provider: with no adapter configured the
// record is still suspended, and the error says the provider was not told.
func TestSuspendWithoutAProviderStillSuspends(t *testing.T) {
	f := newExternalFixture(t, allowAllRevision(), []agentidentity.Provider{}...)
	f.store.identities[externalA] = identity.ExternalAgentIdentity{ID: externalA, TenantProjectID: tenantID,
		ApplicationID: 11, Mode: identity.IdentityModeDedicatedAgent,
		Ref:   identity.ExternalRef{Provider: "fake", ProviderTenant: "t1", Kind: "fake.agent", ID: "obj-a"},
		State: identity.ExternalIdentityActive, RevocationEpoch: 3}
	got, err := f.service.Suspend(context.Background(), externalA, "200")
	if !errors.Is(err, agentidentity.ErrProviderNotConfigured) || got.State != identity.ExternalIdentitySuspended {
		t.Fatalf("Suspend = %s, %v", got.State, err)
	}
	if f.store.identities[externalA].State != identity.ExternalIdentitySuspended {
		t.Fatal("not stored as suspended")
	}
	if _, err := f.service.Provision(context.Background(), externalA, "200"); !errors.Is(err, identity.ErrExternalTransition) {
		t.Fatalf("provision of a provisioned identity: %v", err)
	}
}

func TestCreateRefusesAModeThePolicyDoesNotAllow(t *testing.T) {
	f := newExternalFixture(t, profile.PolicyRevision{ID: "rev-0"})
	if _, err := f.service.Create(context.Background(), externalRequest(), "100"); !errors.Is(err, identity.ErrIdentityModeNotAllowed) {
		t.Fatalf("Create = %v", err)
	}
}
