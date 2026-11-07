package conversations

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/contextview"
)

func normalizeParticipantSettings(body map[string]any) error {
	if body == nil {
		return fmt.Errorf("settings must be an object")
	}
	if err := validParticipantContextHistory(body); err != nil {
		return err
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

// validParticipantContextHistory checks entity_settings.context_history
// (ADR-0050 decision 9b, issue 1609): "share history from: now" when a
// person adds an agent is `since_joined`; `full` (or null) is the version's
// own mode.
func validParticipantContextHistory(settings map[string]any) error {
	raw, present := settings[contextview.ParticipantKey]
	if !present || raw == nil {
		return nil
	}
	if mode, ok := raw.(string); !ok || (contextview.History(mode) != contextview.HistorySinceJoined && contextview.History(mode) != contextview.HistoryFull) {
		return fmt.Errorf("context_history must be since_joined or full")
	}
	return nil
}

func participantPositiveID(raw any) (int64, error) {
	value, err := participantInteger(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("invalid ID")
	}
	return value, nil
}

func participantInteger(raw any) (int64, error) {
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
	if err != nil {
		return 0, fmt.Errorf("invalid integer")
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
			// ADR-0038 decision 1: auto, off, low, medium or high. The
			// stored none of an older client is written as off.
			if value != nil {
				text, isString := value.(string)
				canonical, known := configurationapp.CanonicalReasoningEffort(text)
				if !isString || !known {
					return nil, fmt.Errorf("reasoning_effort must be auto, off, low, medium or high")
				}
				value = canonical
			}
		case "model_name":
			if value != nil {
				if _, ok := value.(string); !ok {
					return nil, fmt.Errorf("model_name must be a string")
				}
			}
		case "max_tokens":
			if value != nil {
				tokens, err := participantInteger(value)
				if err != nil || (tokens <= 0 && tokens != -1) {
					return nil, fmt.Errorf("max_tokens must be a positive integer or -1 for Auto")
				}
				value = tokens
			}
		case "model_project_id":
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
	// off thinks on no step, so it takes a temperature on every model. The
	// model-aware rule for the other values is refuseParticipantReasoning.
	if out["temperature"] != nil && out["reasoning_effort"] != nil &&
		out["reasoning_effort"] != configurationapp.ReasoningEffortOff {
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
		if err := validParticipantContextHistory(p.EntitySettings); err != nil {
			return err
		}
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
