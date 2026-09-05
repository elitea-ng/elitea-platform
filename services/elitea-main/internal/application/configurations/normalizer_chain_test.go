package configurations

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestCurrentConfigurationDataNormalizerOwnsEveryPinnedTypeExactlyOnce(t *testing.T) {
	catalog := currentMutationTestCatalog(t)
	validator := &currentNormalizerChainValidator{}
	normalizer, err := NewCurrentConfigurationDataNormalizer(
		catalog,
		&currentSDKExpanderStub{result: map[string]any{"expanded": true}},
		validator,
	)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := catalog.CompleteEntries()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 52 {
		t.Fatalf("catalog entries = %d, want 52", len(entries))
	}

	sdkTypes := 0
	for _, entry := range entries {
		dataSchema, ok := catalog.DataSchemaByType(entry.Type)
		if !ok {
			t.Fatalf("type %q has no data schema", entry.Type)
		}
		owners := 0
		if entry.UsesSDKValidation() {
			owners++
			sdkTypes++
		}
		if currentLocalConfigurationType(entry.Type) {
			owners++
		}
		if currentLiteLLMCredentialType(entry.Type) {
			owners++
		}
		if currentGatewayCredentialType(entry.Type) {
			owners++
		}
		if currentArtifactsConfigurationType(entry.Type) {
			owners++
		}
		if owners != 1 {
			t.Fatalf("type %q owners = %d, want 1", entry.Type, owners)
		}

		data := currentNormalizerChainValidCreateData(entry.Type)
		result, normalizeErr := normalizer.Normalize(context.Background(), CurrentConfigurationNormalizationRequest{
			Operation:  CurrentConfigurationNormalizationCreate,
			ProjectID:  7,
			AuthorID:   13,
			Type:       entry.Type,
			DataSchema: dataSchema,
			Data:       data,
		})
		if normalizeErr != nil || !result.Complete {
			t.Fatalf("Normalize(create %q) result=%#v error=%v", entry.Type, result, normalizeErr)
		}
		if entry.UsesSDKValidation() && !reflect.DeepEqual(result.Data, data) {
			t.Fatalf("Normalize(create %q) data=%#v, want original %#v", entry.Type, result.Data, data)
		}

		updateData := map[string]any{"max_output_tokens": "42"}
		if entry.Type == "service_prompt" {
			updateData = map[string]any{"key": "code_assistant", "prompt": "Updated"}
		}
		update, updateErr := normalizer.Normalize(context.Background(), CurrentConfigurationNormalizationRequest{
			Operation:  CurrentConfigurationNormalizationUpdate,
			ProjectID:  7,
			AuthorID:   13,
			Type:       entry.Type,
			DataSchema: dataSchema,
			Data:       updateData,
		})
		if updateErr != nil || !update.Complete {
			t.Fatalf("Normalize(update %q) result=%#v error=%v", entry.Type, update, updateErr)
		}
		if entry.Type != "service_prompt" && update.Data["max_output_tokens"] != int64(42) {
			t.Fatalf("Normalize(update %q) data=%#v, want shallow numeric coercion", entry.Type, update.Data)
		}
	}
	if sdkTypes != 32 || validator.calls != 32 {
		t.Fatalf("SDK types=%d validation calls=%d, want 32/32", sdkTypes, validator.calls)
	}
}

func TestCurrentConfigurationDataNormalizerRejectsUnownedCatalogType(t *testing.T) {
	catalog := currentMutationTestCatalog(t)
	catalog.entries = append(catalog.entries, CurrentAvailableConfigurationType{Type: "unowned", Section: "test"})
	catalog.entryIndexes["unowned"] = len(catalog.entries) - 1
	_, err := NewCurrentConfigurationDataNormalizer(
		catalog,
		&currentSDKExpanderStub{},
		&currentNormalizerChainValidator{},
	)
	if err == nil || !strings.Contains(err.Error(), `type "unowned" has 0 create normalizer owners`) {
		t.Fatalf("error = %v, want unowned type failure", err)
	}
}

func currentNormalizerChainValidCreateData(typeName string) map[string]any {
	credentials := map[string]any{"elitea_title": "public", "private": false}
	switch typeName {
	case "llm_model", "embedding_model", "image_generation_model", "asr_model":
		return map[string]any{"name": typeName, "ai_credentials": credentials}
	case "tts_model":
		return map[string]any{"name": typeName}
	case "service_prompt":
		return map[string]any{"key": "code_assistant", "prompt": "Prompt"}
	case "environment_settings", "project_context", "project_icon":
		return map[string]any{}
	case "open_ai", "azure_open_ai", "ai_dial", "ollama", "open_ai_azure", "vllm":
		return map[string]any{"api_base": "https://example.test"}
	case "anthropic":
		return map[string]any{"api_key": "key"}
	case "amazon_bedrock":
		return map[string]any{}
	case "vertex_ai":
		return map[string]any{
			"vertex_project": "project", "vertex_location": "location", "vertex_credentials": "credentials",
		}
	case "s3":
		return map[string]any{"region_name": "region", "storage_url": "https://s3.example.test"}
	case "s3_api_credentials":
		return map[string]any{"access_key_id": "key", "user_id": 13}
	default:
		return map[string]any{"original": typeName}
	}
}

type currentNormalizerChainValidator struct {
	calls int
}

func (v *currentNormalizerChainValidator) ValidateCurrentSDKConfiguration(
	_ context.Context,
	_ CurrentSDKConfigurationValidationRequest,
) error {
	v.calls++
	return nil
}
