package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialadapters"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/secretstore"
)

// The composite basic-auth secret is a JSON object of exactly the two
// catalogue keys, in a fixed order, whatever bytes the values hold (#1481).
func TestKeyMapValueIsTheCatalogueKeyMap(t *testing.T) {
	for _, test := range []struct{ token, username string }{
		{"atlassian-api-token", "bot@acme.example"},
		{`quo"te\back`, "tab\tnew\nline\x01"},
		{"ünïcødé", "user"},
	} {
		raw := keyMapValue([]byte(test.token), []byte(test.username))
		var decoded map[string]string
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("%q is not JSON: %v", raw, err)
		}
		if len(decoded) != 2 || decoded["token"] != test.token || decoded["username"] != test.username {
			t.Fatalf("decoded %v from %q", decoded, raw)
		}
	}
	if got := string(keyMapValue([]byte("t"), []byte("u"))); got != `{"token":"t","username":"u"}` {
		t.Fatalf("key map bytes %s: a rerun compares bytes, so the shape is fixed", got)
	}
}

// Which configurations become a key map: basic auth is a sealed api_key
// with a username and no token; a token wins, as it does for a legacy run.
func TestPlanToolkitStagesBasicAuthAsAKeyMap(t *testing.T) {
	registry, err := credentialadapters.NewBuiltinRegistry()
	if err != nil {
		t.Fatal(err)
	}
	const uuid = "55555555-5555-4555-8555-555555555555"
	toolkit := migrateToolkit{id: 1, toolkitType: "jira", settings: map[string]any{
		"jira_configuration": map[string]any{"elitea_title": "cfg", "private": false},
	}}
	for _, test := range []struct {
		name, data     string
		copyToVault    bool
		wantLocator    string
		wantGeneration int64
		wantMaterial   string
		wantUsername   string
		wantSealedUser string
		wantSkip       string
	}{
		{name: "cloud basic auth", data: `{"base_url":"https://a.atlassian.net","username":"bot@acme.example","api_key":"{{secret.key1}}"}`,
			wantLocator: "governed_55555555555545558555555555555555", wantMaterial: "token+username", wantUsername: "bot@acme.example"},
		{name: "sealed username", data: `{"base_url":"https://a.atlassian.net","username":"{{secret.user1}}","api_key":"{{secret.key1}}"}`,
			wantLocator: "governed_55555555555545558555555555555555", wantMaterial: "token+username", wantSealedUser: "user1"},
		{name: "vault copy", copyToVault: true, data: `{"base_url":"https://a.atlassian.net","username":"bot","api_key":"{{secret.key1}}"}`,
			wantLocator: "elitea/projects/7/credentials/" + uuid, wantMaterial: "token+username", wantUsername: "bot"},
		{name: "token wins", data: `{"base_url":"https://a.atlassian.net","username":"bot","token":"{{secret.pat1}}","api_key":"{{secret.key1}}"}`,
			wantLocator: "pat1", wantGeneration: 1, wantMaterial: "token"},
		{name: "api key without username", data: `{"base_url":"https://a.atlassian.net","api_key":"{{secret.key1}}"}`,
			wantLocator: "key1", wantGeneration: 1, wantMaterial: "token"},
		{name: "half-sealed username", data: `{"base_url":"https://a.atlassian.net","username":"x{{secret.user1}}","api_key":"{{secret.key1}}"}`,
			wantSkip: "secret_not_sealed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := &referenceMigration{registry: registry, opts: migrateOptions{copyToVault: test.copyToVault}}
			configurations := map[string]migrateConfiguration{"cfg": {uuid: uuid, configurationType: "jira", data: test.data}}
			item, include, err := m.planToolkit(7, configurations, toolkit)
			if err != nil || !include {
				t.Fatalf("include=%v err=%v", include, err)
			}
			if test.wantSkip != "" {
				if item.action != migrateSkipped || item.Reason != test.wantSkip {
					t.Fatalf("item = %+v, want skipped %s", item, test.wantSkip)
				}
				return
			}
			if item.action != migrateProposed || item.Locator != test.wantLocator || item.Generation != test.wantGeneration ||
				item.Material != test.wantMaterial || item.username != test.wantUsername || item.usernameSecret != test.wantSealedUser {
				t.Fatalf("item = %+v (username %q, sealed %q)", item, item.username, item.usernameSecret)
			}
		})
	}
}

// A rerun reports the material an existing reference holds: a pgvault
// reference staged token-only before #1481 is not shown as a key map, and
// the operator is told how to add the username.
func TestFillReportsTheExistingReferencesMaterial(t *testing.T) {
	item := migrateItem{username: "ops@example.com", Material: "token+username"}
	item.fill(repos.CredentialReferenceRecord{CredentialReference: credentialgrant.CredentialReference{
		ID: "ref-1", Class: credentialgrant.ClassStaticSecret, Backend: secretstore.BackendPGVault,
		Locator: "jira_token", Generation: 1,
	}})
	if item.Material != "token" || !strings.HasPrefix(item.Reason, "token_only_reference") {
		t.Fatalf("token-only reference = material %q reason %q", item.Material, item.Reason)
	}
	item = migrateItem{username: "ops@example.com", Material: "token+username"}
	item.fill(repos.CredentialReferenceRecord{CredentialReference: credentialgrant.CredentialReference{
		ID: "ref-2", Class: credentialgrant.ClassStaticSecret, Backend: secretstore.BackendPGVault,
		Locator: migrateKeyMapName("0b7f6a2e-9c1d-4f3a-8e2b-5d6c7a8b9c0d"), Generation: 1,
	}})
	if item.Material != "token+username" || item.Reason != "" {
		t.Fatalf("key-map reference = material %q reason %q", item.Material, item.Reason)
	}
}
