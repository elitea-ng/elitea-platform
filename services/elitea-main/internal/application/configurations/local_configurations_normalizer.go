package configurations

import (
	"encoding/json"
	"math"
	"math/big"
	"strconv"
	"strings"
)

const (
	currentLocalDefaultContextWindow   int64 = DefaultLLMContextWindow
	currentLocalDefaultMaxOutputTokens int64 = DefaultLLMMaxOutputTokens
	currentLocalProjectContextMaxRunes       = 2500
)

// CurrentLocalConfigurationCreateNormalizer ports the create-time
// model_validate(...).model_dump(mode="python") behavior of the nine models
// registered locally by the current Configurations plugin. It intentionally
// does not handle SDK/provider types or PUT's separate shallow coercion rules.
//
// The source boundary is:
//   - pylon_main/plugins/configurations/models/pd/registry.py:131-183
//   - pylon_main/plugins/configurations/models/pd/configuration.py:124-175
type CurrentLocalConfigurationCreateNormalizer struct{}

func currentLocalConfigurationType(typeName string) bool {
	switch typeName {
	case "llm_model", "embedding_model", "image_generation_model", "asr_model", "tts_model",
		"service_prompt", "environment_settings", "project_context", "project_icon":
		return true
	default:
		return false
	}
}

// NormalizeCreate returns Complete=false for a type not owned by the current
// Configurations plugin. For handled types, Data is a new object containing
// only declared model fields, including Pydantic defaults.
func (CurrentLocalConfigurationCreateNormalizer) NormalizeCreate(
	typeName string,
	data map[string]any,
) (CurrentConfigurationNormalizationResult, error) {
	var (
		normalized map[string]any
		err        error
	)

	switch typeName {
	case "llm_model", "embedding_model", "image_generation_model", "asr_model", "tts_model":
		normalized, err = normalizeCurrentLocalAIModel(typeName, data)
	case "service_prompt":
		normalized, err = normalizeCurrentLocalServicePrompt(data)
	case "environment_settings":
		normalized, err = normalizeCurrentLocalEnvironmentSettings(data)
	case "project_context":
		normalized, err = normalizeCurrentLocalProjectContext(data)
	case "project_icon":
		normalized, err = normalizeCurrentLocalProjectIcon(data)
	default:
		return CurrentConfigurationNormalizationResult{Complete: false}, nil
	}
	if err != nil {
		return CurrentConfigurationNormalizationResult{Complete: false}, err
	}
	return CurrentConfigurationNormalizationResult{Data: normalized, Complete: true}, nil
}

func normalizeCurrentLocalAIModel(typeName string, data map[string]any) (map[string]any, error) {
	if data == nil {
		return nil, currentLocalConfigurationFieldError("data")
	}
	name, ok := data["name"].(string)
	if !ok {
		return nil, currentLocalConfigurationFieldError("data.name")
	}

	normalized := map[string]any{"name": name}
	if typeName == "llm_model" {
		contextWindow, err := currentLocalIntegerField(data, "context_window", currentLocalDefaultContextWindow)
		if err != nil {
			return nil, err
		}
		maxOutputTokens, err := currentLocalIntegerField(data, "max_output_tokens", currentLocalDefaultMaxOutputTokens)
		if err != nil {
			return nil, err
		}
		normalized["context_window"] = contextWindow
		normalized["max_output_tokens"] = maxOutputTokens

		for _, field := range [...]struct {
			name         string
			defaultValue bool
		}{
			{name: "supports_reasoning", defaultValue: false},
			{name: "supports_vision", defaultValue: true},
			{name: "low_tier", defaultValue: false},
			{name: "high_tier", defaultValue: false},
			{name: "openai_compatible", defaultValue: false},
		} {
			value, err := currentLocalOptionalBooleanField(data, field.name, field.defaultValue)
			if err != nil {
				return nil, err
			}
			normalized[field.name] = value
		}
	}

	credentials, err := normalizeCurrentLocalAICredentials(data)
	if err != nil {
		return nil, err
	}
	if credentials == nil {
		normalized["ai_credentials"] = nil
	} else {
		normalized["ai_credentials"] = credentials
	}

	// ConfigurationCreate enforces credentials only for these four sections.
	// TTS is intentionally omitted in the current source and remains omitted.
	if credentials == nil && (typeName == "llm_model" || typeName == "embedding_model" ||
		typeName == "image_generation_model" || typeName == "asr_model") {
		return nil, currentLocalConfigurationFieldError("data.ai_credentials")
	}
	return normalized, nil
}

func normalizeCurrentLocalAICredentials(data map[string]any) (map[string]any, error) {
	raw, present := data["ai_credentials"]
	if !present || raw == nil {
		return nil, nil
	}
	credentials, ok := raw.(map[string]any)
	if !ok || credentials == nil {
		return nil, currentLocalConfigurationFieldError("data.ai_credentials")
	}
	title, ok := credentials["elitea_title"].(string)
	if !ok {
		return nil, currentLocalConfigurationFieldError("data.ai_credentials.elitea_title")
	}
	privateRaw, present := credentials["private"]
	if !present || privateRaw == nil {
		return nil, currentLocalConfigurationFieldError("data.ai_credentials.private")
	}
	private, ok := currentLocalPydanticBoolean(privateRaw)
	if !ok {
		return nil, currentLocalConfigurationFieldError("data.ai_credentials.private")
	}
	return map[string]any{"elitea_title": title, "private": private}, nil
}

func normalizeCurrentLocalServicePrompt(data map[string]any) (map[string]any, error) {
	if data == nil {
		return nil, currentLocalConfigurationFieldError("data")
	}
	key, ok := data["key"].(string)
	if !ok || key == "" || len(key) > MaxCurrentConfigurationTitleLength || !currentLocalIdentifier(key) {
		return nil, currentLocalConfigurationFieldError("data.key")
	}
	key = strings.ToLower(key)
	if !currentLocalServicePromptKey(key) {
		return nil, currentLocalConfigurationFieldError("data.key")
	}
	prompt, ok := data["prompt"].(string)
	if !ok || prompt == "" {
		return nil, currentLocalConfigurationFieldError("data.prompt")
	}
	return map[string]any{"key": key, "prompt": prompt}, nil
}

func normalizeCurrentLocalEnvironmentSettings(data map[string]any) (map[string]any, error) {
	if data == nil {
		return nil, currentLocalConfigurationFieldError("data")
	}
	sender := "Elitea"
	if raw, present := data["system_sender_name"]; present {
		value, ok := raw.(string)
		if !ok || value == "" {
			return nil, currentLocalConfigurationFieldError("data.system_sender_name")
		}
		sender = value
	}
	duration, err := currentLocalIntegerField(data, "error_toast_duration", int64(20000))
	if err != nil || !currentLocalIntegerInRange(duration, 5000, 20000) {
		return nil, currentLocalConfigurationFieldError("data.error_toast_duration")
	}
	return map[string]any{
		"system_sender_name":   sender,
		"error_toast_duration": duration,
	}, nil
}

func normalizeCurrentLocalProjectContext(data map[string]any) (map[string]any, error) {
	if data == nil {
		return nil, currentLocalConfigurationFieldError("data")
	}
	content := ""
	if raw, present := data["content"]; present {
		value, ok := raw.(string)
		if !ok || len([]rune(value)) > currentLocalProjectContextMaxRunes {
			return nil, currentLocalConfigurationFieldError("data.content")
		}
		content = value
	}
	enabled, err := currentLocalRequiredBooleanField(data, "enabled", true)
	if err != nil {
		return nil, err
	}
	return map[string]any{"content": content, "enabled": enabled}, nil
}

func normalizeCurrentLocalProjectIcon(data map[string]any) (map[string]any, error) {
	if data == nil {
		return nil, currentLocalConfigurationFieldError("data")
	}
	raw, present := data["icon_meta"]
	if !present || raw == nil {
		return map[string]any{"icon_meta": nil}, nil
	}
	icon, ok := raw.(map[string]any)
	if !ok || icon == nil {
		return nil, currentLocalConfigurationFieldError("data.icon_meta")
	}
	normalized := make(map[string]any, 2)
	for _, field := range [...]string{"name", "url"} {
		rawValue, present := icon[field]
		if !present || rawValue == nil {
			normalized[field] = nil
			continue
		}
		value, ok := rawValue.(string)
		if !ok {
			return nil, currentLocalConfigurationFieldError("data.icon_meta." + field)
		}
		normalized[field] = value
	}
	return map[string]any{"icon_meta": normalized}, nil
}

func currentLocalIntegerField(data map[string]any, field string, defaultValue int64) (any, error) {
	raw, present := data[field]
	if !present {
		return defaultValue, nil
	}
	value, ok := currentLocalPydanticInteger(raw)
	if !ok {
		return nil, currentLocalConfigurationFieldError("data." + field)
	}
	return value, nil
}

func currentLocalOptionalBooleanField(data map[string]any, field string, defaultValue bool) (any, error) {
	raw, present := data[field]
	if !present {
		return defaultValue, nil
	}
	if raw == nil {
		return nil, nil
	}
	value, ok := currentLocalPydanticBoolean(raw)
	if !ok {
		return nil, currentLocalConfigurationFieldError("data." + field)
	}
	return value, nil
}

func currentLocalRequiredBooleanField(data map[string]any, field string, defaultValue bool) (bool, error) {
	raw, present := data[field]
	if !present {
		return defaultValue, nil
	}
	if raw == nil {
		return false, currentLocalConfigurationFieldError("data." + field)
	}
	value, ok := currentLocalPydanticBoolean(raw)
	if !ok {
		return false, currentLocalConfigurationFieldError("data." + field)
	}
	return value, nil
}

func currentLocalPydanticInteger(raw any) (any, bool) {
	switch value := raw.(type) {
	case bool:
		if value {
			return int64(1), true
		}
		return int64(0), true
	case int:
		return int64(value), true
	case int8:
		return int64(value), true
	case int16:
		return int64(value), true
	case int32:
		return int64(value), true
	case int64:
		return value, true
	case uint:
		return currentLocalUnsignedInteger(uint64(value))
	case uint8:
		return int64(value), true
	case uint16:
		return int64(value), true
	case uint32:
		return int64(value), true
	case uint64:
		return currentLocalUnsignedInteger(value)
	case float32:
		return currentLocalFloatInteger(float64(value))
	case float64:
		return currentLocalFloatInteger(value)
	case json.Number:
		return currentLocalJSONNumberInteger(value.String())
	case string:
		return currentLocalStringInteger(value)
	default:
		return nil, false
	}
}

func currentLocalUnsignedInteger(value uint64) (any, bool) {
	if value <= math.MaxInt64 {
		return int64(value), true
	}
	return json.Number(new(big.Int).SetUint64(value).String()), true
}

func currentLocalFloatInteger(value float64) (any, bool) {
	limit := math.Ldexp(1, 63)
	if math.IsNaN(value) || math.IsInf(value, 0) || math.Trunc(value) != value || value <= -limit || value >= limit {
		return nil, false
	}
	return int64(value), true
}

func currentLocalJSONNumberInteger(value string) (any, bool) {
	if strings.ContainsAny(value, ".eE") {
		decimal, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return nil, false
		}
		return currentLocalFloatInteger(decimal)
	}
	integer := new(big.Int)
	if _, ok := integer.SetString(value, 10); !ok {
		return nil, false
	}
	return currentLocalBigInteger(integer), true
}

func currentLocalStringInteger(value string) (any, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, false
	}
	integerPart := value
	if decimal := strings.IndexByte(value, '.'); decimal >= 0 {
		integerPart = value[:decimal]
		fraction := value[decimal+1:]
		if integerPart == "" || integerPart == "+" || integerPart == "-" || fraction == "" {
			return nil, false
		}
		for _, character := range fraction {
			if character != '0' {
				return nil, false
			}
		}
	}
	start := 0
	if integerPart[0] == '+' || integerPart[0] == '-' {
		start = 1
	}
	if start == len(integerPart) {
		return nil, false
	}
	for index := start; index < len(integerPart); index++ {
		character := integerPart[index]
		if character >= '0' && character <= '9' {
			continue
		}
		if character != '_' || index == start || index+1 == len(integerPart) ||
			integerPart[index-1] < '0' || integerPart[index-1] > '9' ||
			integerPart[index+1] < '0' || integerPart[index+1] > '9' {
			return nil, false
		}
	}
	integerPart = strings.ReplaceAll(integerPart, "_", "")
	integer := new(big.Int)
	if _, ok := integer.SetString(integerPart, 10); !ok {
		return nil, false
	}
	return currentLocalBigInteger(integer), true
}

func currentLocalBigInteger(value *big.Int) any {
	if value.IsInt64() {
		return value.Int64()
	}
	return json.Number(value.String())
}

func currentLocalIntegerInRange(value any, minimum, maximum int64) bool {
	switch value := value.(type) {
	case int64:
		return value >= minimum && value <= maximum
	case json.Number:
		integer := new(big.Int)
		if _, ok := integer.SetString(value.String(), 10); !ok {
			return false
		}
		return integer.Cmp(big.NewInt(minimum)) >= 0 && integer.Cmp(big.NewInt(maximum)) <= 0
	default:
		return false
	}
}

func currentLocalPydanticBoolean(raw any) (bool, bool) {
	switch value := raw.(type) {
	case bool:
		return value, true
	case string:
		switch strings.ToLower(value) {
		case "1", "on", "t", "true", "y", "yes":
			return true, true
		case "0", "off", "f", "false", "n", "no":
			return false, true
		default:
			return false, false
		}
	case json.Number:
		integer, ok := currentLocalJSONNumberInteger(value.String())
		if !ok {
			return false, false
		}
		return currentLocalIntegerBoolean(integer)
	case int:
		return currentLocalIntegerBoolean(int64(value))
	case int8:
		return currentLocalIntegerBoolean(int64(value))
	case int16:
		return currentLocalIntegerBoolean(int64(value))
	case int32:
		return currentLocalIntegerBoolean(int64(value))
	case int64:
		return currentLocalIntegerBoolean(value)
	case uint:
		return currentLocalIntegerBoolean(uint64(value))
	case uint8:
		return currentLocalIntegerBoolean(uint64(value))
	case uint16:
		return currentLocalIntegerBoolean(uint64(value))
	case uint32:
		return currentLocalIntegerBoolean(uint64(value))
	case uint64:
		return currentLocalIntegerBoolean(value)
	case float32:
		return currentLocalFloatBoolean(float64(value))
	case float64:
		return currentLocalFloatBoolean(value)
	default:
		return false, false
	}
}

func currentLocalIntegerBoolean(value any) (bool, bool) {
	switch value := value.(type) {
	case int64:
		if value == 0 || value == 1 {
			return value == 1, true
		}
	case uint64:
		if value == 0 || value == 1 {
			return value == 1, true
		}
	case json.Number:
		if value == "0" || value == "1" {
			return value == "1", true
		}
	}
	return false, false
}

func currentLocalFloatBoolean(value float64) (bool, bool) {
	if value == 0 || value == 1 {
		return value == 1, true
	}
	return false, false
}

func currentLocalIdentifier(value string) bool {
	for index := 0; index < len(value); index++ {
		character := value[index]
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || character == '_' || character == '-' {
			continue
		}
		return false
	}
	return true
}

func currentLocalConfigurationFieldError(field string) error {
	return currentMutationFieldError(CurrentConfigurationMutationInvalid, field)
}

// Source: pylon_main/plugins/configurations/models/pd/service_prompt_keys.py:8-21.
func currentLocalServicePromptKey(value string) bool {
	switch value {
	case "code_assistant", "decision_assistant", "edit_application_draft", "generate_application_draft",
		"llm_system_assistant", "llm_task_assistant", "mermaid_quick_fix", "printer_assistant",
		"project_context_generator", "router_assistant", "skill_generator", "state_modifier_assistant":
		return true
	default:
		return false
	}
}
