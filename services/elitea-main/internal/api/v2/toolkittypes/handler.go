package toolkittypes

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/go-chi/chi/v5"
)

const (
	CurrentToolkitTypesPath       = "/api/v2/elitea_core/toolkit_types/prompt_lib/{projectID}"
	CurrentToolkitTypesMode       = auth.PermissionModeDefault
	CurrentToolkitTypesPermission = "models.applications.tools.list"
)

var ErrInvalidCurrentToolkitTypesRoute = errors.New("invalid current toolkit types route dependencies")

type CurrentToolkitTypesReader interface {
	ListCurrentToolkitTypes(
		context.Context,
		int32,
		bool,
		bool,
	) ([]string, error)
}

// CurrentToolkitTypesRoute preserves the project-scoped current endpoint while
// keeping it independent from the broad prototype toolkit handler.
type CurrentToolkitTypesRoute struct {
	handler http.Handler
}

func NewCurrentToolkitTypesRoute(
	reader CurrentToolkitTypesReader,
	authConfig apimw.AuthConfig,
	permissions auth.PermissionResolver,
) (*CurrentToolkitTypesRoute, error) {
	if reader == nil || !authConfig.CredentialPlaneComposed() || permissions == nil {
		return nil, ErrInvalidCurrentToolkitTypesRoute
	}

	handler := &currentToolkitTypesHandler{reader: reader}
	route := http.Handler(http.HandlerFunc(handler.get))
	route = apimw.RequireResolvedPermissionsForProject(
		permissions,
		CurrentToolkitTypesMode,
		currentToolkitTypesProjectID,
		CurrentToolkitTypesPermission,
	)(route)
	route = apimw.Auth(authConfig)(route)

	router := chi.NewRouter()
	router.Method(http.MethodGet, CurrentToolkitTypesPath, route)
	return &CurrentToolkitTypesRoute{handler: router}, nil
}

func (route *CurrentToolkitTypesRoute) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if route == nil || route.handler == nil {
		http.NotFound(writer, request)
		return
	}
	route.handler.ServeHTTP(writer, request)
}

type currentToolkitTypesHandler struct {
	reader CurrentToolkitTypesReader
}

func (handler *currentToolkitTypesHandler) get(writer http.ResponseWriter, request *http.Request) {
	projectID, ok := currentToolkitTypesID(chi.URLParam(request, "projectID"))
	if !ok {
		writeCurrentToolkitTypesFailure(writer)
		return
	}

	rows, err := handler.reader.ListCurrentToolkitTypes(
		request.Context(),
		projectID,
		strings.EqualFold(request.URL.Query().Get("mcp"), "true"),
		strings.EqualFold(request.URL.Query().Get("application"), "true"),
	)
	if err != nil {
		writeCurrentToolkitTypesFailure(writer)
		return
	}

	writeCurrentToolkitTypesJSON(writer, http.StatusOK, currentToolkitTypesResponse{
		Rows:  append([]string{}, rows...),
		Total: len(rows),
	})
}

type currentToolkitTypesResponse struct {
	Rows  []string `json:"rows"`
	Total int      `json:"total"`
}

type currentToolkitTypesFailure struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

func currentToolkitTypesProjectID(request *http.Request) (string, bool) {
	projectID, ok := currentToolkitTypesID(chi.URLParam(request, "projectID"))
	if !ok {
		return "", false
	}
	return strconv.FormatInt(int64(projectID), 10), true
}

func currentToolkitTypesID(value string) (int32, bool) {
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

func writeCurrentToolkitTypesFailure(writer http.ResponseWriter) {
	writeCurrentToolkitTypesJSON(writer, http.StatusBadRequest, currentToolkitTypesFailure{
		OK:    false,
		Error: "Failed to list toolkit types",
	})
}

func writeCurrentToolkitTypesJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
