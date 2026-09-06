package configurations

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"testing"
)

func TestCurrentMCPEmptySourceRevisionMatchesCanonicalEmptySchemaMap(t *testing.T) {
	digest := sha256.Sum256([]byte("{}"))
	want := "sha256:" + hex.EncodeToString(digest[:])
	if currentMCPEmptySourceRevision != want {
		t.Fatalf("empty MCP source revision = %q, want %q", currentMCPEmptySourceRevision, want)
	}
}

func TestPinnedCurrentAvailableCatalogMatchesCurrentFixedRegistry(t *testing.T) {
	catalog, err := LoadPinnedCurrentAvailableCatalog()
	if err != nil {
		t.Fatalf("LoadPinnedCurrentAvailableCatalog() error = %v", err)
	}
	if !catalog.Complete() {
		t.Fatal("Complete() = false, want true for the verified empty indexer MCP source")
	}
	entries, err := catalog.CompleteEntries()
	if err != nil {
		t.Fatalf("CompleteEntries() error = %v", err)
	}

	wantTypes := []string{
		"llm_model", "embedding_model", "image_generation_model", "asr_model", "tts_model",
		"service_prompt", "environment_settings", "project_context", "project_icon",
		"open_ai", "azure_open_ai", "ai_dial", "amazon_bedrock", "vertex_ai", "ollama",
		// The three the gateway dispatches to and no legacy plugin registers.
		// They are catalogued so their rows get a section and their keys get
		// sealed — see provider_admission_test.go (#G3).
		"anthropic", "open_ai_azure", "vllm",
		"s3", "s3_api_credentials", "github", "pgvector", "ado", "gitlab", "qtest",
		"bitbucket", "confluence", "jira", "postman", "service_now", "testrail", "slack",
		"azure_search", "delta_lake", "bigquery", "xray", "zephyr", "zephyr_enterprise",
		"zephyr_essential", "figma", "rally", "sonar", "sql", "google_places", "salesforce",
		"sharepoint", "carrier", "report_portal", "testio", "openapi", "langfuse",
		"aha",
	}
	gotTypes := make([]string, len(entries))
	for index, entry := range entries {
		gotTypes[index] = entry.Type
	}
	if !slices.Equal(gotTypes, wantTypes) {
		t.Fatalf("types = %v, want %v", gotTypes, wantTypes)
	}

	assertCurrentAvailableEntry(t, entries, "llm_model", func(t *testing.T, entry CurrentAvailableConfigurationType) {
		if entry.Section != "llm" || entry.HasTestConnection || entry.ValidationFunc != nil || entry.CheckConnectionFunc != nil {
			t.Fatalf("llm_model contract = %+v", entry)
		}
		if entry.UsesSDKValidation() {
			t.Fatal("llm_model was classified as an SDK-owned configuration")
		}
		assertCurrentAvailableSchema(t, entry, "LLM model", "ai_credentials")
	})
	assertCurrentAvailableEntry(t, entries, "tts_model", func(t *testing.T, entry CurrentAvailableConfigurationType) {
		if entry.Section != "tts" || !entry.HasTestConnection || entry.CheckConnectionFunc != nil {
			t.Fatalf("tts_model contract = %+v", entry)
		}
		assertCurrentAvailableSchema(t, entry, "Text to Speech (TTS) Model", "ai_credentials")
	})
	assertCurrentAvailableEntry(t, entries, "open_ai", func(t *testing.T, entry CurrentAvailableConfigurationType) {
		if entry.Section != "ai_credentials" || !entry.HasTestConnection || entry.ValidationFunc != nil || entry.CheckConnectionFunc != nil {
			t.Fatalf("open_ai contract = %+v", entry)
		}
		assertCurrentAvailableSchema(t, entry, "OpenAI", "api_base")
	})
	assertCurrentAvailableEntry(t, entries, "github", func(t *testing.T, entry CurrentAvailableConfigurationType) {
		if entry.Section != "credentials" || !entry.HasTestConnection ||
			entry.ValidationFunc == nil || *entry.ValidationFunc != "applications_configuration_validator" ||
			entry.CheckConnectionFunc == nil || *entry.CheckConnectionFunc != "applications_configuration_check_connection" {
			t.Fatalf("github contract = %+v", entry)
		}
		if !entry.UsesSDKValidation() {
			t.Fatal("github was not classified through its registry validator")
		}
		assertCurrentAvailableSchema(t, entry, "GitHub", "base_url")
	})
	assertCurrentAvailableEntry(t, entries, "s3", func(t *testing.T, entry CurrentAvailableConfigurationType) {
		if entry.Section != "storage" || !entry.HasTestConnection {
			t.Fatalf("s3 contract = %+v", entry)
		}
		assertCurrentAvailableSchema(t, entry, "S3 Storage", "storage_url")
	})
	assertCurrentAvailableEntry(t, entries, "openapi", func(t *testing.T, entry CurrentAvailableConfigurationType) {
		assertCurrentAvailableDataProperties(t, entry, "oauth_discovery_endpoint", "configuration_uuid")
	})
	assertCurrentAvailableEntry(t, entries, "aha", func(t *testing.T, entry CurrentAvailableConfigurationType) {
		if entry.Section != "credentials" || !entry.HasTestConnection ||
			entry.ValidationFunc == nil || *entry.ValidationFunc != "applications_configuration_validator" ||
			entry.CheckConnectionFunc == nil || *entry.CheckConnectionFunc != "applications_configuration_check_connection" {
			t.Fatalf("aha contract = %+v", entry)
		}
		assertCurrentAvailableSchema(t, entry, "Aha!", "api_key")
		assertCurrentAvailableDataProperties(t, entry, "base_url", "api_key")
	})

	sources := catalog.SourceRevisions()
	if got := sources["elitea_sdk"]; got != "a78d3654f99d8ff89ca7233f20a66d676e564f79" {
		t.Fatalf("elitea_sdk revision = %q", got)
	}
	sources["elitea_sdk"] = "changed"
	if got := catalog.SourceRevisions()["elitea_sdk"]; got != "a78d3654f99d8ff89ca7233f20a66d676e564f79" {
		t.Fatalf("SourceRevisions() aliased caller mutation: %q", got)
	}

	dynamicSources := catalog.DynamicSourceRevisions()
	if got := dynamicSources[currentMCPDynamicSource]; got != currentMCPEmptySourceRevision {
		t.Fatalf("indexer MCP source revision = %q, want %q", got, currentMCPEmptySourceRevision)
	}
	dynamicSources[currentMCPDynamicSource] = "changed"
	if got := catalog.DynamicSourceRevisions()[currentMCPDynamicSource]; got != currentMCPEmptySourceRevision {
		t.Fatalf("DynamicSourceRevisions() aliased caller mutation: %q", got)
	}
}

func TestCurrentAvailableCatalogFiltersSectionsLikeCurrentEndpoint(t *testing.T) {
	catalog, err := LoadPinnedCurrentAvailableCatalog()
	if err != nil {
		t.Fatalf("LoadPinnedCurrentAvailableCatalog() error = %v", err)
	}

	aiCredentials, err := catalog.CompleteEntries("ai_credentials")
	if err != nil {
		t.Fatalf("CompleteEntries(ai_credentials) error = %v", err)
	}
	if len(aiCredentials) != 9 {
		t.Fatalf("ai_credentials count = %d, want 9", len(aiCredentials))
	}
	for _, entry := range aiCredentials {
		if entry.Section != "ai_credentials" {
			t.Fatalf("filtered entry section = %q", entry.Section)
		}
	}

	multiple, err := catalog.CompleteEntries("storage", "vectorstorage", "storage")
	if err != nil {
		t.Fatalf("CompleteEntries(multiple) error = %v", err)
	}
	if len(multiple) != 2 {
		t.Fatalf("multiple section types = %v", multiple)
	}
	if got := []string{multiple[0].Type, multiple[1].Type}; !slices.Equal(got, []string{"s3", "pgvector"}) {
		t.Fatalf("multiple section types = %v", multiple)
	}
	if entries, err := catalog.CompleteEntries(""); err != nil || len(entries) != 0 {
		if err != nil {
			t.Fatalf("CompleteEntries(empty section) error = %v", err)
		}
		t.Fatalf("empty section filter returned %d entries", len(entries))
	}

	returned, err := catalog.CompleteEntries("credentials")
	if err != nil {
		t.Fatalf("CompleteEntries(credentials) error = %v", err)
	}
	returned[0].ConfigSchema[0] = '['
	credentials, err := catalog.CompleteEntries("credentials")
	if err != nil {
		t.Fatalf("second CompleteEntries(credentials) error = %v", err)
	}
	if credentials[0].ConfigSchema[0] != '{' {
		t.Fatal("CompleteEntries() returned aliased schema bytes")
	}
}

func TestCurrentAvailableCatalogDistinguishesMissingFromPresentEmptyMCPSource(t *testing.T) {
	missing := currentAvailableTestSnapshot(currentDynamicRequiredMissing)
	catalog, err := LoadCurrentAvailableCatalog([]byte(missing))
	if err != nil {
		t.Fatalf("LoadCurrentAvailableCatalog(missing) error = %v", err)
	}
	if catalog.Complete() {
		t.Fatal("missing MCP source reported a complete catalog")
	}
	if _, err := catalog.CompleteEntries(); !errors.Is(err, ErrCurrentAvailableCatalogPartial) {
		t.Fatalf("CompleteEntries() error = %v, want %v", err, ErrCurrentAvailableCatalogPartial)
	}

	presentEmpty := currentAvailableTestSnapshot(currentMCPEmptySourceRevision)
	catalog, err = LoadCurrentAvailableCatalog([]byte(presentEmpty))
	if err != nil {
		t.Fatalf("LoadCurrentAvailableCatalog(present empty) error = %v", err)
	}
	if !catalog.Complete() {
		t.Fatal("verified empty MCP source reported a partial catalog")
	}
	entries, err := catalog.CompleteEntries()
	if err != nil {
		t.Fatalf("CompleteEntries() error = %v", err)
	}
	if len(entries) != 1 || entries[0].Type != "github" {
		t.Fatalf("CompleteEntries() = %+v", entries)
	}
}

func TestLoadCurrentAvailableCatalogRejectsInvalidSnapshot(t *testing.T) {
	valid := currentAvailableTestSnapshot(currentMCPEmptySourceRevision)
	if _, err := LoadCurrentAvailableCatalog([]byte(valid)); err != nil {
		t.Fatalf("LoadCurrentAvailableCatalog(valid) error = %v", err)
	}

	tests := map[string]string{
		"wrong version":     replaceCurrentAvailableTestValue(valid, currentAvailableSnapshotVersion, "v2"),
		"bad revision":      replaceCurrentAvailableTestValue(valid, "1111111111111111111111111111111111111111", "not-a-revision"),
		"dynamic unproven":  replaceCurrentAvailableTestValue(valid, currentMCPEmptySourceRevision, "sha256:54136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a"),
		"empty with MCP":    replaceCurrentAvailableTestValue(valid, `"type":"github"`, `"type":"mcp_github"`),
		"invalid type":      replaceCurrentAvailableTestValue(valid, `"type":"github"`, `"type":"GitHub"`),
		"non-object schema": replaceCurrentAvailableTestValue(valid, `"config_schema":{"type":"object"}`, `"config_schema":[]`),
		"trailing document": valid + `{}`,
		"unknown field":     replaceCurrentAvailableTestValue(valid, `"schema_version":`, `"unknown":true,"schema_version":`),
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadCurrentAvailableCatalog([]byte(raw)); !errors.Is(err, ErrInvalidCurrentAvailableSnapshot) {
				t.Fatalf("error = %v, want %v", err, ErrInvalidCurrentAvailableSnapshot)
			}
		})
	}
}

func currentAvailableTestSnapshot(mcpRevision string) string {
	return `{
		"schema_version":"elitea.current-configuration-available-snapshot.v1",
		"sources":{
			"configurations":"1111111111111111111111111111111111111111",
			"runtime_interface_litellm":"2222222222222222222222222222222222222222",
			"artifacts":"3333333333333333333333333333333333333333",
			"elitea_sdk":"4444444444444444444444444444444444444444",
			"elitea_core":"5555555555555555555555555555555555555555",
			"indexer_worker":"6666666666666666666666666666666666666666"
		},
		"dynamic_sources":{
			"indexer_mcp_configurations":"` + mcpRevision + `",
			"provider_hub_configurations":"current_source_returns_empty"
		},
		"entries":[{
			"type":"github","section":"credentials","config_schema":{"type":"object"},
			"has_test_connection":true,"check_connection_label":null,
			"validation_func":"validate","check_connection_func":"check"
		}]
	}`
}

func assertCurrentAvailableEntry(
	t *testing.T,
	entries []CurrentAvailableConfigurationType,
	typeName string,
	assert func(*testing.T, CurrentAvailableConfigurationType),
) {
	t.Helper()
	for _, entry := range entries {
		if entry.Type == typeName {
			assert(t, entry)
			return
		}
	}
	t.Fatalf("entry %q not found", typeName)
}

func assertCurrentAvailableSchema(t *testing.T, entry CurrentAvailableConfigurationType, title, requiredDataField string) {
	t.Helper()
	var schema struct {
		Title      string `json:"title"`
		Properties struct {
			Data struct {
				Required []string `json:"required"`
			} `json:"data"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(entry.ConfigSchema, &schema); err != nil {
		t.Fatalf("decode %s schema: %v", entry.Type, err)
	}
	if schema.Title != title {
		t.Fatalf("%s title = %q, want %q", entry.Type, schema.Title, title)
	}
	if !slices.Contains(schema.Required, "elitea_title") ||
		!slices.Contains(schema.Required, "label") ||
		!slices.Contains(schema.Required, "type") ||
		!slices.Contains(schema.Required, "data") {
		t.Fatalf("%s outer required = %v", entry.Type, schema.Required)
	}
	if !slices.Contains(schema.Properties.Data.Required, requiredDataField) {
		t.Fatalf("%s data required = %v, want %q", entry.Type, schema.Properties.Data.Required, requiredDataField)
	}
}

func assertCurrentAvailableDataProperties(t *testing.T, entry CurrentAvailableConfigurationType, names ...string) {
	t.Helper()
	var schema struct {
		Properties struct {
			Data struct {
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"data"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(entry.ConfigSchema, &schema); err != nil {
		t.Fatalf("decode %s schema: %v", entry.Type, err)
	}
	for _, name := range names {
		if _, ok := schema.Properties.Data.Properties[name]; !ok {
			t.Fatalf("%s data property %q is missing", entry.Type, name)
		}
	}
}

func replaceCurrentAvailableTestValue(value, old, replacement string) string {
	for index := 0; index+len(old) <= len(value); index++ {
		if value[index:index+len(old)] == old {
			return value[:index] + replacement + value[index+len(old):]
		}
	}
	return value
}
