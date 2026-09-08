package main

import (
	"testing"

	configurationapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/configurations"
	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
)

// TestOIDCOnlyDeploymentComposesTheConfigurationsPlane holds the boot of every
// single-sign-on-only install with ELITEA_CONFIGURATIONS_ENABLED set — the E2E
// stack among them.
//
// The capability gate in main.go and the route constructor behind it used to
// ask DIFFERENT questions. productionAuthenticationComposed accepts a
// deployment whose credential is the browser session plus a personal access
// token; NewCurrentAvailableRoute demanded a ForwardedIdentityVerifier, which
// only the Form authentication document composes. So the gate let an OIDC-only
// deployment through and the constructor then refused, and the binary stopped
// half a second after start with:
//
//	compose current Configurations available route: invalid current
//	available-configuration route dependencies
//
// Nothing in the build saw it. Every unit test of that constructor passes the
// Form shape, and the gate has its own tests that never reach a constructor.
// This test spans the two: the credential set apiGroupAuthConfig ACTUALLY
// returns for an OIDC-only deployment, handed to the constructor that
// ACTUALLY refused it.
func TestOIDCOnlyDeploymentComposesTheConfigurationsPlane(t *testing.T) {
	// Exactly main.go's OIDC-only inputs: no Form graph, so no production
	// principal validator and no forwarded-identity verifier; a pool-backed
	// session principal validator, the token validator for
	// APPLICATION_SECRET_KEY-signed tokens, and that secret.
	oidcOnly := apiGroupAuthConfig(
		nil, nil, nil,
		activePrincipals{}, &countingTokens{}, apiGroupTestSecret, true, nil,
	)

	if oidcOnly.ForwardedIdentityVerifier != nil {
		t.Fatal("an OIDC-only deployment must not carry a forwarded-identity " +
			"verifier: there is no header-stripping ingress in front of it")
	}
	if !productionAuthenticationComposed(oidcOnly) {
		t.Fatal("the capability gate refuses an OIDC-only deployment")
	}

	catalog, err := configurationapp.LoadPinnedCurrentAvailableCatalog()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := configurationapi.NewCurrentAvailableRoute(catalog, oidcOnly); err != nil {
		t.Fatalf("the Configurations available route refuses the credential "+
			"set the gate in front of it accepted, so elitea-main cannot "+
			"start with ELITEA_CONFIGURATIONS_ENABLED on an OIDC-only "+
			"deployment: %v", err)
	}
}
