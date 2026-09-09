package configurations

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/go-chi/chi/v5"
)

const (
	CurrentConfigurationListPath       = "/api/v2/configurations/configurations/{projectID}"
	CurrentConfigurationDetailsPath    = "/api/v2/configurations/configuration/{projectID}/{configID}"
	CurrentConfigurationReadMode       = auth.PermissionModeDefault
	CurrentConfigurationListPermission = "configurations.configurations.list"
	CurrentConfigurationGetPermission  = "configurations.configuration.details"
)

var ErrInvalidCurrentConfigurationReadRoute = errors.New("invalid current configuration read route dependencies")

// CurrentConfigurationReader is the read surface of CurrentCRUDService used by
// the current Configurations HTTP contract. Mutation orchestration is excluded:
// the current POST, PUT, and DELETE paths also own registry validation, secret
// lifecycle, side effects, and events, not only tenant-row persistence.
type CurrentConfigurationReader interface {
	List(context.Context, configurationapp.CurrentConfigurationListRequest) (configurationapp.CurrentConfigurationListResult, error)
	Get(context.Context, int32, int32) (configurationapp.CurrentConfiguration, error)
}

var _ CurrentConfigurationReader = (*configurationapp.CurrentCRUDService)(nil)
var _ CurrentConfigurationReader = (*configurationapp.CurrentConfigurationReadService)(nil)

// CurrentConfigurationReadRoute owns the two exact current read endpoints when
// the standalone Configurations graph is enabled. Construction binds trusted
// authentication and project-scoped RBAC before either service method can run.
type CurrentConfigurationReadRoute struct {
	handler http.Handler
}

func NewCurrentConfigurationReadRoute(
	reader CurrentConfigurationReader,
	publicProjectID int32,
	authConfig apimw.AuthConfig,
	permissions auth.PermissionResolver,
) (*CurrentConfigurationReadRoute, error) {
	if reader == nil || publicProjectID <= 0 ||
		!authConfig.CredentialPlaneComposed() || permissions == nil {
		return nil, ErrInvalidCurrentConfigurationReadRoute
	}

	handler := &currentConfigurationReadHandler{
		reader:          reader,
		publicProjectID: publicProjectID,
	}

	list := http.Handler(http.HandlerFunc(handler.list))
	list = apimw.RequireResolvedPermissionsForProject(
		permissions,
		CurrentConfigurationReadMode,
		currentConfigurationProjectID,
		CurrentConfigurationListPermission,
	)(list)
	list = apimw.Auth(authConfig)(list)

	details := http.Handler(http.HandlerFunc(handler.get))
	details = apimw.RequireResolvedPermissionsForProject(
		permissions,
		CurrentConfigurationReadMode,
		currentConfigurationProjectID,
		CurrentConfigurationGetPermission,
	)(details)
	details = apimw.Auth(authConfig)(details)

	router := chi.NewRouter()
	router.Method(http.MethodGet, CurrentConfigurationListPath, list)
	router.Method(http.MethodGet, CurrentConfigurationDetailsPath, details)
	return &CurrentConfigurationReadRoute{handler: router}, nil
}

func (route *CurrentConfigurationReadRoute) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if route == nil || route.handler == nil {
		http.NotFound(w, r)
		return
	}
	route.handler.ServeHTTP(w, r)
}

type currentConfigurationReadHandler struct {
	reader          CurrentConfigurationReader
	publicProjectID int32
}

func (h *currentConfigurationReadHandler) list(w http.ResponseWriter, r *http.Request) {
	projectID, ok := currentConfigurationID(chi.URLParam(r, "projectID"))
	if !ok {
		writeCurrentConfigurationError(w, http.StatusBadRequest, "invalid project")
		return
	}

	query := r.URL.Query()
	ids, idsErr := configurationQueryIDs(query)
	if idsErr != nil {
		writeCurrentConfigurationServiceErrorContext(r.Context(), w, idsErr)
		return
	}
	result, err := h.reader.List(r.Context(), configurationapp.CurrentConfigurationListRequest{
		ProjectID:       projectID,
		PublicProjectID: h.publicProjectID,
		IDs:             ids,
		Types:           append([]string(nil), query["type"]...),
		Sections:        append([]string(nil), query["section"]...),
		Offset:          currentConfigurationQueryInteger(query.Get("offset")),
		Limit:           currentConfigurationQueryInteger(query.Get("limit")),
		IncludeShared:   strings.EqualFold(query.Get("include_shared"), "true"),
		SharedOffset:    currentConfigurationQueryInteger(query.Get("shared_offset")),
		SharedLimit:     currentConfigurationQueryInteger(query.Get("shared_limit")),
		Query:           query.Get("query"),
		SortBy:          query.Get("sort_by"),
		SortOrder:       query.Get("sort_order"),
	})
	if err != nil {
		writeCurrentConfigurationServiceErrorContext(r.Context(), w, err)
		return
	}
	writeJSON(w, http.StatusOK, newCurrentConfigurationListDTO(result))
}

func (h *currentConfigurationReadHandler) get(w http.ResponseWriter, r *http.Request) {
	projectID, projectOK := currentConfigurationID(chi.URLParam(r, "projectID"))
	configurationID, configurationOK := currentConfigurationID(chi.URLParam(r, "configID"))
	if !projectOK || !configurationOK {
		writeCurrentConfigurationError(w, http.StatusBadRequest, "invalid configuration")
		return
	}

	configuration, err := h.reader.Get(r.Context(), projectID, configurationID)
	if err != nil {
		writeCurrentConfigurationServiceErrorContext(r.Context(), w, err)
		return
	}
	writeJSON(w, http.StatusOK, newCurrentConfigurationDTO(configuration))
}

func currentConfigurationProjectID(r *http.Request) (string, bool) {
	projectID, ok := currentConfigurationID(chi.URLParam(r, "projectID"))
	if !ok {
		return "", false
	}
	return strconv.FormatInt(int64(projectID), 10), true
}

func currentConfigurationID(value string) (int32, bool) {
	if value == "" {
		return 0, false
	}
	for _, digit := range value {
		if digit < '0' || digit > '9' {
			return 0, false
		}
	}
	parsed, err := strconv.ParseInt(value, 10, 32)
	return int32(parsed), err == nil && parsed > 0
}

// Flask's request.args.get(..., type=int) falls back to its default when the
// query value cannot be converted. Zero is the service's default sentinel for
// both pagination offsets and limits.
func currentConfigurationQueryInteger(value string) int {
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0
	}
	return parsed
}

// The 500 branch LOGS. A configuration read that fails answers a generic
// message by design — the caller must not learn why — but answering it while
// also discarding the cause left `GET /configurations/configurations/{project}`
// failing for every project with nothing anywhere to say what it objected to
// (#293). The client-fault branches stay quiet: they are already fully
// described by the status they return.
func writeCurrentConfigurationServiceErrorContext(ctx context.Context, w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, configurationapp.ErrInvalidCurrentConfigurationRequest):
		writeCurrentConfigurationError(w, http.StatusBadRequest, "invalid configuration request")
	case errors.Is(err, configurationapp.ErrCurrentConfigurationNotFound):
		writeCurrentConfigurationError(w, http.StatusNotFound, "Configuration not found")
	default:
		slog.ErrorContext(ctx, "current configuration read failed", "error", err)
		writeCurrentConfigurationError(w, http.StatusInternalServerError, "internal server error")
	}
}

func writeCurrentConfigurationError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
