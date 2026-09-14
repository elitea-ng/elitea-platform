package admin

import secretsapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/secrets"

func defaultSecretsSection() map[string]any {
	return map[string]any{
		"id": "default_secrets", "order": 2, "icon": "security", "title": "Default Secrets", "description": "Control access to configured default secret names.",
		"fields": []map[string]any{
			{"key": "default_secret_keys", "type": "array", "items": map[string]any{"type": "string"}, "title": "Default Secret Names", "description": "Names marked as default secrets. Values remain in the project vault.", "path": "default_secret_keys", "section": "default_secrets", "default": []any{}, "maxItems": 100},
			{"key": "ignore_default_secret_api", "type": "boolean", "title": "Restrict Default Secrets", "description": "Hide default names and block their API changes unless the project secret header matches. Project permissions still apply.", "path": "ignore_default_secret_api", "section": "default_secrets", "default": false},
		},
	}
}

func validateDefaultSecrets(values map[string]any) string {
	if err := secretsapi.ValidateDefaultSecretPolicy(values); err != nil {
		return err.Error()
	}
	return ""
}
