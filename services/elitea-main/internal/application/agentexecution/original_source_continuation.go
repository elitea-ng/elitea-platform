package agentexecution

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	scope "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/executionchildscope"
)

// Main reads the single original chat_predict input and source under the same
// locked actor/project/run relation. Only application handles leave that read;
// redeemed settings and runtime-added instructions are not continuation source.
type RootSourceContinuation struct {
	Source           scope.SourceDefinition
	ApplicationTools json.RawMessage
}

type OriginalRootContinuationOwner interface {
	RestoreOriginalRootContinuation(context.Context, RootSourceRestoreRequest) (RootSourceContinuation, error)
}

type restoredRootContinuation struct {
	scope.SourceDefinition
	RuntimeVersionDetails json.RawMessage
}

func (service *CurrentApplicationStartService) restoreContinuationSource(ctx context.Context, request RootSourceRestoreRequest) (*restoredRootContinuation, error) {
	if service.originalSources == nil {
		return nil, nil
	}
	owner, ok := service.originalSources.(OriginalRootContinuationOwner)
	if !ok {
		return nil, ErrUnsupportedCurrentAgentStart
	}
	original, err := owner.RestoreOriginalRootContinuation(ctx, request)
	if err != nil || original.Source.ResourceProjectID != request.ProjectID || original.Source.ActorID != request.ActorID {
		return nil, ErrUnsupportedCurrentAgentStart
	}
	verified, err := scope.DecodeSourceWire(original.Source.CanonicalWire, original.Source.PreRedemptionVersion, original.Source.Reference, request.ProjectID, request.ActorID)
	if err != nil || verified.Instructions != original.Source.Instructions {
		return nil, ErrUnsupportedCurrentAgentStart
	}
	runtime, err := restoreOriginalApplicationHandles(verified.PreRedemptionVersion, original.ApplicationTools, int32(request.ProjectID), int32(request.ActorID))
	if err != nil {
		return nil, err
	}
	return &restoredRootContinuation{SourceDefinition: verified, RuntimeVersionDetails: runtime}, nil
}

// Rehydrate exactly the registry materialized for the original saved handles.
// Matching uses application/version and the whole frozen handle, never a name
// lookup. All non-application source fields remain from the raw capture.
func restoreOriginalApplicationHandles(source, admittedTools json.RawMessage, project, actor int32) (json.RawMessage, error) {
	if len(admittedTools) == 0 || len(admittedTools) > 8388608 || !validJSONArray(admittedTools) {
		return nil, ErrUnsupportedCurrentAgentStart
	}
	version, err := decodeCurrentApplicationVersion(source)
	if err != nil {
		return nil, err
	}
	sourceTools, ok := version["tools"].([]any)
	if !ok {
		return nil, ErrUnsupportedCurrentAgentStart
	}
	// The same number-preserving parser as the actual freezer validates handles.
	container := append([]byte(`{"tools":`), admittedTools...)
	container = append(container, '}')
	admitted, err := decodeCurrentApplicationVersion(container)
	if err != nil {
		return nil, err
	}
	handles := make(map[string]map[string]any)
	for _, raw := range admitted["tools"].([]any) {
		tool, ok := raw.(map[string]any)
		if !ok || tool["type"] != "application" {
			return nil, ErrUnsupportedCurrentAgentStart
		}
		frozen, ok := originalApplicationHandle(tool, project, actor)
		if !ok {
			return nil, ErrUnsupportedCurrentAgentStart
		}
		key := originalApplicationHandleKey(frozen)
		if _, duplicate := handles[key]; duplicate {
			return nil, ErrUnsupportedCurrentAgentStart
		}
		handles[key] = frozen
	}
	changed := false
	for index, raw := range sourceTools {
		tool, ok := raw.(map[string]any)
		if !ok {
			return nil, ErrUnsupportedCurrentAgentStart
		}
		if tool["type"] != "application" {
			continue
		}
		// A raw editable registry never replaces the original admitted registry.
		delete(tool, currentNestedSkillRegistryField)
		frozen, ok := originalApplicationHandle(tool, project, actor)
		if !ok {
			return nil, ErrUnsupportedCurrentAgentStart
		}
		key := originalApplicationHandleKey(frozen)
		original, exists := handles[key]
		if !exists {
			return nil, ErrUnsupportedCurrentAgentStart
		}
		registry := original[currentNestedSkillRegistryField]
		delete(original, currentNestedSkillRegistryField)
		left, leftErr := json.Marshal(frozen)
		right, rightErr := json.Marshal(original)
		if leftErr != nil || rightErr != nil || !bytes.Equal(left, right) {
			return nil, ErrUnsupportedCurrentAgentStart
		}
		if registry != nil {
			original[currentNestedSkillRegistryField] = registry
		}
		changed = true
		sourceTools[index] = original
		delete(handles, key)
	}
	if len(handles) != 0 {
		return nil, ErrUnsupportedCurrentAgentStart
	}
	if !changed {
		return bytes.Clone(source), nil
	}
	version["tools"] = sourceTools
	encoded, err := json.Marshal(version)
	if err != nil || !validJSONObject(encoded) || len(encoded) > 8388608 {
		return nil, ErrUnsupportedCurrentAgentStart
	}
	return encoded, nil
}

func originalApplicationHandle(tool map[string]any, project, actor int32) (map[string]any, bool) {
	if tool["id"] == nil {
		return freezeCurrentAdhocApplicationReference(tool, project, actor)
	}
	return freezeCurrentStoredApplicationReference(tool)
}

func originalApplicationHandleKey(tool map[string]any) string {
	settings := tool["settings"].(map[string]any)
	return fmt.Sprintf("%v/%v/%v", tool["id"], settings["application_id"], settings["application_version_id"])
}
