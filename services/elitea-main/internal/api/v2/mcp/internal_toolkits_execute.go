package mcp

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	toolkitsapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/toolkits"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

type internalToolkitExecutor interface {
	Execute(
		context.Context,
		int64,
		int64,
		internalToolkitOperation,
		map[string]any,
	) (internalApplicationExecution, error)
}

type handlerInternalToolkitExecutor struct {
	handler *toolkitsapi.Handler
}

func newHandlerInternalToolkitExecutor(handler *toolkitsapi.Handler) internalToolkitExecutor {
	if handler == nil {
		return nil
	}
	return &handlerInternalToolkitExecutor{handler: handler}
}

func (executor *handlerInternalToolkitExecutor) Execute(
	ctx context.Context,
	projectID int64,
	actorID int64,
	operation internalToolkitOperation,
	arguments map[string]any,
) (internalApplicationExecution, error) {
	if executor == nil || executor.handler == nil {
		return internalApplicationExecution{}, errors.New("internal toolkit executor is unavailable")
	}
	if ctx == nil || projectID <= 0 || actorID <= 0 {
		return internalApplicationExecution{}, errors.New("internal toolkit request is invalid")
	}
	if err := ctx.Err(); err != nil {
		return internalApplicationExecution{}, err
	}

	actor := strconv.FormatInt(actorID, 10)
	ctx = auth.ContextWithUser(ctx, auth.User{ID: actor, UserID: actor})
	project := strconv.FormatInt(projectID, 10)
	params := map[string]string{"projectID": project}

	switch operation {
	case internalAvailableTools:
		toolkitID, err := requiredPositiveID(arguments, "toolkit_id")
		if err != nil {
			return internalToolkitBadRequest(err.Error())
		}
		params["toolkitID"] = toolkitID
		return invokeInternalHandler(ctx, http.MethodGet, nil, nil, params, executor.handler.AvailableTools)
	case internalListToolkitTypes:
		query := url.Values{}
		if raw, present := arguments["type"]; present {
			selected, ok := raw.(string)
			if !ok || len(selected) == 0 || len(selected) > 256 {
				return internalToolkitBadRequest("type must be a non-empty bounded string")
			}
			query.Set("type", selected)
		}
		return invokeInternalHandler(ctx, http.MethodGet, query, nil, params, executor.handler.DiscoverTypeSchemas)
	case internalListToolkits:
		query, result := internalToolkitListQuery(arguments)
		if result != nil {
			return *result, nil
		}
		return invokeInternalHandler(ctx, http.MethodGet, query, nil, params, executor.handler.List)
	case internalCreateToolkit:
		body, result := internalToolkitWriteBody(arguments, true)
		if result != nil {
			return *result, nil
		}
		return invokeInternalHandler(ctx, http.MethodPost, nil, body, params, executor.handler.Create)
	case internalUpdateToolkit:
		toolkitID, err := requiredPositiveID(arguments, "toolkit_id")
		if err != nil {
			return internalToolkitBadRequest(err.Error())
		}
		body, result := internalToolkitWriteBody(arguments, false)
		if result != nil {
			return *result, nil
		}
		params["toolkitID"] = toolkitID
		return invokeInternalHandler(ctx, http.MethodPut, nil, body, params, executor.handler.Update)
	case internalUpdateToolRelation:
		toolkitID, body, result := internalToolkitRelationBody(arguments)
		if result != nil {
			return *result, nil
		}
		params["toolkitID"] = toolkitID
		return invokeInternalHandler(ctx, http.MethodPatch, nil, body, params, executor.handler.Update)
	default:
		return internalApplicationExecution{}, errors.New("unknown internal toolkit operation")
	}
}

func internalToolkitListQuery(arguments map[string]any) (url.Values, *internalApplicationExecution) {
	limit, err := boundedIntegerArgument(arguments, "limit", 20, 1, 100)
	if err != nil {
		result, _ := internalToolkitBadRequest(err.Error())
		return nil, &result
	}
	offset, err := boundedIntegerArgument(arguments, "offset", 0, 0, 1<<31-1)
	if err != nil {
		result, _ := internalToolkitBadRequest(err.Error())
		return nil, &result
	}
	query := url.Values{}
	query.Set("limit", strconv.Itoa(limit))
	query.Set("offset", strconv.Itoa(offset))
	return query, nil
}

func internalToolkitWriteBody(
	arguments map[string]any,
	requireIdentity bool,
) (map[string]any, *internalApplicationExecution) {
	body := make(map[string]any, 5)
	for _, name := range []string{"type", "name", "description"} {
		value, present := arguments[name]
		if !present {
			continue
		}
		text, ok := value.(string)
		if !ok {
			result, _ := internalToolkitBadRequest(name + " must be a string")
			return nil, &result
		}
		if name != "description" && strings.TrimSpace(text) == "" {
			result, _ := internalToolkitBadRequest(name + " must not be empty")
			return nil, &result
		}
		body[name] = text
	}
	if requireIdentity {
		for _, name := range []string{"type", "name"} {
			if _, present := body[name]; !present {
				result, _ := internalToolkitBadRequest(name + " is required")
				return nil, &result
			}
		}
	}
	for _, name := range []string{"settings", "meta"} {
		value, present := arguments[name]
		if !present {
			continue
		}
		object, ok := value.(map[string]any)
		if !ok {
			result, _ := internalToolkitBadRequest(name + " must be an object")
			return nil, &result
		}
		body[name] = object
	}
	if !requireIdentity && len(body) == 0 {
		result, _ := internalToolkitBadRequest("at least one toolkit field must be supplied")
		return nil, &result
	}
	return body, nil
}

func internalToolkitRelationBody(
	arguments map[string]any,
) (string, map[string]any, *internalApplicationExecution) {
	toolkitID, err := requiredPositiveID(arguments, "toolkit_id")
	if err != nil {
		result, _ := internalToolkitBadRequest(err.Error())
		return "", nil, &result
	}
	entityID, err := requiredPositiveID(arguments, "entity_id")
	if err != nil {
		result, _ := internalToolkitBadRequest(err.Error())
		return "", nil, &result
	}
	entityVersionID, err := requiredPositiveID(arguments, "entity_version_id")
	if err != nil {
		result, _ := internalToolkitBadRequest(err.Error())
		return "", nil, &result
	}
	hasRelation, ok := arguments["has_relation"].(bool)
	if !ok {
		result, _ := internalToolkitBadRequest("has_relation must be a boolean")
		return "", nil, &result
	}
	entityType := strings.TrimSpace(stringArgument(arguments["entity_type"]))
	if entityType == "" {
		entityType = "agent"
	}
	if entityType != "agent" {
		result, _ := internalToolkitBadRequest("entity_type must be agent")
		return "", nil, &result
	}
	body := map[string]any{
		"entity_id":         entityID,
		"entity_version_id": entityVersionID,
		"entity_type":       entityType,
		"has_relation":      hasRelation,
	}
	if selected, present := arguments["selected_tools"]; present {
		entries, ok := selected.([]any)
		if !ok {
			result, _ := internalToolkitBadRequest("selected_tools must be an array of tool names")
			return "", nil, &result
		}
		for _, entry := range entries {
			name, ok := entry.(string)
			if !ok || strings.TrimSpace(name) == "" {
				result, _ := internalToolkitBadRequest("selected_tools must be an array of non-empty tool names")
				return "", nil, &result
			}
		}
		body["selected_tools"] = entries
	}
	return toolkitID, body, nil
}

func internalToolkitBadRequest(message string) (internalApplicationExecution, error) {
	return jsonExecution(http.StatusBadRequest, map[string]any{"error": message})
}

func (h *Handler) toolkitDiscoveryAvailable() bool {
	if h == nil {
		return false
	}
	executor, ok := h.internalToolkits.(*handlerInternalToolkitExecutor)
	return ok && executor.handler.DiscoveryAvailable()
}
