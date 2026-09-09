package configurations_test

import (
	"context"
	"testing"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	handler "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/configurations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

// TestCurrentConfigurationRoutesComposeOnAnOIDCOnlyDeployment holds the boot of
// every single-sign-on-only install, the E2E stack among them.
//
// These six constructors used to demand a non-nil ForwardedIdentityVerifier.
// That field is composed by the Form authentication document alone
// (ELITEA_AUTH_CONFIG_FILE); apiGroupAuthConfig's OIDC-only branch leaves it
// nil and carries the browser session secret plus the personal-access-token
// validator instead. The composition-root gate in front of this plane,
// productionAuthenticationComposed, accepts that shape — so the gate admitted
// the deployment and the constructor behind it then refused, and elitea-main
// stopped with "compose current Configurations available route: invalid
// current available-configuration route dependencies" half a second after
// start. Nothing in the unit suite saw it, because every existing case builds
// the Form shape.
//
// A nil verifier is safe at request time: apimw.tryTraefikHeaders refuses every
// X-Auth-* header when it is absent, so the forwarded-identity plane is closed
// rather than open.
func TestCurrentConfigurationRoutesComposeOnAnOIDCOnlyDeployment(t *testing.T) {
	principal := currentReadPrincipalValidatorFunc(
		func(_ context.Context, user auth.User) (auth.User, error) { return user, nil },
	)
	// The shape apiGroupAuthConfig returns when no Form document exists: a
	// principal validator and the session cookie's signing key, and no
	// forwarded-identity verifier at all.
	oidcOnly := apimw.AuthConfig{PrincipalValidator: principal, SessionSecret: "e2e-session-secret"}
	permissions := permissionResolverFunc(
		func(context.Context, auth.User, string, string) (auth.PermissionResolution, error) {
			return auth.PermissionResolution{}, nil
		},
	)

	if !oidcOnly.CredentialPlaneComposed() {
		t.Fatal("the OIDC-only credential set must count as composed")
	}

	for name, compose := range map[string]func() error{
		"available": func() error {
			_, err := handler.NewCurrentAvailableRoute(&currentAvailableCatalogStub{}, oidcOnly)
			return err
		},
		"read": func() error {
			_, err := handler.NewCurrentConfigurationReadRoute(
				&currentConfigurationReaderStub{}, 1, oidcOnly, permissions,
			)
			return err
		},
		"types": func() error {
			_, err := handler.NewCurrentConfigurationTypesRoute(
				&currentConfigurationTypesReaderStub{}, oidcOnly, permissions,
			)
			return err
		},
		"model catalogue": func() error {
			_, err := handler.NewCurrentModelCatalogRoute(
				&currentModelCatalogReaderStub{}, 1, oidcOnly, permissions,
			)
			return err
		},
		"model default": func() error {
			_, err := handler.NewCurrentModelDefaultRoute(
				&currentModelDefaultWriterStub{}, oidcOnly, permissions,
			)
			return err
		},
		"mutation": func() error {
			_, err := handler.NewCurrentConfigurationMutationRoute(
				&currentConfigurationMutatorStub{}, oidcOnly, permissions,
			)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := compose(); err != nil {
				t.Fatalf("the %s route refused an OIDC-only deployment: %v", name, err)
			}
		})
	}
}

// TestCredentialPlaneComposedRefusesADeploymentWithoutACredentialReader keeps
// the widening from becoming "no authentication is fine".
func TestCredentialPlaneComposedRefusesADeploymentWithoutACredentialReader(t *testing.T) {
	principal := currentReadPrincipalValidatorFunc(
		func(_ context.Context, user auth.User) (auth.User, error) { return user, nil },
	)
	for name, config := range map[string]apimw.AuthConfig{
		"nothing at all":                                            {},
		"a reader but no principal validator":                       {SessionSecret: "s"},
		"a principal validator but nothing that reads a credential": {PrincipalValidator: principal},
	} {
		t.Run(name, func(t *testing.T) {
			if config.CredentialPlaneComposed() {
				t.Fatal("an unauthenticated deployment must not count as composed")
			}
		})
	}
}
