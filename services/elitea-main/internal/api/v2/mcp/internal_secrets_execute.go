package mcp

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

type internalSecretHandler interface {
	List(http.ResponseWriter, *http.Request)
	Create(http.ResponseWriter, *http.Request)
	Update(http.ResponseWriter, *http.Request)
}

type internalSecretExecutor interface {
	Execute(
		context.Context,
		int64,
		int64,
		internalSecretOperation,
		map[string]any,
	) (internalApplicationExecution, error)
}

type handlerInternalSecretExecutor struct {
	handler internalSecretHandler
}

func newHandlerInternalSecretExecutor(handler internalSecretHandler) internalSecretExecutor {
	if handler == nil {
		return nil
	}
	return &handlerInternalSecretExecutor{handler: handler}
}

func (executor *handlerInternalSecretExecutor) Execute(
	ctx context.Context,
	projectID int64,
	actorID int64,
	operation internalSecretOperation,
	arguments map[string]any,
) (internalApplicationExecution, error) {
	if executor == nil || executor.handler == nil {
		return internalApplicationExecution{}, errors.New("internal secret executor is unavailable")
	}
	if ctx == nil || projectID <= 0 || actorID <= 0 {
		return internalApplicationExecution{}, errors.New("internal secret request is invalid")
	}
	if err := ctx.Err(); err != nil {
		return internalApplicationExecution{}, err
	}

	actor := strconv.FormatInt(actorID, 10)
	ctx = auth.ContextWithUser(ctx, auth.User{ID: actor, UserID: actor})
	params := map[string]string{"projectID": strconv.FormatInt(projectID, 10)}

	switch operation {
	case internalListSecrets:
		return invokeInternalHandler(ctx, http.MethodGet, nil, nil, params, executor.handler.List)
	case internalCreateSecret:
		name, result := internalSecretName(arguments, "name")
		if result != nil {
			return *result, nil
		}
		body := map[string]any{"name": name}
		if value, present := arguments["value"]; present {
			body["value"] = value
		}
		return invokeInternalHandler(ctx, http.MethodPost, nil, body, params, executor.handler.Create)
	case internalUpdateSecret:
		name, result := internalSecretName(arguments, "secret")
		if result != nil {
			return *result, nil
		}
		params["name"] = name
		// Current pylon overwrites the request body's name with the path
		// secret. Pinning it here preserves value-rotation semantics and keeps
		// a model-supplied body field from becoming an implicit rename.
		body := map[string]any{"name": name}
		if value, present := arguments["value"]; present {
			body["value"] = value
		}
		return invokeInternalHandler(ctx, http.MethodPut, nil, body, params, executor.handler.Update)
	default:
		return internalApplicationExecution{}, errors.New("unknown internal secret operation")
	}
}

func internalSecretName(arguments map[string]any, field string) (string, *internalApplicationExecution) {
	raw, present := arguments[field]
	name, ok := raw.(string)
	if !present || !ok || len(name) == 0 || len(name) > 128 {
		result, _ := internalSecretBadRequest(field + " must contain between 1 and 128 letters, digits, or underscores")
		return "", &result
	}
	for _, char := range name {
		if (char < 'A' || char > 'Z') && (char < 'a' || char > 'z') &&
			(char < '0' || char > '9') && char != '_' {
			result, _ := internalSecretBadRequest(field + " must contain between 1 and 128 letters, digits, or underscores")
			return "", &result
		}
	}
	return name, nil
}

func internalSecretBadRequest(message string) (internalApplicationExecution, error) {
	return jsonExecution(http.StatusBadRequest, map[string]any{"error": message})
}
