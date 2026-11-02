package api

import (
	"testing"

	v2auth "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/auth"
	v2credentialconsent "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/credentialconsent"
)

// The Entra re-consent (#1477) sends the browser to the OIDC sign-in by a
// path it cannot import; it must be the path the router mounts.
func TestEntraReconsentLoginPathIsTheOIDCLogin(t *testing.T) {
	if v2credentialconsent.EntraLoginPath != v2auth.OIDCLoginPath {
		t.Fatalf("re-consent login %q, OIDC login %q", v2credentialconsent.EntraLoginPath, v2auth.OIDCLoginPath)
	}
}
