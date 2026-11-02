package agentexecution

import (
	"context"
	"errors"
	"testing"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
)

// An index run's toolkit is frozen under the mandate's binding by the agent
// admission's rules (#1473).
func TestFreezeToolkitOfAnUnattendedRun(t *testing.T) {
	ref := map[string]any{"elitea_title": "jira-prod", "private": false, "credential_reference_id": frozenReference}
	t.Run("exact reference", func(t *testing.T) {
		credentials, resolver, _ := governedWorld(t, fakeGovernedToolkits{21: jiraRef(ref)}, 21)
		freeze, err := credentials.FreezeToolkit(context.Background(), 7, frozenBinding, 21)
		if err != nil {
			t.Fatal(err)
		}
		if freeze.AgentBindingID != frozenBinding || freeze.ApplicationVersionID != 0 || len(freeze.References) != 1 ||
			freeze.References[0].CredentialReferenceID != frozenReference || freeze.References[0].ToolkitDigest != resolver.digest(21) {
			t.Fatalf("freeze = %+v", freeze)
		}
		if len(resolver.governed) != 1 || !resolver.governed[0] {
			t.Fatalf("the resolution did not run as a governed run: %v", resolver.governed)
		}
	})
	t.Run("digest only", func(t *testing.T) {
		credentials, resolver, _ := governedWorld(t, fakeGovernedToolkits{21: map[string]any{"base_url": "https://x"}}, 21)
		freeze, err := credentials.FreezeToolkit(context.Background(), 7, frozenBinding, 21)
		if err != nil {
			t.Fatal(err)
		}
		if len(freeze.References) != 1 || freeze.References[0].CredentialReferenceID != "" ||
			freeze.References[0].ToolkitDigest != resolver.digest(21) {
			t.Fatalf("freeze = %+v", freeze)
		}
	})
	t.Run("unbound, no reference", func(t *testing.T) {
		credentials, _, _ := governedWorld(t, fakeGovernedToolkits{21: map[string]any{}})
		freeze, err := credentials.FreezeToolkit(context.Background(), 7, frozenBinding, 21)
		if err != nil || freeze.AgentBindingID != frozenBinding || len(freeze.References) != 0 {
			t.Fatalf("freeze = %+v, err = %v", freeze, err)
		}
	})
	t.Run("grant missing", func(t *testing.T) {
		credentials, _, _ := governedWorld(t, fakeGovernedToolkits{21: jiraRef(ref)}, 21)
		_, err := credentials.FreezeToolkit(context.Background(), 7, "another-binding", 21)
		var refused *GovernedCredentialAdmissionError
		if !errors.Is(err, configurationapp.ErrGovernedGrantMissing) || !errors.As(err, &refused) ||
			refused.Code != "credential_grant_missing" || refused.ToolkitID != 21 {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("invalid", func(t *testing.T) {
		credentials, _, _ := governedWorld(t, fakeGovernedToolkits{})
		if _, err := credentials.FreezeToolkit(context.Background(), 7, "", 21); !errors.Is(err, ErrInvalidAgentAdmission) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestUnattendedCredentialFreezeTravelsInTheContext(t *testing.T) {
	if UnattendedCredentialFreezeFrom(context.Background()) != nil {
		t.Fatal("an empty context carries a freeze")
	}
	if UnattendedCredentialFreezeFrom(WithUnattendedCredentialFreeze(context.Background(), nil)) != nil {
		t.Fatal("a nil freeze was carried")
	}
	freeze := &GovernedCredentialFreeze{AgentBindingID: "b"}
	if UnattendedCredentialFreezeFrom(WithUnattendedCredentialFreeze(context.Background(), freeze)) != freeze {
		t.Fatal("the freeze was lost")
	}
}
