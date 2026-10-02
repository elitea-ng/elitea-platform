package mcp

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

const (
	internalNotificationMaxSearchBytes = 8223
	internalNotificationMaxSearchWords = 32
	internalNotificationMaxWordBytes   = 256
	internalNotificationMaxEventBytes  = 255
)

type internalNotificationHandler interface {
	List(http.ResponseWriter, *http.Request)
	Details(http.ResponseWriter, *http.Request)
	MarkSeen(http.ResponseWriter, *http.Request)
}

type internalNotificationExecutor interface {
	Execute(
		context.Context,
		int64,
		int64,
		internalNotificationOperation,
		map[string]any,
	) (internalApplicationExecution, error)
}

type handlerInternalNotificationExecutor struct {
	handler internalNotificationHandler
}

func newHandlerInternalNotificationExecutor(handler internalNotificationHandler) internalNotificationExecutor {
	if handler == nil {
		return nil
	}
	return &handlerInternalNotificationExecutor{handler: handler}
}

func (executor *handlerInternalNotificationExecutor) Execute(
	ctx context.Context,
	projectID int64,
	actorID int64,
	operation internalNotificationOperation,
	arguments map[string]any,
) (internalApplicationExecution, error) {
	if executor == nil || executor.handler == nil {
		return internalApplicationExecution{}, errors.New("internal notification executor is unavailable")
	}
	if ctx == nil || projectID <= 0 || actorID <= 0 {
		return internalApplicationExecution{}, errors.New("internal notification request is invalid")
	}
	if err := ctx.Err(); err != nil {
		return internalApplicationExecution{}, err
	}

	actor := strconv.FormatInt(actorID, 10)
	ctx = auth.ContextWithUser(ctx, auth.User{ID: actor, UserID: actor})
	params := map[string]string{"projectID": strconv.FormatInt(projectID, 10)}

	switch operation {
	case internalListNotifications:
		query, result := internalNotificationListQuery(arguments)
		if result != nil {
			return *result, nil
		}
		return invokeInternalHandler(ctx, http.MethodGet, query, nil, params, executor.handler.List)
	case internalGetNotification, internalMarkNotification:
		notificationID, err := requiredPositiveID(arguments, "notification_id")
		if err != nil {
			return internalNotificationBadRequest(err.Error())
		}
		params["notificationID"] = notificationID
		handler := executor.handler.Details
		method := http.MethodGet
		if operation == internalMarkNotification {
			handler = executor.handler.MarkSeen
			method = http.MethodPut
		}
		return invokeInternalHandler(ctx, method, nil, nil, params, handler)
	default:
		return internalApplicationExecution{}, errors.New("unknown internal notification operation")
	}
}

func internalNotificationListQuery(arguments map[string]any) (url.Values, *internalApplicationExecution) {
	query := url.Values{}
	for _, bounded := range []struct {
		name     string
		fallback int
		minimum  int
		maximum  int
	}{
		{name: "limit", fallback: 10, minimum: 1, maximum: 1000},
		{name: "offset", fallback: 0, minimum: 0, maximum: 1<<31 - 1},
	} {
		value, err := boundedIntegerArgument(
			arguments, bounded.name, bounded.fallback, bounded.minimum, bounded.maximum,
		)
		if err != nil {
			result, _ := internalNotificationBadRequest(err.Error())
			return nil, &result
		}
		query.Set(bounded.name, strconv.Itoa(value))
	}

	for _, name := range []string{"only_new", "only_total"} {
		value, present := arguments[name]
		if !present {
			continue
		}
		flag, ok := value.(bool)
		if !ok {
			result, _ := internalNotificationBadRequest(name + " must be a boolean")
			return nil, &result
		}
		query.Set(name, strconv.FormatBool(flag))
	}

	for name, allowed := range map[string]map[string]struct{}{
		"sort_by": {
			"id": {}, "uuid": {}, "is_seen": {}, "project_id": {}, "user_id": {},
			"meta": {}, "event_type": {}, "created_at": {}, "updated_at": {},
		},
		"sort_order": {"asc": {}, "desc": {}},
	} {
		value, present := arguments[name]
		if !present {
			continue
		}
		text, ok := value.(string)
		if !ok {
			result, _ := internalNotificationBadRequest(name + " is invalid")
			return nil, &result
		}
		if _, ok := allowed[text]; !ok {
			result, _ := internalNotificationBadRequest(name + " is invalid")
			return nil, &result
		}
		query.Set(name, text)
	}

	if raw, present := arguments["event_type"]; present {
		value, ok := raw.(string)
		if !ok || len(value) > internalNotificationMaxEventBytes {
			result, _ := internalNotificationBadRequest("event_type is outside its supported range")
			return nil, &result
		}
		query.Set("event_type", value)
	}
	if raw, present := arguments["search"]; present {
		value, ok := raw.(string)
		if !ok {
			result, _ := internalNotificationBadRequest("search is outside its supported range")
			return nil, &result
		}
		words := strings.Fields(value)
		if len(value) > internalNotificationMaxSearchBytes ||
			len(words) > internalNotificationMaxSearchWords {
			result, _ := internalNotificationBadRequest("search is outside its supported range")
			return nil, &result
		}
		for _, word := range words {
			if len(word) > internalNotificationMaxWordBytes {
				result, _ := internalNotificationBadRequest("search is outside its supported range")
				return nil, &result
			}
		}
		query.Set("search", value)
	}
	return query, nil
}

func internalNotificationBadRequest(message string) (internalApplicationExecution, error) {
	return jsonExecution(http.StatusBadRequest, map[string]any{"error": message})
}
