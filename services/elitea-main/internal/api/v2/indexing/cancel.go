package indexing

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	indexingapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexing"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/go-chi/chi/v5"
)

const (
	CurrentIndexCancelPath       = "/api/v2/elitea_core/index_cancel/prompt_lib/{projectID}/{toolkitID}/{indexName}/{taskID}"
	CurrentIndexCancelMode       = auth.PermissionModeDefault
	CurrentIndexCancelPermission = "models.applications.task.delete"
)

var ErrInvalidCurrentIndexCancelRoute = errors.New("invalid current index-cancel route dependencies")

type CurrentIndexCanceller interface {
	Cancel(context.Context, indexingapp.CurrentIndexCancelRequest) (bool, error)
}

var _ CurrentIndexCanceller = (*indexingapp.CurrentIndexCancellationService)(nil)

// CurrentIndexCancelRoute preserves the current UI method, path, permission,
// and empty 204 response. It accepts only Go execution IDs; the compatibility
// router keeps Arbiter UUIDs on their current owner during migration.
type CurrentIndexCancelRoute struct {
	handler http.Handler
}

func NewCurrentIndexCancelRoute(
	canceller CurrentIndexCanceller,
	authConfig apimw.AuthConfig,
	permissions auth.PermissionResolver,
) (*CurrentIndexCancelRoute, error) {
	if canceller == nil || !authConfig.CredentialPlaneComposed() || permissions == nil {
		return nil, ErrInvalidCurrentIndexCancelRoute
	}

	handler := &currentIndexCancelHandler{canceller: canceller}
	endpoint := http.Handler(http.HandlerFunc(handler.cancel))
	endpoint = apimw.RequireResolvedPermissionsForProject(
		permissions,
		CurrentIndexCancelMode,
		func(request *http.Request) (string, bool) {
			projectID, valid := positiveCurrentIndexMetaID(chi.URLParam(request, "projectID"))
			return strconv.FormatInt(projectID, 10), valid
		},
		CurrentIndexCancelPermission,
	)(endpoint)
	endpoint = apimw.Auth(authConfig)(endpoint)

	router := chi.NewRouter()
	router.Method(http.MethodDelete, CurrentIndexCancelPath, endpoint)
	return &CurrentIndexCancelRoute{handler: router}, nil
}

func (route *CurrentIndexCancelRoute) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if route == nil || route.handler == nil {
		http.NotFound(writer, request)
		return
	}
	route.handler.ServeHTTP(writer, request)
}

type currentIndexCancelHandler struct {
	canceller CurrentIndexCanceller
}

func (handler *currentIndexCancelHandler) cancel(writer http.ResponseWriter, request *http.Request) {
	projectID, projectOK := positiveCurrentIndexMetaID(chi.URLParam(request, "projectID"))
	toolkitID, toolkitOK := positiveCurrentIndexMetaID(chi.URLParam(request, "toolkitID"))
	principal, authenticated := auth.RuntimePrincipalFromContext(request.Context())
	if !authenticated {
		writeError(writer, http.StatusUnauthorized, "authentication required")
		return
	}
	if _, owningUser := principal.OwningUserID(); !owningUser {
		writeError(writer, http.StatusUnauthorized, "authentication required")
		return
	}

	cancelRequest := indexingapp.CurrentIndexCancelRequest{
		ProjectID:   projectID,
		ToolkitID:   toolkitID,
		IndexName:   chi.URLParam(request, "indexName"),
		ExecutionID: chi.URLParam(request, "taskID"),
	}
	if !projectOK || !toolkitOK || cancelRequest.Validate() != nil {
		writeError(writer, http.StatusBadRequest, "Invalid index cancellation request")
		return
	}

	// The transition flag is deliberately not exposed: the current endpoint is
	// idempotent and returns 204 when the exact active target did not transition.
	if _, err := handler.canceller.Cancel(request.Context(), cancelRequest); err != nil {
		writeCurrentIndexCancelError(writer, err)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

func writeCurrentIndexCancelError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, indexingapp.ErrInvalidCurrentIndexCancel):
		writeError(writer, http.StatusBadRequest, "Invalid index cancellation request")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		writeError(writer, http.StatusGatewayTimeout, "Index cancellation request timed out")
	default:
		writeError(writer, http.StatusBadGateway, "Error occurred while cancelling index")
	}
}
