package agentexecution

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	scope "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/executionchildscope"
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
		return nil, unsupportedStart("original source owner cannot restore a continuation")
	}
	original, err := owner.RestoreOriginalRootContinuation(ctx, request)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, contextErr
		}
		return nil, unsupportedStartBecause("original source restore", err)
	}
	if original.Source.ResourceProjectID != request.ProjectID || original.Source.ActorID != request.ActorID {
		return nil, unsupportedStart("original source belongs to another project or actor")
	}
	verified, err := scope.DecodeSourceWire(original.Source.CanonicalWire, original.Source.PreRedemptionVersion, original.Source.Reference, request.ProjectID, request.ActorID)
	if err != nil {
		return nil, unsupportedStartBecause("original source wire does not verify", err)
	}
	if verified.Instructions != original.Source.Instructions {
		return nil, unsupportedStart("original source instructions differ from the verified wire")
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
		return nil, unsupportedStart("original admitted application handles are empty, oversized or not an array")
	}
	version, err := decodeCurrentApplicationVersion(source)
	if err != nil {
		return nil, unsupportedStartBecause("original source version is not one decodable JSON object", err)
	}
	sourceTools, ok := version["tools"].([]any)
	if !ok {
		return nil, unsupportedStart("original source version tools is not an array")
	}
	// The same number-preserving parser as the actual freezer validates handles.
	container := append([]byte(`{"tools":`), admittedTools...)
	container = append(container, '}')
	admitted, err := decodeCurrentApplicationVersion(container)
	if err != nil {
		return nil, unsupportedStartBecause("original admitted application handles are not decodable", err)
	}
	handles := make(map[string]map[string]any)
	for _, raw := range admitted["tools"].([]any) {
		tool, ok := raw.(map[string]any)
		if !ok || tool["type"] != "application" {
			return nil, unsupportedStart("original admitted handle is not an application tool")
		}
		// Both sides of the comparison below are read in the runtime's
		// spelling. The admitted copy already is; this keeps that true for a
		// handle admitted before the stored-to-runtime rename existed.
		normalizeCurrentAgentTypeField(tool)
		frozen, ok := originalApplicationHandle(tool, project, actor)
		if !ok {
			return nil, unsupportedStart("original admitted application handle is not a valid frozen reference")
		}
		key := originalApplicationHandleKey(frozen)
		if _, duplicate := handles[key]; duplicate {
			return nil, unsupportedStart("original admitted application handles repeat one application version")
		}
		handles[key] = frozen
	}
	changed := false
	for index, raw := range sourceTools {
		tool, ok := raw.(map[string]any)
		if !ok {
			return nil, unsupportedStart("original source tool is not an object")
		}
		if tool["type"] != "application" {
			continue
		}
		// A raw editable registry never replaces the original admitted registry.
		delete(tool, currentNestedSkillRegistryField)
		// The captured source is raw: it keeps the stored agent_type ("openai")
		// that turn admission renamed for the runtime. Bring it to the same
		// spelling as the admitted copy before comparing; every other field
		// stays strictly compared.
		normalizeCurrentAgentTypeField(tool)
		frozen, ok := originalApplicationHandle(tool, project, actor)
		if !ok {
			return nil, unsupportedStart("original source application handle is not a valid frozen reference")
		}
		key := originalApplicationHandleKey(frozen)
		original, exists := handles[key]
		if !exists {
			return nil, unsupportedStart("original source names an application the turn did not admit")
		}
		registry := original[currentNestedSkillRegistryField]
		delete(original, currentNestedSkillRegistryField)
		left, leftErr := json.Marshal(frozen)
		right, rightErr := json.Marshal(original)
		if leftErr != nil || rightErr != nil || !bytes.Equal(left, right) {
			return nil, unsupportedStart("original source application handle differs from the admitted handle")
		}
		if registry != nil {
			original[currentNestedSkillRegistryField] = registry
		}
		changed = true
		sourceTools[index] = original
		delete(handles, key)
	}
	if len(handles) != 0 {
		return nil, unsupportedStart("original admitted application handle is absent from the source")
	}
	if !changed {
		return bytes.Clone(source), nil
	}
	version["tools"] = sourceTools
	encoded, err := json.Marshal(version)
	if err != nil || !validJSONObject(encoded) || len(encoded) > 8388608 {
		return nil, unsupportedStart("restored original source version is not one bounded JSON object")
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
