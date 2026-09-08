package configurations

import (
	"context"
	"errors"
	"net/http"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/go-chi/chi/v5"
)

const (
	CurrentConfigurationTypesPath           = "/api/v2/configurations/types/{projectID}"
	CurrentConfigurationTypesMode           = auth.PermissionModeDefault
	CurrentConfigurationTypesPermission     = CurrentConfigurationListPermission
	CurrentConfigurationTypesDefaultSection = "credentials"
)

var ErrInvalidCurrentConfigurationTypesRoute = errors.New("invalid current configuration types route dependencies")

type CurrentConfigurationTypesReader interface {
	List(context.Context, configurationapp.CurrentConfigurationTypesQuery) (configurationapp.CurrentConfigurationTypesResult, error)
}

var _ CurrentConfigurationTypesReader = (*configurationapp.CurrentConfigurationTypesService)(nil)

// CurrentConfigurationTypesRoute preserves the compact current UI endpoint.
// It deliberately applies the existing project-scoped configuration-list
// permission: listing stored type names is inventory access, not public schema
// discovery.
type CurrentConfigurationTypesRoute struct {
	handler http.Handler
}

func NewCurrentConfigurationTypesRoute(
	reader CurrentConfigurationTypesReader,
	authConfig apimw.AuthConfig,
	permissions auth.PermissionResolver,
) (*CurrentConfigurationTypesRoute, error) {
	if reader == nil || !authConfig.CredentialPlaneComposed() || permissions == nil {
		return nil, ErrInvalidCurrentConfigurationTypesRoute
	}

	handler := &currentConfigurationTypesHandler{reader: reader}
	route := http.Handler(http.HandlerFunc(handler.get))
	route = apimw.RequireResolvedPermissionsForProject(
		permissions,
		CurrentConfigurationTypesMode,
		currentConfigurationProjectID,
		CurrentConfigurationTypesPermission,
	)(route)
	route = apimw.Auth(authConfig)(route)

	router := chi.NewRouter()
	router.Method(http.MethodGet, CurrentConfigurationTypesPath, route)
	return &CurrentConfigurationTypesRoute{handler: router}, nil
}

func (route *CurrentConfigurationTypesRoute) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if route == nil || route.handler == nil {
		http.NotFound(writer, request)
		return
	}
	route.handler.ServeHTTP(writer, request)
}

type currentConfigurationTypesHandler struct {
	reader CurrentConfigurationTypesReader
}

func (handler *currentConfigurationTypesHandler) get(writer http.ResponseWriter, request *http.Request) {
	projectID, ok := currentConfigurationID(chi.URLParam(request, "projectID"))
	if !ok {
		writeCurrentConfigurationError(writer, http.StatusBadRequest, "invalid project")
		return
	}

	section := CurrentConfigurationTypesDefaultSection
	if values, present := request.URL.Query()["section"]; present {
		section = ""
		if len(values) > 0 {
			section = values[0]
		}
	}

	result, err := handler.reader.List(request.Context(), configurationapp.CurrentConfigurationTypesQuery{
		ProjectID: projectID,
		Section:   section,
	})
	if err != nil {
		if errors.Is(err, configurationapp.ErrInvalidCurrentConfigurationTypesRequest) {
			writeCurrentConfigurationError(writer, http.StatusBadRequest, "invalid configuration types request")
			return
		}
		writeCurrentConfigurationError(writer, http.StatusInternalServerError, "internal server error")
		return
	}

	writeJSON(writer, http.StatusOK, newCurrentConfigurationTypesDTO(result))
}

type currentConfigurationTypesDTO struct {
	Rows  []string `json:"rows"`
	Total int      `json:"total"`
}

func newCurrentConfigurationTypesDTO(
	result configurationapp.CurrentConfigurationTypesResult,
) currentConfigurationTypesDTO {
	rows := append([]string{}, result.Rows...)
	return currentConfigurationTypesDTO{Rows: rows, Total: result.Total}
}
