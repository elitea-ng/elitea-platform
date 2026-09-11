package mcp

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	configurationsapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/configurations"
	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

const (
	internalConfigurationMaxFilterValues = 64
	internalConfigurationMaxFilterLength = 128
	internalConfigurationMaxQueryLength  = 1024
)

var internalConfigurationTitlePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

type internalConfigurationExecutor interface {
	Execute(
		context.Context,
		int64,
		int64,
		internalConfigurationOperation,
		map[string]any,
	) (internalApplicationExecution, error)
}

type handlerInternalConfigurationExecutor struct {
	handler *configurationsapi.Handler
	typed   *configurationsapi.CurrentConfigurationToolHandler
}

func newHandlerInternalConfigurationExecutor(handler *configurationsapi.Handler, typed *configurationsapi.CurrentConfigurationToolHandler) internalConfigurationExecutor {
	if handler == nil {
		return nil
	}
	return &handlerInternalConfigurationExecutor{handler: handler, typed: typed}
}

func (executor *handlerInternalConfigurationExecutor) Execute(
	ctx context.Context,
	projectID int64,
	actorID int64,
	operation internalConfigurationOperation,
	arguments map[string]any,
) (internalApplicationExecution, error) {
	if executor == nil || executor.handler == nil {
		return internalApplicationExecution{}, errors.New("internal configuration executor is unavailable")
	}
	if ctx == nil || projectID <= 0 || actorID <= 0 {
		return internalApplicationExecution{}, errors.New("internal configuration request is invalid")
	}
	if err := ctx.Err(); err != nil {
		return internalApplicationExecution{}, err
	}

	actor := strconv.FormatInt(actorID, 10)
	ctx = auth.ContextWithUser(ctx, auth.User{ID: actor, UserID: actor})
	params := map[string]string{"projectID": strconv.FormatInt(projectID, 10)}

	switch operation {
	case internalListStoredConfigurationTypes, internalListConfigurationModels, internalSetDefaultConfigurationModel:
		return executor.executeTyped(ctx, operation, arguments, params)
	case internalListConfigurationTypesAvailable:
		query, result := internalConfigurationAvailableQuery(arguments)
		if result != nil {
			return *result, nil
		}
		if raw, present := arguments["type"]; present {
			selected, ok := raw.(string)
			if !ok || len(selected) == 0 || len(selected) > 128 {
				return internalConfigurationBadRequest("type must be a non-empty bounded string")
			}
			query.Set("type", selected)
		}
		query.Set("project_id", params["projectID"])
		return invokeInternalHandler(ctx, http.MethodGet, query, nil, params, executor.handler.DiscoverAvailable)
	case internalListConfigurations:
		query, result := internalConfigurationListQuery(arguments)
		if result != nil {
			return *result, nil
		}
		return invokeInternalHandler(ctx, http.MethodGet, query, nil, params, executor.handler.List)
	case internalCreateConfiguration:
		body, result := internalConfigurationWriteBody(arguments, true)
		if result != nil {
			return *result, nil
		}
		return invokeInternalHandler(ctx, http.MethodPost, nil, body, params, executor.handler.Create)
	case internalGetConfiguration:
		configID, err := requiredPositiveID(arguments, "config_id")
		if err != nil {
			return internalConfigurationBadRequest(err.Error())
		}
		params["configID"] = configID
		return invokeInternalHandler(ctx, http.MethodGet, nil, nil, params, executor.handler.Get)
	case internalUpdateConfiguration:
		configID, err := requiredPositiveID(arguments, "config_id")
		if err != nil {
			return internalConfigurationBadRequest(err.Error())
		}
		body, result := internalConfigurationWriteBody(arguments, false)
		if result != nil {
			return *result, nil
		}
		params["configID"] = configID
		return invokeInternalHandler(ctx, http.MethodPut, nil, body, params, executor.handler.Update)
	default:
		return internalApplicationExecution{}, errors.New("unknown internal configuration operation")
	}
}

func internalConfigurationAvailableQuery(arguments map[string]any) (url.Values, *internalApplicationExecution) {
	values, err := boundedStringArrayArgument(
		arguments, "section", internalConfigurationMaxFilterValues, internalConfigurationMaxFilterLength,
	)
	if err != nil {
		result, _ := internalConfigurationBadRequest(err.Error())
		return nil, &result
	}
	query := url.Values{}
	for _, value := range values {
		query.Add("section", value)
	}
	return query, nil
}

func internalConfigurationListQuery(arguments map[string]any) (url.Values, *internalApplicationExecution) {
	if raw, present := arguments["ids"]; present {
		value, ok := raw.(string)
		if !ok {
			result, _ := internalConfigurationBadRequest("ids must be a comma-separated string")
			return nil, &result
		}
		if _, err := configurationapp.ParseCurrentConfigurationIDs(value); err != nil {
			result, _ := internalConfigurationBadRequest("invalid configuration ids")
			return nil, &result
		}
	}

	query, failure := internalConfigurationAvailableQuery(arguments)
	if failure != nil {
		return nil, failure
	}
	if raw, present := arguments["ids"]; present {
		query.Set("ids", raw.(string))
	}
	for _, name := range []string{"type"} {
		values, err := boundedStringArrayArgument(
			arguments, name, internalConfigurationMaxFilterValues, internalConfigurationMaxFilterLength,
		)
		if err != nil {
			result, _ := internalConfigurationBadRequest(err.Error())
			return nil, &result
		}
		for _, value := range values {
			query.Add(name, value)
		}
	}

	for _, bounded := range []struct {
		name     string
		fallback int
		minimum  int
		maximum  int
	}{
		{"offset", 0, 0, 1<<31 - 1},
		{"limit", 20, 1, 200},
		{"shared_offset", 0, 0, 1<<31 - 1},
		{"shared_limit", 20, 1, 200},
	} {
		value, err := boundedIntegerArgument(
			arguments, bounded.name, bounded.fallback, bounded.minimum, bounded.maximum,
		)
		if err != nil {
			result, _ := internalConfigurationBadRequest(err.Error())
			return nil, &result
		}
		query.Set(bounded.name, strconv.Itoa(value))
	}

	if raw, present := arguments["query"]; present {
		value, ok := raw.(string)
		if !ok || utf8.RuneCountInString(value) > internalConfigurationMaxQueryLength {
			result, _ := internalConfigurationBadRequest("query is outside its supported range")
			return nil, &result
		}
		query.Set("query", value)
	}
	if raw, present := arguments["include_shared"]; present {
		value, ok := raw.(bool)
		if !ok {
			result, _ := internalConfigurationBadRequest("include_shared must be a boolean")
			return nil, &result
		}
		query.Set("include_shared", strconv.FormatBool(value))
	}
	for name, allowed := range map[string]map[string]struct{}{
		"sort_by": {
			"id": {}, "uuid": {}, "project_id": {}, "label": {}, "elitea_title": {}, "type": {},
			"section": {}, "data": {}, "meta": {}, "shared": {}, "status_ok": {}, "status_logs": {},
			"source": {}, "author_id": {}, "created_at": {}, "updated_at": {},
		},
		"sort_order": {"asc": {}, "desc": {}},
	} {
		raw, present := arguments[name]
		if !present {
			continue
		}
		value, ok := raw.(string)
		if !ok {
			result, _ := internalConfigurationBadRequest(name + " is invalid")
			return nil, &result
		}
		if _, ok := allowed[value]; !ok {
			result, _ := internalConfigurationBadRequest(name + " is invalid")
			return nil, &result
		}
		query.Set(name, value)
	}
	return query, nil
}

func internalConfigurationWriteBody(
	arguments map[string]any,
	create bool,
) (map[string]any, *internalApplicationExecution) {
	body := make(map[string]any, 6)
	if raw, present := arguments["elitea_title"]; present {
		title, err := internalConfigurationTitle(raw)
		if err != nil {
			result, _ := internalConfigurationBadRequest(err.Error())
			return nil, &result
		}
		body["elitea_title"] = title
	}
	if create {
		if _, present := body["elitea_title"]; !present {
			result, _ := internalConfigurationBadRequest("elitea_title is required")
			return nil, &result
		}
		rawType, present := arguments["type"]
		configType, ok := rawType.(string)
		if !present || !ok || strings.TrimSpace(configType) == "" || utf8.RuneCountInString(configType) > 128 {
			result, _ := internalConfigurationBadRequest("type must contain between 1 and 128 characters")
			return nil, &result
		}
		body["type"] = configType
	}
	if raw, present := arguments["label"]; present {
		label, ok := raw.(string)
		if !ok || utf8.RuneCountInString(label) > 768 {
			result, _ := internalConfigurationBadRequest("label must be a string of at most 768 characters")
			return nil, &result
		}
		body["label"] = label
	}
	if create {
		if _, present := body["label"]; !present {
			result, _ := internalConfigurationBadRequest("label is required")
			return nil, &result
		}
	}
	if raw, present := arguments["shared"]; present {
		shared, ok := raw.(bool)
		if !ok {
			result, _ := internalConfigurationBadRequest("shared must be a boolean")
			return nil, &result
		}
		body["shared"] = shared
	}
	objectFields := []string{"data"}
	if !create {
		objectFields = append(objectFields, "meta")
	}
	for _, name := range objectFields {
		raw, present := arguments[name]
		if !present {
			continue
		}
		value, ok := raw.(map[string]any)
		if !ok {
			result, _ := internalConfigurationBadRequest(name + " must be an object")
			return nil, &result
		}
		body[name] = value
	}
	if create {
		if _, present := body["data"]; !present {
			result, _ := internalConfigurationBadRequest("data is required")
			return nil, &result
		}
	} else if len(body) == 0 {
		result, _ := internalConfigurationBadRequest("at least one configuration field must be supplied")
		return nil, &result
	}
	return body, nil
}

func internalConfigurationTitle(raw any) (string, error) {
	value, ok := raw.(string)
	if !ok || utf8.RuneCountInString(value) < 1 || utf8.RuneCountInString(value) > 128 ||
		!internalConfigurationTitlePattern.MatchString(value) {
		return "", errors.New("elitea_title must use 1-128 letters, digits, underscores, or hyphens")
	}
	return strings.ToLower(value), nil
}

func boundedStringArrayArgument(
	arguments map[string]any,
	name string,
	maxItems int,
	maxLength int,
) ([]string, error) {
	raw, present := arguments[name]
	if !present {
		return nil, nil
	}
	values, ok := raw.([]any)
	if !ok || len(values) > maxItems {
		return nil, errors.New(name + " must be an array inside its supported bound")
	}
	result := make([]string, 0, len(values))
	for _, rawValue := range values {
		value, ok := rawValue.(string)
		if !ok || utf8.RuneCountInString(value) > maxLength {
			return nil, errors.New(name + " contains an invalid value")
		}
		result = append(result, value)
	}
	return result, nil
}

func internalConfigurationBadRequest(message string) (internalApplicationExecution, error) {
	return jsonExecution(http.StatusBadRequest, map[string]any{"error": message})
}
