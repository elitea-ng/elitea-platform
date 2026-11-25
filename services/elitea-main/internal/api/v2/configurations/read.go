package configurations

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"

	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
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
	tracing TracingRule,
) (*CurrentConfigurationReadRoute, error) {
	if reader == nil || publicProjectID <= 0 ||
		!authConfig.CredentialPlaneComposed() || permissions == nil || !tracing.composed() {
		return nil, ErrInvalidCurrentConfigurationReadRoute
	}

	handler := &currentConfigurationReadHandler{
		reader:          reader,
		publicProjectID: publicProjectID,
		tracing:         tracing,
	}

	// The same gate as the compatibility routes at these patterns, central
	// branch included (ADR-0059 R0.6b): where this route is composed it
	// shadows them, so it is what serves these paths (#3024).
	override := currentSettingsOverride(reader, permissions)

	list := http.Handler(http.HandlerFunc(handler.list))
	list = currentConfigurationGate(
		permissions, override, CurrentConfigurationListPath, CurrentConfigurationListPermission,
	)(list)
	list = apimw.Auth(authConfig)(list)

	details := http.Handler(http.HandlerFunc(handler.get))
	details = currentConfigurationGate(
		permissions, override, CurrentConfigurationDetailsPath, CurrentConfigurationGetPermission,
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
	// tracing hides a tracing row from a caller who does not manage tracing,
	// with the answer an absent row gets, as the compatibility read by id
	// does (#3032).
	tracing TracingRule
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
		// The viewer, so the project page can withhold another member's
		// UNSHARED rows (#922). An unauthenticated request never reaches this
		// handler — Auth runs in front of it — but a principal that carries no
		// numeric user id (a service token) reads the page unrestricted, which
		// is the behaviour every non-browser caller had before.
		ViewerID: currentConfigurationViewerID(r.Context()),
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
	if !currentConfigurationVisibleToViewer(configuration, currentConfigurationViewerID(r.Context())) {
		// The same answer an absent row gets. Withholding the row from the
		// list (#922) while serving it by id would leave the id itself as the
		// whole access control, and ids here are small consecutive integers.
		writeCurrentConfigurationServiceErrorContext(r.Context(), w, configurationapp.ErrCurrentConfigurationNotFound)
		return
	}
	if h.tracing.forbidden(r.Context(), int64(projectID), configuration.Type) {
		writeCurrentConfigurationServiceErrorContext(r.Context(), w, configurationapp.ErrCurrentConfigurationNotFound)
		return
	}
	writeJSON(w, http.StatusOK, newCurrentConfigurationDTO(configuration))
}

// currentConfigurationViewerID reads the authenticated user this list is for.
//
// It is the same principal read the mutation path uses to stamp author_id
// (mutation.go's currentConfigurationMutationAuthorID), which is the column
// the visibility predicate compares against — the two must agree or a user
// would be hidden from their own rows.
//
// A request the gate admitted through the central override (ADR-0059 R0.6b)
// reads every row of the guarded types it reached, as the compatibility route
// does: the operator manages the project's settings, and the same operator
// may update or delete any of those rows (the gate reads the stored type
// unfiltered), so withholding one from the read would hide what it can change.
func currentConfigurationViewerID(ctx context.Context) *int32 {
	if admittedCentrally(ctx) {
		return nil
	}
	principal, ok := auth.UserFromContext(ctx)
	if !ok {
		return nil
	}
	viewerID, ok := principal.OwningUserID()
	if !ok || viewerID <= 0 || viewerID > math.MaxInt32 {
		return nil
	}
	value := int32(viewerID)
	return &value
}

// currentConfigurationVisibleToViewer applies the LIST's visibility rule to
// one row, and is deliberately the same three clauses the SQL predicate
// spells (internal/db/queries/configurations.sql): shared, unauthored, or the
// viewer's own. A nil viewer is a caller with no user identity, which reads
// everything, as it did before #922.
func currentConfigurationVisibleToViewer(configuration configurationapp.CurrentConfiguration, viewerID *int32) bool {
	switch {
	case viewerID == nil, configuration.Shared, configuration.AuthorID == nil:
		return true
	default:
		return *configuration.AuthorID == *viewerID
	}
}

// currentConfigurationGate is the route gate of one reviewed Configurations
// route: the permission in the project, or the central branch of the guarded
// project settings for an allowlisted (route, configuration type) pair.
func currentConfigurationGate(
	permissions auth.PermissionResolver, override settingsOverrideGate, pattern, permission string,
) func(http.Handler) http.Handler {
	return apimw.RequireResolvedPermissionsForProjectOrCentral(
		permissions,
		CurrentConfigurationReadMode,
		currentConfigurationProjectID,
		override.fallback(pattern),
		permission,
	)
}

// currentSettingsOverride is the central branch of the reviewed routes. The
// stored type of a `{configID}` row is read through the reader, which reads it
// whoever wrote it (the visibility rule of #922 is the handler's, after the
// gate).
func currentSettingsOverride(reader CurrentConfigurationReader, permissions auth.PermissionResolver) settingsOverrideGate {
	return settingsOverrideGate{resolver: permissions, storedType: currentStoredType(reader)}
}

// currentStoredType answers the stored type of the request's `{configID}`
// row through reader, "" for an absent row or an id that is not one.
func currentStoredType(reader CurrentConfigurationReader) func(*http.Request) (string, error) {
	return func(r *http.Request) (string, error) {
		projectID, projectOK := currentConfigurationID(chi.URLParam(r, "projectID"))
		configurationID, configurationOK := currentConfigurationID(chi.URLParam(r, "configID"))
		if reader == nil || !projectOK || !configurationOK {
			return "", nil
		}
		configuration, err := reader.Get(r.Context(), projectID, configurationID)
		switch {
		case errors.Is(err, configurationapp.ErrCurrentConfigurationNotFound),
			errors.Is(err, configurationapp.ErrInvalidCurrentConfigurationRequest):
			return "", nil
		case err != nil:
			return "", err
		}
		return configuration.Type, nil
	}
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
