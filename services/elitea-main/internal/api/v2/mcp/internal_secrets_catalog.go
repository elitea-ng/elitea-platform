package mcp

// The fixed secrets category mirrors only the three current operations marked
// mcp_tool=True. Reading plaintext, deleting, hiding, and administration-vault
// operations remain outside the model-facing catalogue.
const internalSecretsCategory = "secrets"

type internalSecretOperation string

const (
	internalListSecrets  internalSecretOperation = "list_secrets"
	internalCreateSecret internalSecretOperation = "create_secret"
	internalUpdateSecret internalSecretOperation = "update_secret"
)

type internalSecretToolDefinition struct {
	name        string
	description string
	permission  string
	operation   internalSecretOperation
	schema      map[string]any
}

var internalSecretToolDefinitions = []internalSecretToolDefinition{
	{
		name: "get_secrets_secrets",
		description: "List project secret names and reference placeholders without returning plaintext values. " +
			"The Main vault does not currently project legacy default-secret metadata, so is_default is false.",
		permission: "configuration.secrets.secret.list",
		operation:  internalListSecrets,
		schema: objectSchema(map[string]any{
			"project_id": intProperty("Current project ID. The server verifies this value."),
		}, "project_id"),
	},
	{
		name: "post_secrets_secrets",
		description: "Create a new project secret. The value is written to the encrypted project vault and is never " +
			"returned by this tool.",
		permission: "configuration.secrets.secret.create",
		operation:  internalCreateSecret,
		schema: objectSchema(map[string]any{
			"project_id": intProperty("Current project ID. The server verifies this value."),
			"name":       secretNameProperty("Secret name used by {{secret.NAME}} references."),
			"value":      nullableStringProperty("Secret value. Null is stored as an empty value."),
		}, "project_id", "name"),
	},
	{
		name: "put_secrets_secret",
		description: "Replace the value of an existing project secret. This operation does not rename the secret and " +
			"never returns its plaintext value.",
		permission: "configuration.secrets.secret.edit",
		operation:  internalUpdateSecret,
		schema: objectSchema(map[string]any{
			"project_id": intProperty("Current project ID. The server verifies this value."),
			"secret":     secretNameProperty("Existing secret name."),
			"value":      nullableStringProperty("Replacement secret value. Null is stored as an empty value."),
		}, "project_id", "secret"),
	},
}

func secretNameProperty(description string) map[string]any {
	return map[string]any{
		"type": "string", "minLength": 1, "maxLength": 128,
		"pattern": "^[A-Za-z0-9_]+$", "description": description,
	}
}

func nullableStringProperty(description string) map[string]any {
	return map[string]any{
		"description": description,
		"anyOf": []any{
			map[string]any{"type": "string"},
			map[string]any{"type": "null"},
		},
	}
}

func internalSecretTools() []Tool {
	tools := make([]Tool, 0, len(internalSecretToolDefinitions))
	for _, definition := range internalSecretToolDefinitions {
		tools = append(tools, Tool{
			Name:                    definition.name,
			Description:             definition.description,
			InputSchema:             definition.schema,
			internalSecretOperation: definition.operation,
			permission:              definition.permission,
		})
	}
	return tools
}
