package credentialbroker

import (
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialadapters"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
)

const tAPIBase = "https://api.atlassian.com/ex/jira/11223344-a1b2-c3d4-e5f6-77889900aabb"

// #1464: an Atlassian 3LO envelope carries the gateway base with the token,
// under the catalogue's api_base key.
func TestProviderOAuthEnvelopeCarriesTheAPIBase(t *testing.T) {
	w, tokens := providerWorld()
	w.catalog.keys[credentialgrant.ClassOAuthDelegated] = []string{"access_token", MaterialKeyAPIBase}
	tokens.token.APIBase = tAPIBase
	result := redeem(t, w.broker(t, func(c *Config) { c.ProviderTokens = tokens }), w.request(t))
	if result.Envelope == nil {
		t.Fatalf("denied: %+v", result.Denial)
	}
	defer result.Envelope.Destroy()
	if string(result.Envelope.Material["access_token"]) != tProviderToken ||
		string(result.Envelope.Material[MaterialKeyAPIBase]) != tAPIBase || len(result.Envelope.Material) != 2 {
		t.Fatalf("material keys %d, api_base %q", len(result.Envelope.Material), result.Envelope.Material[MaterialKeyAPIBase])
	}
}

// A grant with no stored base (consented before #1464) is never released:
// the provider would refuse the token at the site URL.
func TestProviderOAuthWithoutAnAPIBaseIsDenied(t *testing.T) {
	w, tokens := providerWorld()
	w.catalog.keys[credentialgrant.ClassOAuthDelegated] = []string{"access_token", MaterialKeyAPIBase}
	denial := requireDenial(t, redeem(t, w.broker(t, func(c *Config) { c.ProviderTokens = tokens }), w.request(t)), CodeDenied)
	if denial.reason != "provider_oauth_api_base_missing" {
		t.Fatalf("reason %q", denial.reason)
	}
}

// A provider without a gateway key still releases the token alone, even
// when the grant has a base.
func TestProviderOAuthWithoutTheKeyReleasesTheTokenOnly(t *testing.T) {
	w, tokens := providerWorld()
	tokens.token.APIBase = tAPIBase
	result := redeem(t, w.broker(t, func(c *Config) { c.ProviderTokens = tokens }), w.request(t))
	if result.Envelope == nil {
		t.Fatalf("denied: %+v", result.Denial)
	}
	defer result.Envelope.Destroy()
	if len(result.Envelope.Material) != 1 {
		t.Fatalf("material keys %d", len(result.Envelope.Material))
	}
}

// The reference descriptors name the key for Jira and Confluence OAuth
// only: a static PAT never gets one.
func TestAtlassianDescriptorsNameTheAPIBaseForOAuthOnly(t *testing.T) {
	for _, d := range []credentialadapters.AdapterDescriptor{credentialadapters.Jira(), credentialadapters.Confluence()} {
		oauth := d.MaterialKeys[credentialgrant.ClassOAuthDelegated]
		if len(oauth) != 2 || oauth[1] != MaterialKeyAPIBase {
			t.Fatalf("%s oauth keys %v", d.ProviderType, oauth)
		}
		for _, key := range d.MaterialKeys[credentialgrant.ClassStaticSecret] {
			if key == MaterialKeyAPIBase {
				t.Fatalf("%s static keys name api_base", d.ProviderType)
			}
		}
	}
}
