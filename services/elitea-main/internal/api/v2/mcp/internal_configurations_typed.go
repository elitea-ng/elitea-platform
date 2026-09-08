package mcp

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"unicode/utf8"
)

func (executor *handlerInternalConfigurationExecutor) executeTyped(
	ctx context.Context,
	operation internalConfigurationOperation,
	arguments map[string]any,
	params map[string]string,
) (internalApplicationExecution, error) {
	if executor.typed == nil {
		return jsonExecution(http.StatusServiceUnavailable, map[string]string{"error": "typed configuration services are unavailable"})
	}
	query := url.Values{}
	if raw, present := arguments["section"]; present && !(raw == nil && operation == internalSetDefaultConfigurationModel) {
		section, ok := raw.(string)
		if !ok || utf8.RuneCountInString(section) > internalConfigurationMaxFilterLength {
			return internalConfigurationBadRequest("section must be a bounded string")
		}
		query.Set("section", section)
	}
	switch operation {
	case internalListStoredConfigurationTypes:
		return invokeInternalHandler(ctx, http.MethodGet, query, nil, params, executor.typed.Types)
	case internalListConfigurationModels:
		if raw, present := arguments["include_shared"]; present {
			include, ok := raw.(bool)
			if !ok {
				return internalConfigurationBadRequest("include_shared must be a boolean")
			}
			query.Set("include_shared", strconv.FormatBool(include))
		}
		return invokeInternalHandler(ctx, http.MethodGet, query, nil, params, executor.typed.Models)
	case internalSetDefaultConfigurationModel:
		name, ok := arguments["name"].(string)
		if !ok || name == "" || utf8.RuneCountInString(name) > 1024 {
			return internalConfigurationBadRequest("name must be a non-empty bounded string")
		}
		target, err := requiredPositiveID(arguments, "target_project_id")
		if err != nil {
			return internalConfigurationBadRequest("target_project_id must be a positive integer")
		}
		body := map[string]any{"name": name, "target_project_id": target}
		if section, present := arguments["section"]; present {
			body["section"] = section
		}
		return invokeInternalHandler(ctx, http.MethodPost, nil, body, params, executor.typed.SetDefaultModel)
	default:
		return internalApplicationExecution{}, errors.New("unknown typed configuration operation")
	}
}
