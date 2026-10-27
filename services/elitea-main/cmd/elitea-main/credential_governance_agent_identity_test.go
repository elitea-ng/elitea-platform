package main

import (
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialgovernance/profile"
)

// Flag off (#1151): no registry, no broker and no admin handler is built, so
// the router mounts no agent identity route and admission has no session to
// open (field 17 stays unset). A nil pool proves nothing is dialled.
func TestAgentIdentityCompositionIsEmptyWithTheFlagOff(t *testing.T) {
	got, err := agentIdentityAdmin(profile.LegacyAdmission(false), profile.PolicyRevision{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Admin != nil || got.ExternalAdmin != nil || got.Broker != nil {
		t.Fatalf("flag off built %+v", got)
	}
	if len(agentIdentityProviders()) != 0 {
		t.Fatal("an adapter is configured before #1152/#1153 exist")
	}
}
