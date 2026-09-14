package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"

	conversationsapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/conversations"
	configurationsapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
)

// internalChatSendTarget contains only the selected participant and model input.
// Durable admission must recheck the selection through the shared start service.
type internalChatSendTarget struct {
	participantID int64
	application   bool
	llmSettings   json.RawMessage
}

// resolveInternalChatSendTarget uses the same authorized reads as the browser.
// It never admits work and cannot fall back after a failed admission.
func resolveInternalChatSendTarget(ctx context.Context, projectID int64, conversationUUID string, participantID int64, llmSettings json.RawMessage, conversationGet, modelsGet http.HandlerFunc) (internalChatSendTarget, error) {
	if ctx == nil || projectID <= 0 || participantID < 0 || conversationGet == nil {
		return internalChatSendTarget{}, errors.New("invalid chat target request")
	}
	params := map[string]string{"projectID": strconv.FormatInt(projectID, 10), "conversationID": conversationUUID}
	read, err := invokeInternalHandler(ctx, http.MethodGet, url.Values{"messages_limit": {"0"}}, nil, params, conversationGet)
	if err != nil {
		return internalChatSendTarget{}, err
	}
	if read.status != http.StatusOK {
		return internalChatSendTarget{}, errors.New("chat target is unavailable")
	}
	var conversation conversationsapi.Conversation
	if json.Unmarshal(read.body, &conversation) != nil || conversation.UUID != conversationUUID || conversation.ProjectID != params["projectID"] {
		return internalChatSendTarget{}, errors.New("invalid chat target response")
	}
	var selected *conversationsapi.Participant
	for i := range conversation.Participants {
		candidate := &conversation.Participants[i]
		if (participantID > 0 && int64(candidate.ID) == participantID) || (participantID == 0 && candidate.EntityName == "dummy") {
			if selected != nil {
				return internalChatSendTarget{}, errors.New("ambiguous chat target")
			}
			selected = candidate
		}
	}
	if selected == nil || selected.ID <= 0 {
		return internalChatSendTarget{}, errors.New("chat participant is unavailable")
	}
	target := internalChatSendTarget{participantID: int64(selected.ID)}
	switch selected.EntityName {
	case "application":
		if len(llmSettings) > 0 && string(llmSettings) != "null" {
			return internalChatSendTarget{}, errors.New("application model overrides are unsupported")
		}
		target.application = true
		return target, nil
	case "dummy", "llm":
	default:
		return internalChatSendTarget{}, errors.New("unsupported chat participant")
	}
	if len(llmSettings) > 0 && string(llmSettings) != "null" {
		var settings map[string]json.RawMessage
		if json.Unmarshal(llmSettings, &settings) != nil || settings == nil {
			return internalChatSendTarget{}, errors.New("invalid model settings")
		}
		if len(settings) > 0 {
			target.llmSettings = append(json.RawMessage(nil), llmSettings...)
			return target, nil
		}
	}
	if modelsGet == nil {
		return internalChatSendTarget{}, errors.New("default model catalog is unavailable")
	}
	read, err = invokeInternalHandler(ctx, http.MethodGet, url.Values{"section": {"llm"}, "include_shared": {"true"}}, nil, params, modelsGet)
	if err != nil {
		return internalChatSendTarget{}, err
	}
	if read.status != http.StatusOK {
		return internalChatSendTarget{}, errors.New("default model catalog is unavailable")
	}
	var catalog configurationsapp.CurrentModelCatalogResponse
	if json.Unmarshal(read.body, &catalog) != nil || catalog.DefaultModelName == nil || catalog.DefaultModelProjectID == nil {
		return internalChatSendTarget{}, errors.New("default model is unavailable")
	}
	for _, model := range catalog.Items {
		if model.Name != "" && model.Name == *catalog.DefaultModelName && model.ProjectID == *catalog.DefaultModelProjectID {
			target.llmSettings, err = json.Marshal(map[string]any{"model_name": model.Name, "model_project_id": model.ProjectID})
			return target, err
		}
	}
	return internalChatSendTarget{}, errors.New("default model is not in the authorized catalog")
}
