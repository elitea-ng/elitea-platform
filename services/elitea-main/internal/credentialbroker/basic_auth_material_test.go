package credentialbroker

import (
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialadapters"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/mandate"
)

// #1481: `migrate-references` stages a Jira Cloud (or Confluence Cloud)
// basic-auth configuration as ONE secret holding the key map
// {"token","username"}, the same bytes in pgvault and in Vault. Redemption
// releases both keys under the jira descriptor's static_secret keys; a
// reference to a token-only secret (a PAT, or a reference staged before the
// composite) still redeems the token alone.
func TestMigratedBasicAuthReferenceRedeemsBothKeys(t *testing.T) {
	registry, err := credentialadapters.NewBuiltinRegistry()
	if err != nil {
		t.Fatal(err)
	}
	keys := registry.MaterialKeys("jira", credentialgrant.ClassStaticSecret)
	if len(keys) != 2 || keys[0] != "token" || keys[1] != "username" {
		t.Fatalf("jira static_secret keys = %v, want [token username]", keys)
	}
	for _, test := range []struct {
		name   string
		stored string
		want   map[string]string
	}{
		{
			// The exact shape migrate-references writes (keyMapValue).
			name:   "composite basic auth",
			stored: `{"token":"atlassian-api-token","username":"bot@acme.example"}`,
			want:   map[string]string{"token": "atlassian-api-token", "username": "bot@acme.example"},
		},
		{
			name:   "token only",
			stored: `personal-access-token`,
			want:   map[string]string{"token": "personal-access-token"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			w := newWorld().withMandate(mandate.KindDelegatedBackground)
			w.catalog.keys[credentialgrant.ClassStaticSecret] = keys
			w.secrets.value = []byte(test.stored)
			result := redeem(t, w.broker(t), w.request(t))
			if result.Envelope == nil {
				t.Fatalf("denied: %+v", result.Denial)
			}
			defer result.Envelope.Destroy()
			material := result.Envelope.Material
			if len(material) != len(test.want) {
				t.Fatalf("material holds %d keys, want %d", len(material), len(test.want))
			}
			for key, value := range test.want {
				if string(material[key]) != value {
					t.Fatalf("material[%q] does not hold the stored value", key)
				}
			}
		})
	}
}
