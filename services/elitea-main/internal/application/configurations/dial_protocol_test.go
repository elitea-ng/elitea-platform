package configurations

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

// dial_protocol_test.go — legacy issue #6707, the configuration half.

func dialModelData(name string, protocol any) map[string]any {
	data := map[string]any{
		"name":           name,
		"ai_credentials": map[string]any{"elitea_title": "epam_dial", "private": false},
	}
	if protocol != "<absent>" {
		data[DialProtocolField] = protocol
	}
	return data
}

func TestValidateLLMModelDialProtocol(t *testing.T) {
	for _, test := range []struct {
		name        string
		model       string
		protocol    any
		want        string
		wantPresent bool
		wantErr     error
	}{
		{name: "absent", model: "gpt-5", protocol: "<absent>"},
		{name: "null", model: "gpt-5", protocol: nil},
		{name: "azure", model: "anthropic.claude-sonnet-4", protocol: "azure", want: "azure", wantPresent: true},
		{name: "anthropic", model: "anthropic.claude-sonnet-4", protocol: "anthropic", want: "anthropic", wantPresent: true},
		{name: "openai on gpt", model: "gpt-5", protocol: "openai", want: "openai", wantPresent: true},
		{name: "openai on claude", model: "claude-sonnet-4-5", protocol: "openai", wantErr: ErrDialProtocolOpenAIIsGPTOnly},
		{name: "openai on bedrock-style id", model: "Anthropic.Claude-Haiku", protocol: "openai", wantErr: ErrDialProtocolOpenAIIsGPTOnly},
		{name: "unknown", model: "gpt-5", protocol: "bedrock", wantErr: ErrInvalidDialProtocol},
		{name: "case is not forgiven", model: "gpt-5", protocol: "OpenAI", wantErr: ErrInvalidDialProtocol},
		{name: "not a string", model: "gpt-5", protocol: 7, wantErr: ErrInvalidDialProtocol},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, present, err := ValidateLLMModelDialProtocol(dialModelData(test.model, test.protocol))
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v, want %v", err, test.wantErr)
			}
			if got != test.want || present != test.wantPresent {
				t.Fatalf("got (%q, %v), want (%q, %v)", got, present, test.want, test.wantPresent)
			}
		})
	}
}

// TestCreateNormalizerKeepsTheDialProtocol proves the create normalizer, which
// drops every field it does not declare, keeps the protocol when it is sent
// and writes nothing when it is not. The second half is the "no existing model
// changes" guarantee: a row created without the field is byte-identical to a
// row created before the field existed.
func TestCreateNormalizerKeepsTheDialProtocol(t *testing.T) {
	normalizer := CurrentLocalConfigurationCreateNormalizer{}

	withField, err := normalizer.NormalizeCreate("llm_model", dialModelData("anthropic.claude-sonnet-4", "anthropic"))
	if err != nil || !withField.Complete {
		t.Fatalf("NormalizeCreate() = (%#v, %v)", withField, err)
	}
	if got := withField.Data[DialProtocolField]; got != "anthropic" {
		t.Fatalf("dial_protocol = %#v, want anthropic", got)
	}

	without, err := normalizer.NormalizeCreate("llm_model", dialModelData("gpt-5", "<absent>"))
	if err != nil {
		t.Fatalf("NormalizeCreate() error = %v", err)
	}
	if _, present := without.Data[DialProtocolField]; present {
		t.Fatalf("dial_protocol was written for a row that did not send it: %#v", without.Data)
	}
	withNull, err := normalizer.NormalizeCreate("llm_model", dialModelData("gpt-5", nil))
	if err != nil {
		t.Fatalf("NormalizeCreate() error = %v", err)
	}
	if !reflect.DeepEqual(without.Data, withNull.Data) {
		t.Fatalf("a null protocol stored %#v, want the absent shape %#v", withNull.Data, without.Data)
	}

	// The field belongs to the chat model only.
	embedding, err := normalizer.NormalizeCreate("embedding_model", dialModelData("text-embedding-3", "openai"))
	if err != nil {
		t.Fatalf("NormalizeCreate(embedding_model) error = %v", err)
	}
	if _, present := embedding.Data[DialProtocolField]; present {
		t.Fatalf("embedding_model kept dial_protocol: %#v", embedding.Data)
	}
}

func TestCreateNormalizerRefusesAnInvalidDialProtocol(t *testing.T) {
	for name, data := range map[string]map[string]any{
		"unknown":          dialModelData("gpt-5", "vertex"),
		"openai on claude": dialModelData("claude-opus-4", "openai"),
	} {
		t.Run(name, func(t *testing.T) {
			result, err := (CurrentLocalConfigurationCreateNormalizer{}).NormalizeCreate("llm_model", data)
			var mutationError *CurrentConfigurationMutationError
			if !errors.As(err, &mutationError) || mutationError.Field != "data.dial_protocol" {
				t.Fatalf("NormalizeCreate() = (%#v, %v), want an invalid data.dial_protocol", result, err)
			}
		})
	}
}

// TestShallowUpdateRefusesAnInvalidDialProtocol covers the update path, which
// otherwise stores `data` as sent.
func TestShallowUpdateRefusesAnInvalidDialProtocol(t *testing.T) {
	normalizer := CurrentPoVDataNormalizer{}
	_, err := normalizer.Normalize(context.Background(), CurrentConfigurationNormalizationRequest{
		Operation: CurrentConfigurationNormalizationUpdate,
		Type:      "llm_model",
		Data:      dialModelData("claude-sonnet-4", "openai"),
	})
	var mutationError *CurrentConfigurationMutationError
	if !errors.As(err, &mutationError) || mutationError.Field != "data.dial_protocol" {
		t.Fatalf("Normalize(update) error = %v, want an invalid data.dial_protocol", err)
	}

	ok, err := normalizer.Normalize(context.Background(), CurrentConfigurationNormalizationRequest{
		Operation: CurrentConfigurationNormalizationUpdate,
		Type:      "llm_model",
		Data:      dialModelData("gpt-5", "openai"),
	})
	if err != nil || !ok.Complete || ok.Data[DialProtocolField] != "openai" {
		t.Fatalf("Normalize(update) = (%#v, %v), want the openai protocol kept", ok, err)
	}
}

// TestRegistrySchemaDeclaresTheDialProtocol pins the form contract: the
// llm_model schema carries a plain string enum with the azure default. A plain
// enum (not anyOf/null) is the shape every schema form renders as a select.
func TestRegistrySchemaDeclaresTheDialProtocol(t *testing.T) {
	catalog, err := LoadPinnedCurrentAvailableCatalog()
	if err != nil {
		t.Fatalf("LoadPinnedCurrentAvailableCatalog() error = %v", err)
	}
	schema, ok := catalog.DataSchemaByType("llm_model")
	if !ok {
		t.Fatal("the registry has no llm_model schema")
	}
	properties, _ := schema["properties"].(map[string]any)
	field, _ := properties[DialProtocolField].(map[string]any)
	if field == nil {
		t.Fatalf("llm_model schema declares no %s property", DialProtocolField)
	}
	if field["type"] != "string" || field["default"] != DialProtocolAzure {
		t.Fatalf("dial_protocol = %#v, want a string with the azure default", field)
	}
	raw, _ := json.Marshal(field["enum"])
	want, _ := json.Marshal(DialProtocols())
	if string(raw) != string(want) {
		t.Fatalf("dial_protocol enum = %s, want %s", raw, want)
	}
	required, _ := schema["required"].([]any)
	for _, name := range required {
		if name == DialProtocolField {
			t.Fatal("dial_protocol must stay optional; an existing row carries no value")
		}
	}
}
