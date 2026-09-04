package mcpregistry

import (
	"errors"
	"math"
	"net/url"
	"testing"
)

func parameterizedPrebuiltServer() PrebuiltServer {
	return PrebuiltServer{
		Key:         "ado_mcp_project",
		DisplayName: "ADO-MCP-project",
		ServerURL:   "https://mcp.dev.azure.com/{org_name}",
		Headers:     map[string]string{"Authorization": "Bearer {api_token}"},
		ConfigSchema: map[string]any{"properties": map[string]any{
			"api_token": map[string]any{
				"type": "string", "required": true, "secret": true,
			},
			"org_name": map[string]any{
				"type": "string", "required": true,
			},
			"project_name": map[string]any{
				"type": "string", "required": true,
			},
		}},
		Enabled: true,
	}
}

func TestMaterializePrebuiltTemplatesMatchesCurrentADOConfiguration(t *testing.T) {
	entry := parameterizedPrebuiltServer()
	endpoint, headers, err := MaterializePrebuiltTemplates(entry, map[string]any{
		"api_token":    "project-token",
		"org_name":     "engineering team",
		"project_name": "runtime",
	})
	if err != nil {
		t.Fatal(err)
	}
	if endpoint != "https://mcp.dev.azure.com/engineering%20team" {
		t.Fatalf("endpoint=%q", endpoint)
	}
	if headers["Authorization"] != "Bearer project-token" {
		t.Fatalf("authorization=%q", headers["Authorization"])
	}
	if entry.ServerURL != "https://mcp.dev.azure.com/{org_name}" {
		t.Fatal("materialization changed the catalogue entry")
	}
}

func TestMaterializePrebuiltTemplatesAcceptsRuntimeOwnedInternalValues(t *testing.T) {
	entry := PrebuiltServer{
		Key:         "elitea_applications",
		DisplayName: "Elitea Applications",
		ServerURL:   "https://elitea.example.test/app/{project_id}/mcp/elitea_core/applications",
		Headers: map[string]string{
			"Authorization": "Bearer {personal_token}",
		},
		Enabled: true,
	}

	endpoint, headers, err := MaterializePrebuiltTemplates(entry, map[string]any{
		"project_id":     "42",
		"personal_token": "ephemeral-bearer",
	})
	if err != nil {
		t.Fatal(err)
	}
	if endpoint != "https://elitea.example.test/app/42/mcp/elitea_core/applications" {
		t.Fatalf("endpoint=%q", endpoint)
	}
	if headers["Authorization"] != "Bearer ephemeral-bearer" {
		t.Fatalf("authorization=%q", headers["Authorization"])
	}
	if names, err := PrebuiltParameterNames(entry); err != nil || len(names) != 0 {
		t.Fatalf("runtime values reached toolkit schema: names=%v error=%v", names, err)
	}
}

func TestPrebuiltSchemaRejectsRuntimeOwnedValues(t *testing.T) {
	for _, name := range []string{"project_id", "personal_token"} {
		t.Run(name, func(t *testing.T) {
			entry := parameterizedPrebuiltServer()
			entry.ConfigSchema["properties"].(map[string]any)[name] = map[string]any{"type": "string"}
			if err := ValidatePrebuiltServer(entry); !errors.Is(err, ErrInvalidPrebuiltParameters) {
				t.Fatalf("runtime-owned field error=%v", err)
			}
		})
	}
}

func TestPrebuiltTemplatesRejectUndeclaredOrAuthorityPlaceholders(t *testing.T) {
	entry := parameterizedPrebuiltServer()
	entry.Headers["X-Unknown"] = "{missing}"
	if err := ValidatePrebuiltServer(entry); !errors.Is(err, ErrInvalidPrebuiltParameters) {
		t.Fatalf("undeclared placeholder error=%v", err)
	}

	entry = parameterizedPrebuiltServer()
	entry.ServerURL = "https://{org_name}.example.test/mcp"
	if err := ValidatePrebuiltServer(entry); !errors.Is(err, ErrInvalidPrebuiltParameters) {
		t.Fatalf("authority placeholder error=%v", err)
	}
}

func TestPrebuiltTemplatesRejectMalformedPlaceholders(t *testing.T) {
	entry := parameterizedPrebuiltServer()
	entry.Headers["X-Invalid"] = "Bearer {api-token}"
	if err := ValidatePrebuiltServer(entry); !errors.Is(err, ErrInvalidPrebuiltParameters) {
		t.Fatalf("malformed placeholder error=%v", err)
	}
}

func TestPrebuiltTemplatesRequireDeclaredFieldsAndPreserveExtraFields(t *testing.T) {
	entry := parameterizedPrebuiltServer()
	_, _, err := MaterializePrebuiltTemplates(entry, map[string]any{
		"api_token": "project-token",
		"org_name":  "engineering",
	})
	if !errors.Is(err, ErrInvalidPrebuiltParameters) {
		t.Fatalf("missing required field error=%v", err)
	}

	names, err := PrebuiltParameterNames(entry)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"api_token", "org_name", "project_name"}
	for index := range want {
		if names[index] != want[index] {
			t.Fatalf("names=%v", names)
		}
	}
}

func TestPrebuiltSchemaRejectsReservedAndNonStringSecretFields(t *testing.T) {
	entry := parameterizedPrebuiltServer()
	properties := entry.ConfigSchema["properties"].(map[string]any)
	properties["url"] = map[string]any{"type": "string"}
	if err := ValidatePrebuiltServer(entry); !errors.Is(err, ErrInvalidPrebuiltParameters) {
		t.Fatalf("reserved field error=%v", err)
	}

	entry = parameterizedPrebuiltServer()
	properties = entry.ConfigSchema["properties"].(map[string]any)
	properties["api_token"] = map[string]any{"type": "integer", "secret": true}
	if err := ValidatePrebuiltServer(entry); !errors.Is(err, ErrInvalidPrebuiltParameters) {
		t.Fatalf("non-string secret error=%v", err)
	}
}

func TestPrebuiltIntegerParameterRejectsFractionalAndOutOfRangeNumbers(t *testing.T) {
	entry := parameterizedPrebuiltServer()
	entry.ConfigSchema = map[string]any{"properties": map[string]any{
		"organization_id": map[string]any{"type": "integer", "required": true},
	}}
	entry.ServerURL = "https://mcp.example.test/{organization_id}"
	entry.Headers = nil

	for _, value := range []float64{1.25, math.MaxFloat64, math.Inf(1), math.NaN()} {
		if _, _, err := MaterializePrebuiltTemplates(
			entry, map[string]any{"organization_id": value},
		); !errors.Is(err, ErrInvalidPrebuiltParameters) {
			t.Fatalf("value=%v error=%v", value, err)
		}
	}
}

func TestPrebuiltToolkitTypeSchemaCarriesSecretAndRequiredMetadata(t *testing.T) {
	toolkitType, schema, err := prebuiltToolkitTypeSchema(parameterizedPrebuiltServer())
	if err != nil {
		t.Fatal(err)
	}
	if toolkitType != "mcp_ado_mcp_project" {
		t.Fatalf("toolkit type=%q", toolkitType)
	}
	properties := schema["properties"].(map[string]any)
	if secret, _ := properties["api_token"].(map[string]any)["secret"].(bool); !secret {
		t.Fatal("secret annotation was lost")
	}
	if format, _ := properties["api_token"].(map[string]any)["format"].(string); format != "password" {
		t.Fatalf("secret format=%q", format)
	}
	if _, ok := properties["selected_tools"].(map[string]any); !ok {
		t.Fatal("selected-tools schema is missing")
	}
}

func FuzzMaterializedPrebuiltPathCannotChangeAuthority(f *testing.F) {
	for _, value := range []string{"engineering", "other.example/mcp", "../admin", "tenant name"} {
		f.Add(value)
	}
	entry := parameterizedPrebuiltServer()
	entry.ConfigSchema = map[string]any{"properties": map[string]any{
		"org_name": map[string]any{"type": "string", "required": true},
	}}
	entry.ServerURL = "https://mcp.example.test/{org_name}/mcp"
	entry.Headers = nil

	f.Fuzz(func(t *testing.T, organization string) {
		endpoint, _, err := MaterializePrebuiltTemplates(
			entry, map[string]any{"org_name": organization})
		if err != nil {
			return
		}
		parsed, err := url.Parse(endpoint)
		if err != nil {
			t.Fatal(err)
		}
		if parsed.Scheme != "https" || parsed.Host != "mcp.example.test" {
			t.Fatalf("parameter changed authority: %q", endpoint)
		}
	})
}
