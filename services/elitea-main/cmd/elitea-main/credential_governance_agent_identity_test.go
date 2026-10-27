package main

import (
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialgovernance/profile"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity"
)

// Flag off (#1151): no registry, no broker and no admin handler is built, so
// the router mounts no agent identity route and admission has no session to
// open (field 17 stays unset). A nil pool proves nothing is dialled.
func TestAgentIdentityCompositionIsEmptyWithTheFlagOff(t *testing.T) {
	got, err := agentIdentityAdmin(profile.LegacyAdmission(false), profile.PolicyRevision{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Admin != nil || got.ExternalAdmin != nil || got.Broker != nil || got.Reconciler != nil {
		t.Fatalf("flag off built %+v", got)
	}
	if len(agentIdentityProviders()) != 0 {
		t.Fatal("an adapter is configured before #1152/#1153 exist")
	}
}

// Flag on with no provider configured (#1154): the broker reads the
// reconcile health, the drift route is served, and the reconcile loop is not
// built. A nil pool proves nothing is dialled.
func TestAgentIdentityReconcileIsNotBuiltWithoutProviders(t *testing.T) {
	revision := profile.PolicyRevision{ID: "2026-10-r1", Document: profile.PolicyDocument{
		Schema: profile.PolicySchemaV1, AllowedIdentityModes: []agentidentity.IdentityMode{agentidentity.IdentityModeDedicatedAgent},
		MaxMandateTTLSeconds: 3600, AuditBacklogAlertThreshold: 100}}
	got, err := agentIdentityAdmin(profile.GovernedAdmission(revision), revision, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Broker == nil || got.ExternalAdmin == nil {
		t.Fatalf("flag on built %+v", got)
	}
	if got.Reconciler != nil {
		t.Fatal("the reconcile loop is built with no provider configured")
	}
}
