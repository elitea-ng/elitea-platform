package conversations

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
)

func normalizeParticipantSettings(body map[string]any) error {
	if body == nil {
		return fmt.Errorf("settings must be an object")
	}
	if raw, present := body["version_id"]; present && raw != nil {
		id, err := participantPositiveID(raw)
		if err != nil {
			return fmt.Errorf("version_id must be an integer")
		}
		body["version_id"] = id
	}
	return nil
}
func participantPositiveID(raw any) (int64, error) {
	text := ""
	switch value := raw.(type) {
	case string:
		text = value
	case json.Number:
		text = value.String()
	case float64:
		if math.Trunc(value) != value {
			return 0, fmt.Errorf("invalid ID")
		}
		text = strconv.FormatFloat(value, 'f', 0, 64)
	case int:
		text = strconv.Itoa(value)
	case int64:
		text = strconv.FormatInt(value, 10)
	default:
		return 0, fmt.Errorf("invalid ID")
	}
	value, err := strconv.ParseInt(text, 10, 32)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("invalid ID")
	}
	return value, nil
}
func normalizeParticipantLLM(raw any, read bool) (map[string]any, error) {
	input, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("llm_settings must be an object")
	}
	out := map[string]any{"chat_history_template": "all"}
	for _, key := range []string{"temperature", "reasoning_effort", "max_tokens", "model_name", "model_project_id"} {
		out[key] = nil
	}
	for key, value := range input {
		switch key {
		case "temperature":
			if value != nil {
				switch value.(type) {
				case float64, int, json.Number:
				default:
					return nil, fmt.Errorf("temperature must be numeric")
				}
			}
		case "reasoning_effort":
			if value != nil && value != "low" && value != "medium" && value != "high" {
				return nil, fmt.Errorf("reasoning_effort must be low, medium or high")
			}
		case "model_name":
			if value != nil {
				if _, ok := value.(string); !ok {
					return nil, fmt.Errorf("model_name must be a string")
				}
			}
		case "max_tokens", "model_project_id":
			if value != nil {
				id, err := participantPositiveID(value)
				if err != nil {
					return nil, fmt.Errorf("%s must be a positive integer", key)
				}
				value = id
			}
		case "chat_history_template":
			if value != "all" && value != "context_managed" {
				if _, err := participantPositiveID(value); err != nil {
					return nil, fmt.Errorf("invalid chat_history_template")
				}
			}
		default:
			continue // Legacy Pydantic model ignores unknown fields.
		}
		out[key] = value
	}
	if out["temperature"] != nil && out["reasoning_effort"] != nil {
		if !read {
			return nil, fmt.Errorf("temperature is not allowed together with reasoning_effort")
		}
		out["temperature"] = nil
	}
	return out, nil
}
func participantLLMComparable(value map[string]any) map[string]any {
	out := map[string]any{}
	for key, v := range value {
		if v != nil {
			out[key] = fmt.Sprint(v)
		}
	}
	return out
}

// Validate checks participant identity fields before any mapping is written.
func (p Participant) Validate() error {
	if p.EntityMeta == nil {
		return fmt.Errorf("entity_meta must be an object")
	}
	switch p.EntityName {
	case "dummy":
		return nil
	case "user":
		_, err := participantPositiveID(p.EntityMeta["id"])
		if err != nil {
			return fmt.Errorf("user participant requires a positive id")
		}
	case "llm":
		name, ok := p.EntityMeta["model_name"].(string)
		if !ok || name == "" {
			return fmt.Errorf("LLM participant requires model_name")
		}
	case "application", "toolkit", "pipeline":
		if _, err := participantPositiveID(p.EntityMeta["id"]); err != nil {
			return fmt.Errorf("entity participant requires a positive id")
		}
		if _, err := participantPositiveID(p.EntityMeta["project_id"]); err != nil {
			return fmt.Errorf("entity participant requires a positive project_id")
		}
	default:
		return fmt.Errorf("unsupported participant type")
	}
	return nil
}
