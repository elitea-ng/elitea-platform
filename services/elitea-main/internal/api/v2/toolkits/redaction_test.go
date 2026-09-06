package toolkits

import "testing"

func TestRedactSettingsRemovesSensitiveValuesRecursively(t *testing.T) {
	got := redactSettings(map[string]any{
		"repository":   "EliteaAI/elitea-platform",
		"access_token": "must-not-leak",
		"nested": map[string]any{
			"client_secret": "must-not-leak",
			"visible":       true,
		},
		"items": []any{map[string]any{"password": "must-not-leak", "name": "safe"}},
	}).(map[string]any)

	if _, ok := got["access_token"]; ok {
		t.Fatal("access_token was not redacted")
	}
	if got["repository"] != "EliteaAI/elitea-platform" {
		t.Fatal("non-sensitive value was changed")
	}
	nested := got["nested"].(map[string]any)
	if _, ok := nested["client_secret"]; ok || nested["visible"] != true {
		t.Fatalf("nested redaction incorrect: %#v", nested)
	}
	item := got["items"].([]any)[0].(map[string]any)
	if _, ok := item["password"]; ok || item["name"] != "safe" {
		t.Fatalf("array redaction incorrect: %#v", item)
	}
}

// TestIsSecretSettingKeyClassifiesTokenBudgetsAsNotSecret is the #705 table.
//
// The four keys the issue names are the first four rows: `max_tokens` and
// `max_output_tokens` must survive a read, `api_token` and `token` must not.
// The rest of the table holds the keys that made the substring rule look
// correct. A rule that keeps them redacted is the point, not a side effect.
func TestIsSecretSettingKeyClassifiesTokenBudgetsAsNotSecret(t *testing.T) {
	cases := []struct {
		key    string
		secret bool
		why    string
	}{
		{"max_tokens", false, "a model budget, measured in the issue as stripped"},
		{"max_output_tokens", false, "the same budget under the model's own name"},
		{"api_token", true, "a credential: token is the last word, singular"},
		{"token", true, "a credential on its own"},

		{"toolkit_configuration_max_tokens", false, "the DeepWiki chat path reads this key"},
		{"toolkit_configuration_max_output_tokens", false, "the same key, model spelling"},
		{"max_total_tokens", false, "a budget"},
		{"token_limit", false, "a bound: token is not the last word"},
		{"maxOutputTokens", false, "camelCase splits the same way"},

		{"access_token", true, "a credential"},
		{"github_access_token", true, "the credential the substring rule protected"},
		{"jira_api_token", true, "the credential the substring rule protected"},
		{"refresh_token", true, "a credential"},
		{"apiToken", true, "one whole-key name, separators removed"},
		{"api_key", true, "a key with a credential qualifier"},
		{"apikey", true, "one whole-key name"},
		{"apiKey", true, "one whole-key name, camelCase"},
		{"APIKey", true, "one whole-key name, a run of capitals"},
		{"openai_api_key", true, "the SDK snapshot declares this field secret"},
		{"secret_access_key", true, "the SDK snapshot declares this field secret"},
		{"secret_key", true, "the SDK snapshot declares this field secret"},
		{"access_key", true, "the SDK snapshot declares this field secret"},
		{"client_secret", true, "a credential"},
		{"password", true, "a credential"},
		{"passphrase", true, "a credential"},
		{"credentials", true, "a credential"},
		{"authorization", true, "a whole-key name"},
		{"pat", true, "a personal access token"},

		{"repository", false, "an ordinary string"},
		{"llm_model", false, "an ordinary string"},
		{"sort_key", false, "a key with no credential qualifier"},
		{"partition_key", false, "a key with no credential qualifier"},
		{"cache_key", false, "a key with no credential qualifier"},
		{"", false, "no words at all"},
	}

	for _, testCase := range cases {
		t.Run(testCase.key, func(t *testing.T) {
			if got := IsSecretSettingKey(testCase.key); got != testCase.secret {
				t.Fatalf(
					"IsSecretSettingKey(%q) = %t, want %t — %s",
					testCase.key, got, testCase.secret, testCase.why,
				)
			}
		})
	}
}

// TestRedactSettingsKeepsTokenBudgetsAtEveryDepth measures the defect where it
// was reported: through redactSettings, on the settings object the issue read
// back from a running stack.
func TestRedactSettingsKeepsTokenBudgetsAtEveryDepth(t *testing.T) {
	got := redactSettings(map[string]any{
		"repository":                       "acme/e2e-service",
		"branch":                           "main",
		"llm_model":                        "gpt-4o-mini",
		"max_tokens":                       float64(32000),
		"toolkit_configuration_max_tokens": float64(32000),
		"access_token":                     "must-not-leak",
		"nested": map[string]any{
			"max_output_tokens": float64(16000),
			"api_token":         "must-not-leak",
		},
		"items": []any{map[string]any{"token_limit": float64(8), "token": "must-not-leak"}},
	}).(map[string]any)

	for key, want := range map[string]any{
		"max_tokens":                       float64(32000),
		"toolkit_configuration_max_tokens": float64(32000),
	} {
		if got[key] != want {
			t.Fatalf("%s = %#v, want %#v — the budget was stripped again", key, got[key], want)
		}
	}
	if _, present := got["access_token"]; present {
		t.Fatal("access_token survived the redaction")
	}

	nested := got["nested"].(map[string]any)
	if nested["max_output_tokens"] != float64(16000) {
		t.Fatalf("nested max_output_tokens = %#v, want 16000", nested["max_output_tokens"])
	}
	if _, present := nested["api_token"]; present {
		t.Fatal("nested api_token survived the redaction")
	}

	item := got["items"].([]any)[0].(map[string]any)
	if item["token_limit"] != float64(8) {
		t.Fatalf("token_limit = %#v, want 8", item["token_limit"])
	}
	if _, present := item["token"]; present {
		t.Fatal("token survived the redaction inside an array")
	}
}
