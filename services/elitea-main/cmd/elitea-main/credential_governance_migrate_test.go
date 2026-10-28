package main

import (
	"encoding/json"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialadapters"
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
