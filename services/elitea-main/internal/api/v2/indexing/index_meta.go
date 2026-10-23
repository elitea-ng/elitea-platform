package indexing

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	indexmetaapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/indexmeta"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/go-chi/chi/v5"
)

const (
	CurrentIndexMetaListPath       = "/api/v2/elitea_core/index_meta/prompt_lib/{projectID}/{toolkitID}"
	CurrentIndexMetaListMode       = auth.PermissionModeDefault
	CurrentIndexMetaListPermission = "models.applications.index_meta.details"
)

var ErrInvalidCurrentIndexMetaRoute = errors.New("invalid current index-meta route dependencies")

// CurrentIndexMetaReader is the provider-neutral application boundary for the
// current index list. The implementation (the index registry) resolves the
// saved toolkit; HTTP input cannot select a DSN or a PostgreSQL schema.
type CurrentIndexMetaReader interface {
	List(context.Context, indexmetaapp.Request) ([]indexmetaapp.Item, error)
}

// CurrentIndexMetaRoute preserves the current UI path and raw-array response,
// with trusted authentication and project RBAC applied before storage lookup.
type CurrentIndexMetaRoute struct {
	handler http.Handler
}

func NewCurrentIndexMetaRoute(
	reader CurrentIndexMetaReader,
	authConfig apimw.AuthConfig,
	permissions auth.PermissionResolver,
) (*CurrentIndexMetaRoute, error) {
	if reader == nil || !authConfig.CredentialPlaneComposed() || permissions == nil {
		return nil, ErrInvalidCurrentIndexMetaRoute
	}

	handler := &currentIndexMetaHandler{reader: reader}
	endpoint := http.Handler(http.HandlerFunc(handler.list))
	endpoint = apimw.RequireResolvedPermissionsForProject(
		permissions,
		CurrentIndexMetaListMode,
		func(request *http.Request) (string, bool) {
			projectID, valid := positiveCurrentIndexMetaID(chi.URLParam(request, "projectID"))
			return strconv.FormatInt(projectID, 10), valid
		},
		CurrentIndexMetaListPermission,
	)(endpoint)
	endpoint = apimw.Auth(authConfig)(endpoint)

	router := chi.NewRouter()
	router.Method(http.MethodGet, CurrentIndexMetaListPath, endpoint)
	return &CurrentIndexMetaRoute{handler: router}, nil
}

func (route *CurrentIndexMetaRoute) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if route == nil || route.handler == nil {
		http.NotFound(writer, request)
		return
	}
	route.handler.ServeHTTP(writer, request)
}

type currentIndexMetaHandler struct {
	reader CurrentIndexMetaReader
}

func (handler *currentIndexMetaHandler) list(writer http.ResponseWriter, request *http.Request) {
	projectID, projectOK := positiveCurrentIndexMetaID(chi.URLParam(request, "projectID"))
	toolkitID, toolkitOK := positiveCurrentIndexMetaID(chi.URLParam(request, "toolkitID"))
	if !projectOK || !toolkitOK {
		writeError(writer, http.StatusBadRequest, "Invalid index metadata request")
		return
	}

	principal, ok := auth.RuntimePrincipalFromContext(request.Context())
	if !ok {
		writeError(writer, http.StatusUnauthorized, "authentication required")
		return
	}
	actorUserID, ok := principal.OwningUserID()
	if !ok {
		writeError(writer, http.StatusUnauthorized, "authentication required")
		return
	}

	items, err := handler.reader.List(request.Context(), indexmetaapp.Request{
		ProjectID:   projectID,
		ActorUserID: actorUserID,
		ToolkitID:   toolkitID,
	})
	if err != nil {
		writeCurrentIndexMetaError(writer, err, toolkitID)
		return
	}
	writeJSON(writer, http.StatusOK, items)
}

// Flask's integer path converter accepts decimal values with leading zeroes.
// Preserve that current behavior while passing a canonical ID to RBAC and the
// application service.
func positiveCurrentIndexMetaID(value string) (int64, bool) {
	if value == "" {
		return 0, false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return 0, false
		}
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	return parsed, err == nil && parsed > 0
}

func writeCurrentIndexMetaError(writer http.ResponseWriter, err error, toolkitID int64) {
	switch {
	case errors.Is(err, indexmetaapp.ErrInvalidCurrentIndexMetaRequest):
		writeError(writer, http.StatusBadRequest, "Invalid index metadata request")
	case errors.Is(err, indexmetaapp.ErrCurrentIndexMetaToolkitMissing):
		// The current Python path turns a missing row into a toolkit-id
		// validation error rather than exposing tenant lookup details.
		writeError(writer, http.StatusBadRequest, "Toolkit id is missing for toolkit "+strconv.FormatInt(toolkitID, 10))
	case errors.Is(err, indexmetaapp.ErrCurrentIndexMetaTargetMissing):
		writeError(writer, http.StatusBadRequest, "Index storage is not available for toolkit "+strconv.FormatInt(toolkitID, 10))
	case errors.Is(err, indexmetaapp.ErrCurrentIndexMetaLimitExceeded):
		writeError(writer, http.StatusRequestEntityTooLarge, "Index metadata exceeds the approved response limit")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		writeError(writer, http.StatusGatewayTimeout, "Index metadata request timed out")
	default:
		writeError(writer, http.StatusBadGateway, "Error occurred while fetching index_meta")
	}
}
