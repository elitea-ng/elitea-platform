package social

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/publicproject"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	CurrentFeedbackCreatePath       = "/api/v2/social/feedbacks/default/{projectID}"
	CurrentFeedbackCreateAliasPath  = "/api/v2/social/feedbacks/{projectID}"
	CurrentFeedbackCreateMode       = auth.PermissionModeDefault
	CurrentFeedbackCreatePermission = "models.social.feedbacks.create"
	CurrentFeedbackListPermission   = "models.social.feedbacks.list"
	// Paths below the /social mount; the alias is the legacy implicit-default form.
	CurrentFeedbackListSubPath      = "/feedbacks/default/{projectID}"
	CurrentFeedbackListAliasSubPath = "/feedbacks/{projectID}"
	MaxCurrentFeedbackBodyBytes     = 64 << 10
)

var ErrInvalidCurrentFeedbackCreateRoute = errors.New("invalid current feedback-create route dependencies")

// CurrentFeedbackCreator is the shared-table persistence boundary used by the
// current feedback endpoint. The route checks project membership before the
// request body is read, and the statement repeats the decision.
type CurrentFeedbackCreator interface {
	CreateCurrentFeedback(
		ctx context.Context,
		userID int64,
		projectID int64,
		description string,
		rating int,
		referrer *string,
		userAgent string,
	) (int64, error)
}

// CurrentFeedbackLister is the listing side of the same shared table.
type CurrentFeedbackLister interface {
	ListCurrentFeedback(
		ctx context.Context,
		callerID int64,
		projectID int64,
		ownOnly bool,
		page repos.FeedbackPage,
	) (repos.FeedbackList, error)
}

// CurrentFeedbackCreateRoute owns only the current feedback POST. Production
// mounts the same handler through Handler.Routes; this standalone form stays
// for the integration suites that exercise the POST without the Social group.
type CurrentFeedbackCreateRoute struct {
	handler http.Handler
}

func NewCurrentFeedbackCreateRoute(
	creator CurrentFeedbackCreator,
	authConfig apimw.AuthConfig,
	permissions auth.PermissionResolver,
) (*CurrentFeedbackCreateRoute, error) {
	if creator == nil || authConfig.PrincipalValidator == nil ||
		authConfig.ForwardedIdentityVerifier == nil || permissions == nil {
		return nil, ErrInvalidCurrentFeedbackCreateRoute
	}

	endpoint := http.Handler(http.HandlerFunc((&currentFeedbackHandler{creator: creator}).create))
	endpoint = apimw.RequireResolvedPermissionsForProject(
		permissions,
		CurrentFeedbackCreateMode,
		currentFeedbackProjectID,
		CurrentFeedbackCreatePermission,
	)(endpoint)
	endpoint = apimw.Auth(authConfig)(endpoint)

	router := chi.NewRouter()
	router.Method(http.MethodPost, CurrentFeedbackCreateAliasPath, endpoint)
	router.Method(http.MethodPost, CurrentFeedbackCreatePath, endpoint)
	return &CurrentFeedbackCreateRoute{handler: router}, nil
}

func (route *CurrentFeedbackCreateRoute) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if route == nil || route.handler == nil {
		http.NotFound(writer, request)
		return
	}
	route.handler.ServeHTTP(writer, request)
}

type currentFeedbackHandler struct {
	creator CurrentFeedbackCreator
	lister  CurrentFeedbackLister
}

type currentFeedbackCreateRequest struct {
	Description *string `json:"description"`
	Rating      *int    `json:"rating"`
}

type currentFeedbackCreateResponse struct {
	ID int64 `json:"id"`
}

func (handler *currentFeedbackHandler) create(writer http.ResponseWriter, request *http.Request) {
	principal, ok := auth.UserFromContext(request.Context())
	if !ok {
		writeCurrentFeedbackError(writer, http.StatusUnauthorized, "authentication required")
		return
	}
	userID, ok := principal.OwningUserID()
	if !ok {
		writeCurrentFeedbackError(writer, http.StatusUnauthorized, "authentication required")
		return
	}

	projectID, ok := currentFeedbackProjectNumber(request)
	if !ok {
		writeCurrentFeedbackError(writer, http.StatusBadRequest, "invalid request")
		return
	}

	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || (mediaType != "application/json" && !strings.HasSuffix(mediaType, "+json")) {
		writeCurrentFeedbackError(writer, http.StatusUnsupportedMediaType, "unsupported media type")
		return
	}

	request.Body = http.MaxBytesReader(writer, request.Body, MaxCurrentFeedbackBodyBytes)
	var body currentFeedbackCreateRequest
	decoder := json.NewDecoder(request.Body)
	if err := decoder.Decode(&body); err != nil {
		var sizeError *http.MaxBytesError
		if errors.As(err, &sizeError) {
			writeCurrentFeedbackError(writer, http.StatusRequestEntityTooLarge, "request body is too large")
			return
		}
		writeCurrentFeedbackError(writer, http.StatusBadRequest, "invalid request")
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		writeCurrentFeedbackError(writer, http.StatusBadRequest, "invalid request")
		return
	}
	if body.Description == nil || body.Rating == nil || *body.Rating < 0 || *body.Rating > 5 {
		writeCurrentFeedbackError(writer, http.StatusBadRequest, "invalid request")
		return
	}

	var referrer *string
	if value := request.Referer(); value != "" {
		referrer = &value
	}
	id, err := handler.creator.CreateCurrentFeedback(
		request.Context(),
		userID,
		projectID,
		*body.Description,
		*body.Rating,
		referrer,
		request.UserAgent(),
	)
	if errors.Is(err, repos.ErrSocialFeedbackForbidden) {
		writeCurrentFeedbackError(writer, http.StatusForbidden, "forbidden")
		return
	}
	if err != nil {
		writeCurrentFeedbackError(writer, http.StatusInternalServerError, "internal server error")
		return
	}

	writeJSON(writer, http.StatusCreated, currentFeedbackCreateResponse{ID: id})
}

func currentFeedbackProjectID(request *http.Request) (string, bool) {
	value := chi.URLParam(request, "projectID")
	projectID, err := strconv.ParseInt(value, 10, 64)
	return value, err == nil && projectID > 0 && strconv.FormatInt(projectID, 10) == value
}

func writeCurrentFeedbackError(writer http.ResponseWriter, status int, message string) {
	writeJSON(writer, status, map[string]string{"error": message})
}

func currentFeedbackProjectNumber(request *http.Request) (int64, bool) {
	value, ok := currentFeedbackProjectID(request)
	if !ok {
		return 0, false
	}
	projectID, err := strconv.ParseInt(value, 10, 64)
	return projectID, err == nil
}

const (
	defaultFeedbackListLimit = 50
	maxFeedbackListLimit     = repos.MaxFeedbackPageLimit
	maxFeedbackListOffset    = repos.MaxFeedbackPageOffset
)

// parseFeedbackPage reads limit, offset, sort_by and sort_order. Any other key,
// a repeated key or an out-of-range value is refused.
func parseFeedbackPage(query url.Values) (repos.FeedbackPage, bool) {
	page := repos.FeedbackPage{Limit: defaultFeedbackListLimit, SortBy: "id"}
	for key, values := range query {
		if len(values) != 1 {
			return page, false
		}
		value := values[0]
		switch key {
		case "limit":
			limit, err := strconv.Atoi(value)
			if err != nil || limit < 1 || limit > maxFeedbackListLimit {
				return page, false
			}
			page.Limit = limit
		case "offset":
			offset, err := strconv.Atoi(value)
			if err != nil || offset < 0 || offset > maxFeedbackListOffset {
				return page, false
			}
			page.Offset = offset
		case "sort_by":
			if value != "id" && value != "created_at" {
				return page, false
			}
			page.SortBy = value
		case "sort_order":
			if value != "asc" && value != "desc" {
				return page, false
			}
			page.Desc = value == "desc"
		default:
			return page, false
		}
	}
	return page, true
}

// list answers the feedback visible to the caller in the route's project. The
// public project shows only the caller's own rows.
func (handler *currentFeedbackHandler) list(writer http.ResponseWriter, request *http.Request) {
	principal, ok := auth.UserFromContext(request.Context())
	if !ok {
		writeCurrentFeedbackError(writer, http.StatusUnauthorized, "authentication required")
		return
	}
	userID, ok := principal.OwningUserID()
	if !ok {
		writeCurrentFeedbackError(writer, http.StatusUnauthorized, "authentication required")
		return
	}
	projectID, ok := currentFeedbackProjectNumber(request)
	if !ok {
		writeCurrentFeedbackError(writer, http.StatusBadRequest, "invalid request")
		return
	}
	// ParseQuery, not URL.Query: Query silently drops a malformed pair.
	query, err := url.ParseQuery(request.URL.RawQuery)
	if err != nil {
		writeCurrentFeedbackError(writer, http.StatusBadRequest, "invalid request")
		return
	}
	page, ok := parseFeedbackPage(query)
	if !ok {
		writeCurrentFeedbackError(writer, http.StatusBadRequest, "invalid request")
		return
	}

	list, err := handler.lister.ListCurrentFeedback(
		request.Context(), userID, projectID, projectID == int64(publicproject.ID()), page)
	if errors.Is(err, repos.ErrSocialFeedbackForbidden) {
		writeCurrentFeedbackError(writer, http.StatusForbidden, "forbidden")
		return
	}
	if err != nil {
		slog.ErrorContext(request.Context(), "social_feedbacks_list failed",
			"project_id", projectID, "error", err)
		writeCurrentFeedbackError(writer, http.StatusInternalServerError, "failed to list feedback")
		return
	}
	writeJSON(writer, http.StatusOK, list)
}

// feedbackHandlers builds the mounted list and create handlers over the pool.
// Without a pool both answer 503, never a panic or an empty success.
func feedbackHandlers(pool *pgxpool.Pool) (list, create http.Handler) {
	repository, err := repos.NewCurrentSocialFeedbacksRepository(pool)
	if err != nil {
		unavailable := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writeCurrentFeedbackError(writer, http.StatusServiceUnavailable, "feedback storage unavailable")
		})
		return unavailable, unavailable
	}
	handler := &currentFeedbackHandler{creator: repository, lister: repository}
	return http.HandlerFunc(handler.list), http.HandlerFunc(handler.create)
}

// NewFeedbackListHandler is the GET handler for the project's feedback, for
// mounts outside this package (the elitea_core twin). It applies no gate: the
// caller mounts it behind authentication, project access and the
// CurrentFeedbackListPermission check.
func NewFeedbackListHandler(pool *pgxpool.Pool) http.Handler {
	list, _ := feedbackHandlers(pool)
	return list
}

// requireFeedbackPermission gates a feedback route on one permission. With no
// resolver it fails closed: 401 without a principal, otherwise 503.
func requireFeedbackPermission(
	resolver auth.PermissionResolver,
	permission string,
) func(http.Handler) http.Handler {
	if resolver != nil {
		return apimw.RequireResolvedPermissionsForProject(
			resolver, CurrentFeedbackCreateMode, currentFeedbackProjectID, permission)
	}
	return func(http.Handler) http.Handler {
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if _, ok := auth.UserFromContext(request.Context()); !ok {
				writeCurrentFeedbackError(writer, http.StatusUnauthorized, "authentication required")
				return
			}
			writeCurrentFeedbackError(writer, http.StatusServiceUnavailable, "feedback authorization unavailable")
		})
	}
}
