package agentexecution

import (
	"encoding/json"
	"math"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
)

// freezeCurrentModelContextLimits replaces any authored limits with the
// authorized catalogue values. It never changes the saved configuration.
func freezeCurrentModelContextLimits(version map[string]any, model configurationapp.CurrentModelCatalogItem) error {
	limits := &runtimev1.ModelContextLimitsV1{}
	read := func(value *int, fallback uint32) (uint32, error) {
		if value == nil {
			return fallback, nil
		}
		if *value <= 0 || uint64(*value) > math.MaxUint32 {
			return 0, unsupportedStart("the model catalogue has an invalid context or output limit")
		}
		return uint32(*value), nil
	}
	var err error
	limits.ContextWindowTokens, err = read(model.ContextWindow, configurationapp.DefaultLLMContextWindow)
	if err != nil {
		return err
	}
	limits.MaxOutputTokens, err = read(model.MaxOutputTokens, configurationapp.DefaultLLMMaxOutputTokens)
	if err != nil {
		return err
	}
	limits.ContextWindowFallback = model.ContextWindowFallback || model.ContextWindow == nil
	limits.MaxOutputFallback = model.MaxOutputFallback || model.MaxOutputTokens == nil
	if limits.MaxOutputTokens >= limits.ContextWindowTokens {
		return unsupportedStart("the model output limit leaves no input context")
	}
	version["model_context_limits"] = limits
	return nil
}

func currentFrozenModelContextLimits(version map[string]any) (*runtimev1.ModelContextLimitsV1, error) {
	value, exists := version["model_context_limits"]
	if !exists {
		return nil, nil // Previously frozen inputs remain readable.
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, ErrUnsupportedCurrentAgentStart
	}
	var limits runtimev1.ModelContextLimitsV1
	if json.Unmarshal(raw, &limits) != nil || limits.ContextWindowTokens == 0 ||
		limits.MaxOutputTokens == 0 || limits.MaxOutputTokens >= limits.ContextWindowTokens {
		return nil, ErrUnsupportedCurrentAgentStart
	}
	return &limits, nil
}
