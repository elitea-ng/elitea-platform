package social

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	socialapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/social"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/go-chi/chi/v5"
)

const (
	CurrentAuthorsPath        = "/api/v2/social/authors/{projectID}"
	CurrentAuthorsDefaultPath = "/api/v2/social/authors/default/{projectID}"
	CurrentAuthorsMode        = auth.PermissionModeDefault
	CurrentAuthorsPermission  = "models.social.authors.get"
)

var ErrInvalidCurrentAuthorsRoute = errors.New("invalid current authors route dependencies")

type CurrentAuthorsReader interface {
	ListCurrentProjectAuthors(context.Context, int32) ([]socialapp.CurrentAuthor, error)
}

// CurrentAuthorsRoute owns only the source-compatible author-list endpoints.
// Production composition remains explicit so this slice can be verified before
// it replaces the legacy route.
type CurrentAuthorsRoute struct {
	handler http.Handler
}

func NewCurrentAuthorsRoute(
	reader CurrentAuthorsReader,
	authConfig apimw.AuthConfig,
	permissions auth.PermissionResolver,
) (*CurrentAuthorsRoute, error) {
	if reader == nil || !authConfig.CredentialPlaneComposed() || permissions == nil {
		return nil, ErrInvalidCurrentAuthorsRoute
	}

	endpoint := http.Handler(http.HandlerFunc((&currentAuthorsHandler{reader: reader}).get))
	endpoint = apimw.RequireResolvedPermissionsForProject(
		permissions,
		CurrentAuthorsMode,
		currentAuthorsProjectID,
		CurrentAuthorsPermission,
	)(endpoint)
	endpoint = apimw.Auth(authConfig)(endpoint)

	router := chi.NewRouter()
	router.Method(http.MethodGet, CurrentAuthorsPath, endpoint)
	router.Method(http.MethodGet, CurrentAuthorsDefaultPath, endpoint)
	return &CurrentAuthorsRoute{handler: router}, nil
}

func (route *CurrentAuthorsRoute) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if route == nil || route.handler == nil {
		http.NotFound(writer, request)
		return
	}
	route.handler.ServeHTTP(writer, request)
}

type currentAuthorsHandler struct {
	reader CurrentAuthorsReader
}

type currentAuthorResponse struct {
	ID        int32   `json:"id"`
	Email     *string `json:"email"`
	Name      *string `json:"name"`
	LastLogin *string `json:"last_login"`
	Suspended bool    `json:"suspended"`
	Avatar    *string `json:"avatar"`
}

func (handler *currentAuthorsHandler) get(writer http.ResponseWriter, request *http.Request) {
	projectID, ok := currentAuthorsID(chi.URLParam(request, "projectID"))
	if !ok {
		writeCurrentAuthorsError(writer, http.StatusBadRequest)
		return
	}

	authors, err := handler.reader.ListCurrentProjectAuthors(request.Context(), projectID)
	if err != nil {
		writeCurrentAuthorsError(writer, http.StatusInternalServerError)
		return
	}

	response := make([]currentAuthorResponse, 0, len(authors))
	for _, author := range authors {
		var lastLogin *string
		if author.LastLogin != nil {
			value := author.LastLogin.UTC().Format(http.TimeFormat)
			lastLogin = &value
		}
		response = append(response, currentAuthorResponse{
			ID:        author.ID,
			Email:     author.Email,
			Name:      author.Name,
			LastLogin: lastLogin,
			Suspended: author.Suspended,
			Avatar:    author.Avatar,
		})
	}

	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(writer).Encode(response)
}

func currentAuthorsProjectID(request *http.Request) (string, bool) {
	projectID, ok := currentAuthorsID(chi.URLParam(request, "projectID"))
	if !ok {
		return "", false
	}
	return strconv.FormatInt(int64(projectID), 10), true
}

func currentAuthorsID(value string) (int32, bool) {
	if value == "" {
		return 0, false
	}
	for _, digit := range value {
		if digit < '0' || digit > '9' {
			return 0, false
		}
	}
	parsed, err := strconv.ParseInt(value, 10, 32)
	return int32(parsed), err == nil && parsed > 0 && strconv.FormatInt(parsed, 10) == value
}

func writeCurrentAuthorsError(writer http.ResponseWriter, status int) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(map[string]string{"error": "request failed"})
}
