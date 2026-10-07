package agentexecution

import (
	"context"
	"errors"
	"net/http"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/go-chi/chi/v5"
)

const (
	CurrentAgentCancelPath       = "/api/v2/elitea_core/task/prompt_lib/{projectID}/{responseMessageID}"
	CurrentAgentCancelMode       = auth.PermissionModeDefault
	CurrentAgentCancelPermission = "models.chat.task.delete"
)

var ErrInvalidCurrentAgentCancelRoute = errors.New("invalid current agent-cancel route dependencies")

type CurrentAgentCanceller interface {
	Cancel(
		context.Context,
		agentexecutionapp.CurrentAgentCancelRequest,
	) (agentexecutionapp.CurrentAgentCancelOutcome, error)
}

var _ CurrentAgentCanceller = (*agentexecutionapp.CurrentAgentCancellationService)(nil)

// CurrentAgentCancelRoute preserves the current DELETE contract while binding
// the stop to the exact Go-owned response and durable execution server-side.
type CurrentAgentCancelRoute struct {
	handler http.Handler
}

func NewCurrentAgentCancelRoute(
	canceller CurrentAgentCanceller,
	authConfig apimw.AuthConfig,
	permissions auth.PermissionResolver,
) (*CurrentAgentCancelRoute, error) {
	if canceller == nil || authConfig.PrincipalValidator == nil ||
		authConfig.ForwardedIdentityVerifier == nil || permissions == nil {
		return nil, ErrInvalidCurrentAgentCancelRoute
	}
	handler := &currentAgentCancelHandler{canceller: canceller}
	endpoint := http.Handler(http.HandlerFunc(handler.cancel))
	endpoint = apimw.RequireResolvedPermissionsForProject(
		permissions,
		CurrentAgentCancelMode,
		func(request *http.Request) (string, bool) {
			projectID := chi.URLParam(request, "projectID")
			_, valid := positiveCanonicalID(projectID)
			return projectID, valid
		},
		CurrentAgentCancelPermission,
	)(endpoint)
	endpoint = apimw.Auth(authConfig)(endpoint)

	router := chi.NewRouter()
	router.Method(http.MethodDelete, CurrentAgentCancelPath, endpoint)
	return &CurrentAgentCancelRoute{handler: router}, nil
}

func (route *CurrentAgentCancelRoute) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if route == nil || route.handler == nil {
		http.NotFound(writer, request)
		return
	}
	route.handler.ServeHTTP(writer, request)
}

type currentAgentCancelHandler struct {
	canceller CurrentAgentCanceller
}

func (handler *currentAgentCancelHandler) cancel(writer http.ResponseWriter, request *http.Request) {
	projectID, validProject := positiveCanonicalID(chi.URLParam(request, "projectID"))
	user, authenticated := auth.UserFromContext(request.Context())
	if !authenticated {
		writeError(writer, http.StatusUnauthorized, "authentication required")
		return
	}
	actorUserID, owningUser := user.OwningUserID()
	if !owningUser {
		writeError(writer, http.StatusUnauthorized, "authentication required")
		return
	}
	responseMessageID := agentexecutionapp.CanonicalCurrentAgentResponseMessageID(
		chi.URLParam(request, "responseMessageID"),
	)
	requestModel := agentexecutionapp.CurrentAgentCancelRequest{
		ProjectID:         projectID,
		ActorUserID:       actorUserID,
		ResponseMessageID: responseMessageID,
	}
	if !validProject || requestModel.Validate() != nil {
		writeError(writer, http.StatusBadRequest, "Invalid agent cancellation request")
		return
	}
	if _, err := handler.canceller.Cancel(request.Context(), requestModel); err != nil {
		writeCurrentAgentCancelError(writer, err)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

func writeCurrentAgentCancelError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, agentexecutionapp.ErrInvalidCurrentAgentCancel):
		writeError(writer, http.StatusBadRequest, "Invalid agent cancellation request")
	case errors.Is(err, agentexecutionapp.ErrCurrentAgentCancelNotAllowed):
		// The repository deliberately does not disclose whether a response is
		// absent, already terminal, stale, or owned by another actor. Do not turn
		// that safe ambiguity into a false claim that another user owns the run.
		writeError(writer, http.StatusConflict, "Message is not active or cannot be stopped by this user")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		writeError(writer, http.StatusGatewayTimeout, "Agent cancellation request timed out")
	default:
		writeError(writer, http.StatusBadGateway, "Error occurred while stopping task")
	}
}
