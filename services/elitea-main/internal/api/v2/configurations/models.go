package configurations

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/go-chi/chi/v5"
)

const (
	CurrentModelCatalogPath       = "/api/v2/configurations/models/{projectID}"
	CurrentModelCatalogMode       = auth.PermissionModeDefault
	CurrentModelCatalogPermission = CurrentConfigurationListPermission
)

var ErrInvalidCurrentModelCatalogRoute = errors.New("invalid current model catalog route dependencies")

// CurrentModelCatalogReader is the provider-neutral Configurations boundary
// used by both the UI catalog and the LiteLLM routing layer. Provider
// credentials never cross this interface.
type CurrentModelCatalogReader interface {
	Get(context.Context, configurationapp.CurrentModelCatalogQuery) (configurationapp.CurrentModelCatalogResponse, error)
}

var _ CurrentModelCatalogReader = (*configurationapp.CurrentModelCatalogService)(nil)

// CurrentModelCatalogRoute preserves the current GET wire contract while
// correcting the current endpoint's missing explicit authorization decorator.
// configurations.configurations.list is already granted to the normal
// viewer/editor/admin roles and binds the read to project membership.
type CurrentModelCatalogRoute struct {
	handler http.Handler
}

func NewCurrentModelCatalogRoute(
	reader CurrentModelCatalogReader,
	publicProjectID int32,
	authConfig apimw.AuthConfig,
	permissions auth.PermissionResolver,
) (*CurrentModelCatalogRoute, error) {
	if reader == nil || publicProjectID <= 0 ||
		!authConfig.CredentialPlaneComposed() || permissions == nil {
		return nil, ErrInvalidCurrentModelCatalogRoute
	}

	handler := &currentModelCatalogHandler{
		reader:          reader,
		publicProjectID: publicProjectID,
	}
	route := http.Handler(http.HandlerFunc(handler.get))
	route = apimw.RequireResolvedPermissionsForProject(
		permissions,
		CurrentModelCatalogMode,
		currentConfigurationProjectID,
		CurrentModelCatalogPermission,
	)(route)
	route = apimw.Auth(authConfig)(route)

	router := chi.NewRouter()
	router.Method(http.MethodGet, CurrentModelCatalogPath, route)
	return &CurrentModelCatalogRoute{handler: router}, nil
}

func (route *CurrentModelCatalogRoute) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if route == nil || route.handler == nil {
		http.NotFound(w, r)
		return
	}
	route.handler.ServeHTTP(w, r)
}

type currentModelCatalogHandler struct {
	reader          CurrentModelCatalogReader
	publicProjectID int32
}

func (h *currentModelCatalogHandler) get(w http.ResponseWriter, r *http.Request) {
	projectID, ok := currentConfigurationID(chi.URLParam(r, "projectID"))
	if !ok {
		writeCurrentConfigurationError(w, http.StatusBadRequest, "invalid project")
		return
	}

	section := strings.ToLower(r.URL.Query().Get("section"))
	if section == "" {
		section = string(configurationapp.CurrentModelSectionLLM)
	}
	modelSection := configurationapp.CurrentModelSection(section)
	if !configurationapp.IsSupportedCurrentModelSection(modelSection) {
		// Current ModelConfigurationService treats an unknown section as an
		// empty catalog rather than a request error.
		writeJSON(w, http.StatusOK, configurationapp.BuildCurrentModelCatalog(
			configurationapp.CurrentModelCatalogRequest{Section: modelSection},
		))
		return
	}
	response, err := h.reader.Get(r.Context(), configurationapp.CurrentModelCatalogQuery{
		Section:         modelSection,
		ProjectID:       projectID,
		PublicProjectID: h.publicProjectID,
		IncludeShared:   strings.EqualFold(r.URL.Query().Get("include_shared"), "true"),
	})
	if err != nil {
		switch {
		case errors.Is(err, configurationapp.ErrInvalidCurrentModelCatalogRequest):
			writeCurrentConfigurationError(w, http.StatusBadRequest, "invalid model catalog request")
		default:
			// Logged for the same reason as the configuration read (#293): the
			// generic body is deliberate, discarding the cause is not. This
			// route is what the model picker asks for its catalogue, so a
			// silent 500 here presents as "the product has no models".
			slog.ErrorContext(r.Context(), "current model catalog read failed", "error", err)
			writeCurrentConfigurationError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}
	writeJSON(w, http.StatusOK, response)
}
