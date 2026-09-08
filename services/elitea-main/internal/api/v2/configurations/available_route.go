package configurations

import (
	"errors"
	"net/http"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/go-chi/chi/v5"
)

const (
	CurrentAvailablePath        = "/api/v2/configurations/available"
	CurrentAvailableSlashPath   = "/api/v2/configurations/available/"
	CurrentAvailableProjectPath = "/api/v2/configurations/available/{projectID}"
)

var ErrInvalidCurrentAvailableRoute = errors.New("invalid current available-configuration route dependencies")

type CurrentAvailableCatalogReader interface {
	CompleteEntries(...string) ([]configurationapp.CurrentAvailableConfigurationType, error)
}

var _ CurrentAvailableCatalogReader = (*configurationapp.CurrentAvailableCatalog)(nil)

// CurrentAvailableRoute exposes the complete, pinned current registry to an
// authenticated user. The optional project path remains a compatibility alias;
// the catalog is global and contains schemas, never project credentials.
type CurrentAvailableRoute struct {
	handler http.Handler
}

func NewCurrentAvailableRoute(
	catalog CurrentAvailableCatalogReader,
	authConfig apimw.AuthConfig,
) (*CurrentAvailableRoute, error) {
	if catalog == nil || !authConfig.CredentialPlaneComposed() {
		return nil, ErrInvalidCurrentAvailableRoute
	}

	handler := http.Handler(http.HandlerFunc((&currentAvailableHandler{catalog: catalog}).get))
	handler = apimw.Auth(authConfig)(handler)

	router := chi.NewRouter()
	router.Method(http.MethodGet, CurrentAvailablePath, handler)
	router.Method(http.MethodGet, CurrentAvailableSlashPath, handler)
	router.Method(http.MethodGet, CurrentAvailableProjectPath, handler)
	return &CurrentAvailableRoute{handler: router}, nil
}

func (route *CurrentAvailableRoute) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if route == nil || route.handler == nil {
		http.NotFound(writer, request)
		return
	}
	route.handler.ServeHTTP(writer, request)
}

type currentAvailableHandler struct {
	catalog CurrentAvailableCatalogReader
}

func (handler *currentAvailableHandler) get(writer http.ResponseWriter, request *http.Request) {
	entries, err := handler.catalog.CompleteEntries(request.URL.Query()["section"]...)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, configurationapp.ErrCurrentAvailableCatalogPartial) {
			status = http.StatusServiceUnavailable
		}
		writeCurrentConfigurationError(writer, status, "configuration catalog is unavailable")
		return
	}
	writeJSON(writer, http.StatusOK, newCurrentAvailableConfigurationTypesDTO(entries))
}
