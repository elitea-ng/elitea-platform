package toolkits

import "testing"

func TestRedactSettingsKeepsDiscoveredToolSchemaNames(t *testing.T) {
	got := redactSettings(map[string]any{
		"available_mcp_tool_schemas": map[string]any{
			"refresh_token": map[string]any{"inputSchema": map[string]any{"properties": map[string]any{"password": map[string]any{"type": "string"}}}},
		},
	}).(map[string]any)
	schemas := got["available_mcp_tool_schemas"].(map[string]any)
	tool, ok := schemas["refresh_token"].(map[string]any)
	if !ok {
		t.Fatalf("a tool named like a credential was dropped: %#v", schemas)
	}
	props := tool["inputSchema"].(map[string]any)["properties"].(map[string]any)
	if _, ok := props["password"]; !ok {
		t.Fatalf("an argument named like a credential was dropped: %#v", props)
	}
}

// TestRedactSettingsKeepsSecretReferencesUnderSensitiveKeys is the #2116/#2126
// round trip: a reference is a pointer, so the edit form must read it back, while
// a literal under the same key stays removed.
func TestRedactSettingsKeepsSecretReferencesUnderSensitiveKeys(t *testing.T) {
	got := redactSettings(map[string]any{
		"client_secret": "{{secret.my_client_secret}}",
		"headers": map[string]any{
			"Authorization": "Bearer {{secret.token}}",
			"X-Api-Key":     "{{secret.api_key}}",
			"X-Token":       "abc123{{secret.part}}",
		},
		"access_token": "literal-must-not-leak",
		"password":     "{{secret.}}",
	}).(map[string]any)

	if got["client_secret"] != "{{secret.my_client_secret}}" {
		t.Fatalf("client_secret reference was dropped: %#v", got)
	}
	headers := got["headers"].(map[string]any)
	if headers["Authorization"] != "Bearer {{secret.token}}" || headers["X-Api-Key"] != "{{secret.api_key}}" {
		t.Fatalf("header reference was dropped: %#v", headers)
	}
	if _, ok := headers["X-Token"]; ok {
		t.Fatalf("a value with literal text around a reference must stay removed: %#v", headers)
	}
	if _, ok := got["access_token"]; ok {
		t.Fatal("a literal credential leaked")
	}
	if _, ok := got["password"]; ok {
		t.Fatal("a malformed reference leaked")
	}
}
