package indexing

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	indexscheduleapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/indexschedule"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/go-chi/chi/v5"
)

const currentIndexScheduleDeleteTimeout = 5 * time.Second

type CurrentIndexScheduleDeleter interface {
	Delete(context.Context, indexscheduleapp.DeleteRequest) (indexscheduleapp.DeleteResult, error)
}

type CurrentIndexScheduleDeleteRoute struct {
	handler http.Handler
}

func NewCurrentIndexScheduleDeleteRoute(
	deleter CurrentIndexScheduleDeleter,
	authConfig apimw.AuthConfig,
	permissions auth.PermissionResolver,
) (*CurrentIndexScheduleDeleteRoute, error) {
	if deleter == nil || !authConfig.CredentialPlaneComposed() || permissions == nil {
		return nil, ErrInvalidCurrentIndexScheduleRoute
	}
	handler := &currentIndexScheduleDeleteHandler{deleter: deleter}
	endpoint := http.Handler(http.HandlerFunc(handler.delete))
	endpoint = apimw.RequireResolvedPermissionsForProject(
		permissions,
		SourceOnlyIndexScheduleDeleteMode,
		func(request *http.Request) (string, bool) {
			projectID, valid := positiveCurrentIndexMetaID(
				chi.URLParam(request, "projectID"),
			)
			return strconv.FormatInt(projectID, 10), valid
		},
		SourceOnlyIndexScheduleDeletePermission,
	)(endpoint)
	endpoint = apimw.Auth(authConfig)(endpoint)

	router := chi.NewRouter()
	router.Method(
		SourceOnlyIndexScheduleDeleteMethod,
		SourceOnlyIndexScheduleDeletePath,
		endpoint,
	)
	return &CurrentIndexScheduleDeleteRoute{handler: router}, nil
}

func (route *CurrentIndexScheduleDeleteRoute) ServeHTTP(
	writer http.ResponseWriter,
	request *http.Request,
) {
	if route == nil || route.handler == nil {
		http.NotFound(writer, request)
		return
	}
	route.handler.ServeHTTP(writer, request)
}

type currentIndexScheduleDeleteHandler struct {
	deleter CurrentIndexScheduleDeleter
}

func (handler *currentIndexScheduleDeleteHandler) delete(
	writer http.ResponseWriter,
	request *http.Request,
) {
	projectID, projectOK := positiveCurrentIndexMetaID(
		chi.URLParam(request, "projectID"),
	)
	toolkitID, toolkitOK := positiveCurrentIndexMetaID(
		chi.URLParam(request, "toolkitID"),
	)
	indexMetaID := chi.URLParam(request, "indexMetaID")
	principal, authenticated := auth.RuntimePrincipalFromContext(request.Context())
	if !authenticated {
		writeCurrentIndexScheduleError(
			writer,
			http.StatusUnauthorized,
			"authentication required",
		)
		return
	}
	actorUserID, owningUser := principal.OwningUserID()
	if !projectOK || !toolkitOK || !owningUser ||
		indexMetaID == "" ||
		len(indexMetaID) > indexscheduleapp.MaxIndexMetaIDBytes {
		writeCurrentIndexScheduleError(
			writer,
			http.StatusBadRequest,
			"Error occurred while deleting index schedule",
		)
		return
	}
	var targetUserID *string
	if values, present := request.URL.Query()["user_id"]; present {
		value := ""
		if len(values) != 0 {
			value = values[0]
		}
		targetUserID = &value
	}
	ctx, cancel := context.WithTimeout(
		request.Context(),
		currentIndexScheduleDeleteTimeout,
	)
	defer cancel()
	result, err := handler.deleter.Delete(ctx, indexscheduleapp.DeleteRequest{
		ProjectID:    projectID,
		ActorUserID:  actorUserID,
		ToolkitID:    toolkitID,
		IndexMetaID:  indexMetaID,
		TargetUserID: targetUserID,
	})
	if err != nil {
		switch {
		case errors.Is(err, indexscheduleapp.ErrToolkitNotFound):
			writeCurrentIndexScheduleError(
				writer,
				http.StatusNotFound,
				"Toolkit not found",
			)
		case errors.Is(err, indexscheduleapp.ErrScheduleIndexNotFound):
			writeCurrentIndexScheduleError(
				writer,
				http.StatusNotFound,
				fmt.Sprintf("No schedule found for index '%s'", indexMetaID),
			)
		case errors.Is(err, indexscheduleapp.ErrScheduleUserNotFound):
			target := ""
			if targetUserID == nil {
				target = strconv.FormatInt(actorUserID, 10)
			} else {
				target = *targetUserID
			}
			writeCurrentIndexScheduleError(
				writer,
				http.StatusNotFound,
				fmt.Sprintf("No schedule found for user '%s'", target),
			)
		default:
			writeCurrentIndexScheduleError(
				writer,
				http.StatusBadRequest,
				"Error occurred while deleting index schedule",
			)
		}
		return
	}
	writeJSON(writer, http.StatusOK, result.IndexesMeta)
}

var _ CurrentIndexScheduleDeleter = (*indexscheduleapp.DeleteService)(nil)
