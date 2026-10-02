package mcp

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

type internalProjectContextHandler interface {
	ProjectContext(http.ResponseWriter, *http.Request)
	UpdateProjectContext(http.ResponseWriter, *http.Request)
	DeleteProjectContext(http.ResponseWriter, *http.Request)
}

type internalProjectContextExecutor interface {
	Execute(
		context.Context,
		int64,
		int64,
		internalProjectContextOperation,
		map[string]any,
	) (internalApplicationExecution, error)
}

type handlerInternalProjectContextExecutor struct {
	handler internalProjectContextHandler
}

func newHandlerInternalProjectContextExecutor(handler internalProjectContextHandler) internalProjectContextExecutor {
	if handler == nil {
		return nil
	}
	return &handlerInternalProjectContextExecutor{handler: handler}
}

func (executor *handlerInternalProjectContextExecutor) Execute(
	ctx context.Context,
	projectID int64,
	actorID int64,
	operation internalProjectContextOperation,
	arguments map[string]any,
) (internalApplicationExecution, error) {
	if executor == nil || executor.handler == nil {
		return internalApplicationExecution{}, errors.New("internal project context executor is unavailable")
	}
	if ctx == nil || projectID <= 0 || actorID <= 0 {
		return internalApplicationExecution{}, errors.New("internal project context request is invalid")
	}
	if err := ctx.Err(); err != nil {
		return internalApplicationExecution{}, err
	}

	actor := strconv.FormatInt(actorID, 10)
	ctx = auth.ContextWithUser(ctx, auth.User{ID: actor, UserID: actor})
	params := map[string]string{"projectID": strconv.FormatInt(projectID, 10)}

	switch operation {
	case internalGetProjectContext:
		return invokeInternalHandler(ctx, http.MethodGet, nil, nil, params, executor.handler.ProjectContext)
	case internalUpdateProjectContext:
		body := make(map[string]any, 3)
		for _, name := range []string{"content", "enabled", "activation_description"} {
			if value, present := arguments[name]; present {
				body[name] = value
			}
		}
		return invokeInternalHandler(ctx, http.MethodPut, nil, body, params, executor.handler.UpdateProjectContext)
	case internalDeleteProjectContext:
		return invokeInternalHandler(ctx, http.MethodDelete, nil, nil, params, executor.handler.DeleteProjectContext)
	default:
		return internalApplicationExecution{}, errors.New("unknown internal project context operation")
	}
}
