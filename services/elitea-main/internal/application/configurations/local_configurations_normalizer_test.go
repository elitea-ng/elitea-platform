package configurations

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestCurrentLocalConfigurationCreateNormalizerDefaultsAndDeclaredFields(t *testing.T) {
	credentials := map[string]any{"elitea_title": "public", "private": false}
	tests := []struct {
		name     string
		typeName string
		data     map[string]any
		want     map[string]any
	}{
		{
			name:     "llm model",
			typeName: "llm_model",
			data:     map[string]any{"name": "gpt", "ai_credentials": credentials, "extra": "ignored"},
			want: map[string]any{
				"name": "gpt", "context_window": int64(128000), "max_output_tokens": int64(16000),
				"supports_reasoning": false, "supports_vision": true, "low_tier": false,
				"high_tier": false, "openai_compatible": false,
				"ai_credentials": map[string]any{"elitea_title": "public", "private": false},
			},
		},
		{
			name:     "embedding model",
			typeName: "embedding_model",
			data:     map[string]any{"name": "embedding", "ai_credentials": credentials},
			want: map[string]any{
				"name": "embedding", "ai_credentials": map[string]any{"elitea_title": "public", "private": false},
			},
		},
		{
			name:     "image generation model",
			typeName: "image_generation_model",
			data:     map[string]any{"name": "image", "ai_credentials": credentials},
			want: map[string]any{
				"name": "image", "ai_credentials": map[string]any{"elitea_title": "public", "private": false},
			},
		},
		{
			name:     "asr model",
			typeName: "asr_model",
			data:     map[string]any{"name": "speech", "ai_credentials": credentials},
			want: map[string]any{
				"name": "speech", "ai_credentials": map[string]any{"elitea_title": "public", "private": false},
			},
		},
		{
			name:     "tts preserves current missing credentials behavior",
			typeName: "tts_model",
			data:     map[string]any{"name": "voice"},
			want:     map[string]any{"name": "voice", "ai_credentials": nil},
		},
		{
			name:     "environment settings",
			typeName: "environment_settings",
			data:     map[string]any{"extra": true},
			want:     map[string]any{"system_sender_name": "Elitea", "error_toast_duration": int64(20000)},
		},
		{
			name:     "project context",
			typeName: "project_context",
			data:     map[string]any{"extra": true},
			want:     map[string]any{"content": "", "enabled": true},
		},
		{
			name:     "project icon",
			typeName: "project_icon",
			data:     map[string]any{},
			want:     map[string]any{"icon_meta": nil},
		},
		{
			name:     "service prompt",
			typeName: "service_prompt",
			data:     map[string]any{"key": "CODE_ASSISTANT", "prompt": "Prompt", "extra": true},
			want:     map[string]any{"key": "code_assistant", "prompt": "Prompt"},
		},
	}

	normalizer := CurrentLocalConfigurationCreateNormalizer{}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := normalizer.NormalizeCreate(test.typeName, test.data)
			if err != nil {
				t.Fatalf("NormalizeCreate() error = %v", err)
			}
			if !result.Complete {
				t.Fatal("NormalizeCreate() did not mark a locally owned type complete")
			}
			if !reflect.DeepEqual(result.Data, test.want) {
				t.Fatalf("NormalizeCreate() data = %#v, want %#v", result.Data, test.want)
			}
		})
	}
}

func TestCurrentLocalConfigurationCreateNormalizerPydanticCoercion(t *testing.T) {
	inputCredentials := map[string]any{
		"elitea_title": "public",
		"private":      "yes",
		"extra":        "ignored",
	}
	input := map[string]any{
		"name":               "gpt",
		"context_window":     "4_2",
		"max_output_tokens":  json.Number("3.0"),
		"supports_reasoning": "y",
		"supports_vision":    "off",
		"low_tier":           json.Number("1"),
		"high_tier":          float64(0),
		"openai_compatible":  nil,
		"ai_credentials":     inputCredentials,
	}

	result, err := (CurrentLocalConfigurationCreateNormalizer{}).NormalizeCreate("llm_model", input)
	if err != nil {
		t.Fatalf("NormalizeCreate() error = %v", err)
	}
	want := map[string]any{
		"name": "gpt", "context_window": int64(42), "max_output_tokens": int64(3),
		"supports_reasoning": true, "supports_vision": false, "low_tier": true,
		"high_tier": false, "openai_compatible": nil,
		"ai_credentials": map[string]any{"elitea_title": "public", "private": true},
	}
	if !reflect.DeepEqual(result.Data, want) {
		t.Fatalf("NormalizeCreate() data = %#v, want %#v", result.Data, want)
	}
	if inputCredentials["extra"] != "ignored" || inputCredentials["private"] != "yes" {
		t.Fatalf("NormalizeCreate() mutated caller data: %#v", inputCredentials)
	}

	environment, err := (CurrentLocalConfigurationCreateNormalizer{}).NormalizeCreate(
		"environment_settings",
		map[string]any{"system_sender_name": "System", "error_toast_duration": "5000"},
	)
	if err != nil {
		t.Fatalf("NormalizeCreate(environment_settings) error = %v", err)
	}
	if got := environment.Data["error_toast_duration"]; got != int64(5000) {
		t.Fatalf("error_toast_duration = %#v, want int64(5000)", got)
	}

	contextResult, err := (CurrentLocalConfigurationCreateNormalizer{}).NormalizeCreate(
		"project_context",
		map[string]any{"content": "context", "enabled": "OFF"},
	)
	if err != nil {
		t.Fatalf("NormalizeCreate(project_context) error = %v", err)
	}
	if got := contextResult.Data["enabled"]; got != false {
		t.Fatalf("enabled = %#v, want false", got)
	}
}

func TestCurrentLocalConfigurationCreateNormalizerPreservesUnboundedPydanticInteger(t *testing.T) {
	result, err := (CurrentLocalConfigurationCreateNormalizer{}).NormalizeCreate("llm_model", map[string]any{
		"name":               "gpt",
		"context_window":     "9223372036854775808.0",
		"ai_credentials":     map[string]any{"elitea_title": "public", "private": false},
		"max_output_tokens":  json.Number("1e3"),
		"supports_reasoning": nil,
	})
	if err != nil {
		t.Fatalf("NormalizeCreate() error = %v", err)
	}
	if got := result.Data["context_window"]; got != json.Number("9223372036854775808") {
		t.Fatalf("context_window = %#v", got)
	}
	if got := result.Data["max_output_tokens"]; got != int64(1000) {
		t.Fatalf("max_output_tokens = %#v", got)
	}
	if got := result.Data["supports_reasoning"]; got != nil {
		t.Fatalf("supports_reasoning = %#v, want nil", got)
	}
}

func TestCurrentLocalConfigurationCreateNormalizerRejectsCurrentInvalidInputs(t *testing.T) {
	tests := []struct {
		name      string
		typeName  string
		data      map[string]any
		wantField string
	}{
		{name: "null data", typeName: "project_icon", data: nil, wantField: "data"},
		{name: "missing model name", typeName: "llm_model", data: map[string]any{}, wantField: "data.name"},
		{name: "fractional model integer", typeName: "llm_model", data: map[string]any{"name": "gpt", "context_window": json.Number("1.5")}, wantField: "data.context_window"},
		{name: "decimal model integer outside Pydantic float range", typeName: "llm_model", data: map[string]any{"name": "gpt", "context_window": json.Number("9223372036854775808.0")}, wantField: "data.context_window"},
		{name: "model boolean whitespace", typeName: "llm_model", data: map[string]any{"name": "gpt", "supports_vision": " true "}, wantField: "data.supports_vision"},
		{name: "llm credentials required", typeName: "llm_model", data: map[string]any{"name": "gpt"}, wantField: "data.ai_credentials"},
		{name: "embedding credentials required", typeName: "embedding_model", data: map[string]any{"name": "embedding"}, wantField: "data.ai_credentials"},
		{name: "image credentials required", typeName: "image_generation_model", data: map[string]any{"name": "image"}, wantField: "data.ai_credentials"},
		{name: "asr credentials required", typeName: "asr_model", data: map[string]any{"name": "speech"}, wantField: "data.ai_credentials"},
		{name: "credential title required", typeName: "llm_model", data: map[string]any{"name": "gpt", "ai_credentials": map[string]any{"private": false}}, wantField: "data.ai_credentials.elitea_title"},
		{name: "credential private required", typeName: "llm_model", data: map[string]any{"name": "gpt", "ai_credentials": map[string]any{"elitea_title": "public"}}, wantField: "data.ai_credentials.private"},
		{name: "environment sender empty", typeName: "environment_settings", data: map[string]any{"system_sender_name": ""}, wantField: "data.system_sender_name"},
		{name: "environment duration below minimum", typeName: "environment_settings", data: map[string]any{"error_toast_duration": 4999}, wantField: "data.error_toast_duration"},
		{name: "project context null enabled", typeName: "project_context", data: map[string]any{"enabled": nil}, wantField: "data.enabled"},
		{name: "project context too long", typeName: "project_context", data: map[string]any{"content": strings.Repeat("🙂", 2501)}, wantField: "data.content"},
		{name: "project icon must be object", typeName: "project_icon", data: map[string]any{"icon_meta": []any{}}, wantField: "data.icon_meta"},
		{name: "project icon name must be string", typeName: "project_icon", data: map[string]any{"icon_meta": map[string]any{"name": 1}}, wantField: "data.icon_meta.name"},
		{name: "service prompt does not trim", typeName: "service_prompt", data: map[string]any{"key": " code_assistant ", "prompt": "Prompt"}, wantField: "data.key"},
		{name: "service prompt key is predefined", typeName: "service_prompt", data: map[string]any{"key": "custom", "prompt": "Prompt"}, wantField: "data.key"},
		{name: "service prompt cannot be empty", typeName: "service_prompt", data: map[string]any{"key": "code_assistant", "prompt": ""}, wantField: "data.prompt"},
	}

	normalizer := CurrentLocalConfigurationCreateNormalizer{}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := normalizer.NormalizeCreate(test.typeName, test.data)
			if err == nil {
				t.Fatalf("NormalizeCreate() result = %#v, want error", result)
			}
			if result.Complete {
				t.Fatal("NormalizeCreate() marked invalid data complete")
			}
			var mutationError *CurrentConfigurationMutationError
			if !errors.As(err, &mutationError) {
				t.Fatalf("NormalizeCreate() error = %T, want CurrentConfigurationMutationError", err)
			}
			if mutationError.Code != CurrentConfigurationMutationInvalid || mutationError.Field != test.wantField {
				t.Fatalf("NormalizeCreate() error = %#v, want invalid field %q", mutationError, test.wantField)
			}
		})
	}
}

func TestCurrentLocalConfigurationCreateNormalizerLeavesOtherTypesForNextNormalizer(t *testing.T) {
	result, err := (CurrentLocalConfigurationCreateNormalizer{}).NormalizeCreate(
		"github",
		map[string]any{"token": "do-not-touch"},
	)
	if err != nil {
		t.Fatalf("NormalizeCreate() error = %v", err)
	}
	if result.Complete || result.Data != nil {
		t.Fatalf("NormalizeCreate() result = %#v, want incomplete zero result", result)
	}
}

func TestCurrentLocalModelPreservesOptionalInputCeiling(t *testing.T) {
	for _, value := range []any{nil, "272000", 0, -1, "invalid"} {
		result, err := (CurrentLocalConfigurationCreateNormalizer{}).NormalizeCreate("llm_model", map[string]any{
			"name": "model", "max_input_tokens": value,
			"ai_credentials": map[string]any{"elitea_title": "public", "private": false},
		})
		if value != nil && value != "272000" {
			if err == nil {
				t.Fatal("invalid input ceiling accepted")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if value == nil {
			if _, exists := result.Data["max_input_tokens"]; exists {
				t.Fatal("invented input ceiling")
			}
		} else if result.Data["max_input_tokens"] != int64(272000) {
			t.Fatal("lost explicit input ceiling")
		}
	}
}
