package configurations

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
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

// ModelPriceReader resolves the gateway catalogue price of each named model for
// a caller, keyed by catalogue name. It applies the budget routes'
// can_see_amounts rule itself: a caller who may not see amounts for the project
// gets no prices, and a name it cannot price unambiguously is absent.
type ModelPriceReader interface {
	ModelPrices(ctx context.Context, projectID, userID int64, names []string) (map[string]configurationapp.ModelPrice, error)
}

// CurrentModelCatalogOption configures optional route dependencies.
type CurrentModelCatalogOption func(*currentModelCatalogHandler)

// WithModelPrices serves section=llm items with their gateway price to a
// caller who may see amounts. Without it (or with a nil reader) no item
// carries a price.
func WithModelPrices(prices ModelPriceReader) CurrentModelCatalogOption {
	return func(handler *currentModelCatalogHandler) { handler.prices = prices }
}

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
	options ...CurrentModelCatalogOption,
) (*CurrentModelCatalogRoute, error) {
	if reader == nil || publicProjectID <= 0 ||
		!authConfig.CredentialPlaneComposed() || permissions == nil {
		return nil, ErrInvalidCurrentModelCatalogRoute
	}

	handler := &currentModelCatalogHandler{
		reader:          reader,
		publicProjectID: publicProjectID,
	}
	for _, option := range options {
		if option != nil {
			option(handler)
		}
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
	prices          ModelPriceReader
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
	if modelSection == configurationapp.CurrentModelSectionLLM {
		h.applyPrices(r, projectID, response.Items)
	}
	writeJSON(w, http.StatusOK, response)
}

// applyPrices stamps the gateway price onto the llm items a caller who may see
// amounts is served. It never fails the catalogue: this is the model picker's
// read (#293), and a price is an annotation on it, so a failed price or
// visibility read is logged and the items are served unpriced.
func (h *currentModelCatalogHandler) applyPrices(
	r *http.Request, projectID int32, items []configurationapp.CurrentModelCatalogItem,
) {
	if h.prices == nil || len(items) == 0 {
		return
	}
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		return
	}
	userID, ok := user.OwningUserID()
	if !ok {
		return
	}
	names := make([]string, 0, len(items))
	for _, item := range items {
		names = append(names, item.Name)
	}
	prices, err := h.prices.ModelPrices(r.Context(), int64(projectID), userID, names)
	if err != nil {
		slog.WarnContext(r.Context(), "model catalog price read failed; serving unpriced",
			"project_id", projectID, "error", err)
		return
	}
	configurationapp.ApplyModelPrices(items, prices)
}
