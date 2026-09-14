package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	notificationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/notifications"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/go-chi/chi/v5"
)

const (
	CurrentNotificationsPath = "/api/v2/notifications/notifications/prompt_lib/{projectID}"
	CurrentNotificationPath  = "/api/v2/notifications/notification/prompt_lib/{projectID}/{notificationID}"

	CurrentNotificationsMode             = auth.PermissionModeDefault
	CurrentNotificationsListPermission   = "models.notifications.notifications.list"
	CurrentNotificationDetailsPermission = "models.notifications.notification.details"
	CurrentNotificationUpdatePermission  = "models.notifications.notification.update"
	CurrentNotificationDeletePermission  = "models.notifications.notification.delete"

	maxCurrentNotificationRequestBytes = int64(64 * 1024)
	maxCurrentNotificationListLimit    = int64(1000)
)

var ErrInvalidCurrentNotificationAPIRoute = errors.New("invalid current notification API route dependencies")

// CurrentNotificationAPIRoute preserves the generic current notification API
// over the existing centry.notifications table. Every store call is scoped to
// the authenticated user; projectID remains the current RBAC context.
type CurrentNotificationAPIRoute struct {
	handler http.Handler
}

func NewCurrentNotificationAPIRoute(
	store notificationapp.Store,
	authConfig apimw.AuthConfig,
	permissions auth.PermissionResolver,
) (*CurrentNotificationAPIRoute, error) {
	// ForwardedIdentityVerifier is optional. Only a FormGraph supplies one, and
	// a FormGraph exists only when ELITEA_AUTH_CONFIG_FILE is set. A required
	// verifier therefore made this route composable under Form authentication
	// alone, so GET /api/v2/notifications/notifications/prompt_lib/{projectID}
	// answered 404 on every OIDC-only deployment, the E2E stack included
	// (#413). The screen showed "No notifications yet" and hid the 404. This is
	// the same relaxation, and the same reason, as
	// NewCurrentNotificationEventsRoute above.
	//
	// PrincipalValidator stays mandatory. apimw.validatePrincipal returns the
	// session user unchanged when that field is nil, so a deactivated user's
	// unexpired cookie would reach the handler (#314).
	//
	// Authorization is unchanged: apimw.Auth still refuses an unauthenticated
	// request, and RequireResolvedPermissionsForProject still resolves the
	// per-method permission against the requested project.
	if store == nil || authConfig.PrincipalValidator == nil || permissions == nil {
		return nil, ErrInvalidCurrentNotificationAPIRoute
	}

	handler := &currentNotificationAPIHandler{store: store}
	router := chi.NewRouter()
	router.Method(http.MethodGet, CurrentNotificationsPath, currentNotificationEndpoint(
		handler.list, CurrentNotificationsListPermission, authConfig, permissions,
	))
	router.Method(http.MethodPut, CurrentNotificationsPath, currentNotificationEndpoint(
		handler.bulkUpdate, CurrentNotificationUpdatePermission, authConfig, permissions,
	))
	router.Method(http.MethodDelete, CurrentNotificationsPath, currentNotificationEndpoint(
		handler.bulkDelete, CurrentNotificationDeletePermission, authConfig, permissions,
	))
	router.Method(http.MethodGet, CurrentNotificationPath, currentNotificationEndpoint(
		handler.details, CurrentNotificationDetailsPermission, authConfig, permissions,
	))
	router.Method(http.MethodPut, CurrentNotificationPath, currentNotificationEndpoint(
		handler.markSeen, CurrentNotificationUpdatePermission, authConfig, permissions,
	))
	router.Method(http.MethodDelete, CurrentNotificationPath, currentNotificationEndpoint(
		handler.delete, CurrentNotificationDeletePermission, authConfig, permissions,
	))
	return &CurrentNotificationAPIRoute{handler: router}, nil
}

func (route *CurrentNotificationAPIRoute) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if route == nil || route.handler == nil {
		http.NotFound(writer, request)
		return
	}
	route.handler.ServeHTTP(writer, request)
}

func currentNotificationEndpoint(
	handler http.HandlerFunc,
	permission string,
	authConfig apimw.AuthConfig,
	permissions auth.PermissionResolver,
) http.Handler {
	endpoint := http.Handler(handler)
	endpoint = apimw.RequireResolvedPermissionsForProject(
		permissions,
		CurrentNotificationsMode,
		func(request *http.Request) (string, bool) {
			return currentNotificationProjectID(chi.URLParam(request, "projectID"))
		},
		permission,
	)(endpoint)
	return apimw.Auth(authConfig)(endpoint)
}

type currentNotificationAPIHandler struct {
	store notificationapp.Store
}

// CurrentNotificationToolHandler exposes only the three operations the
// current platform marks as Internal MCP tools. It deliberately reuses the
// same HTTP normalization and response projection as the browser REST route;
// authentication and permission checks remain at the MCP boundary.
type CurrentNotificationToolHandler struct {
	handler *currentNotificationAPIHandler
}

// NewCurrentNotificationToolHandler returns nil when no store is composed so
// the MCP category can list its fixed contract while execution fails closed.
func NewCurrentNotificationToolHandler(store notificationapp.Store) *CurrentNotificationToolHandler {
	if store == nil {
		return nil
	}
	return &CurrentNotificationToolHandler{
		handler: &currentNotificationAPIHandler{store: store},
	}
}

func (handler *CurrentNotificationToolHandler) List(writer http.ResponseWriter, request *http.Request) {
	handler.handler.list(writer, request)
}

func (handler *CurrentNotificationToolHandler) Details(writer http.ResponseWriter, request *http.Request) {
	handler.handler.details(writer, request)
}

func (handler *CurrentNotificationToolHandler) MarkSeen(writer http.ResponseWriter, request *http.Request) {
	handler.handler.markSeen(writer, request)
}

type currentNotificationResponse struct {
	ID        int32           `json:"id"`
	UUID      string          `json:"uuid"`
	IsSeen    bool            `json:"is_seen"`
	ProjectID int32           `json:"project_id"`
	UserID    int32           `json:"user_id"`
	Meta      json.RawMessage `json:"meta"`
	CreatedAt string          `json:"created_at"`
	UpdatedAt *string         `json:"updated_at"`
	EventType string          `json:"event_type"`
}

func (handler *currentNotificationAPIHandler) list(writer http.ResponseWriter, request *http.Request) {
	userID, ok := currentNotificationRequestUserID(request.Context())
	if !ok {
		writeCurrentNotificationAPIError(writer, http.StatusUnauthorized, "Authentication required")
		return
	}
	filter, onlyTotal, err := currentNotificationListFilter(request)
	if err != nil {
		writeCurrentNotificationAPIError(writer, http.StatusBadRequest, "Invalid notification query")
		return
	}
	total, err := handler.store.Count(request.Context(), userID, filter)
	if err != nil {
		writeCurrentNotificationAPIError(writer, http.StatusInternalServerError, "Notification request failed")
		return
	}
	if onlyTotal {
		writeCurrentNotificationJSON(writer, http.StatusOK, struct {
			Total int64 `json:"total"`
		}{Total: total})
		return
	}
	notifications, err := handler.store.List(request.Context(), userID, filter)
	if err != nil {
		writeCurrentNotificationAPIError(writer, http.StatusInternalServerError, "Notification request failed")
		return
	}
	rows := make([]currentNotificationResponse, 0, len(notifications))
	for _, notification := range notifications {
		rows = append(rows, currentNotificationDTO(notification, true))
	}
	writeCurrentNotificationJSON(writer, http.StatusOK, struct {
		Total int64                         `json:"total"`
		Rows  []currentNotificationResponse `json:"rows"`
	}{Total: total, Rows: rows})
}

func (handler *currentNotificationAPIHandler) details(writer http.ResponseWriter, request *http.Request) {
	handler.withNotification(writer, request, handler.store.Get)
}

func (handler *currentNotificationAPIHandler) markSeen(writer http.ResponseWriter, request *http.Request) {
	handler.withNotification(writer, request, handler.store.MarkSeen)
}

type currentNotificationRead func(context.Context, int64, int64) (notificationapp.Notification, error)

func (handler *currentNotificationAPIHandler) withNotification(
	writer http.ResponseWriter,
	request *http.Request,
	read currentNotificationRead,
) {
	userID, notificationID, ok := currentNotificationRequestIDs(request)
	if !ok {
		writeCurrentNotificationAPIError(writer, http.StatusBadRequest, "Invalid notification")
		return
	}
	notification, err := read(request.Context(), userID, notificationID)
	if errors.Is(err, notificationapp.ErrNotificationNotFound) {
		writeCurrentNotificationNotFound(writer)
		return
	}
	if err != nil {
		writeCurrentNotificationAPIError(writer, http.StatusInternalServerError, "Notification request failed")
		return
	}
	writeCurrentNotificationJSON(writer, http.StatusOK, currentNotificationDTO(notification, false))
}

func (handler *currentNotificationAPIHandler) delete(writer http.ResponseWriter, request *http.Request) {
	userID, notificationID, ok := currentNotificationRequestIDs(request)
	if !ok {
		writeCurrentNotificationAPIError(writer, http.StatusBadRequest, "Invalid notification")
		return
	}
	err := handler.store.Delete(request.Context(), userID, notificationID)
	if errors.Is(err, notificationapp.ErrNotificationNotFound) {
		writeCurrentNotificationNotFound(writer)
		return
	}
	if err != nil {
		writeCurrentNotificationAPIError(writer, http.StatusInternalServerError, "Notification request failed")
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

func (handler *currentNotificationAPIHandler) bulkUpdate(writer http.ResponseWriter, request *http.Request) {
	userID, ok := currentNotificationRequestUserID(request.Context())
	if !ok {
		writeCurrentNotificationAPIError(writer, http.StatusUnauthorized, "Authentication required")
		return
	}
	ids, all, isSeen, err := decodeCurrentNotificationBulkUpdate(writer, request)
	if err != nil {
		writeCurrentNotificationAPIError(writer, http.StatusBadRequest, "Invalid notification update")
		return
	}
	updated, err := handler.store.BulkSetSeen(request.Context(), userID, ids, all, isSeen)
	if err != nil {
		writeCurrentNotificationAPIError(writer, http.StatusInternalServerError, "Notification request failed")
		return
	}
	writeCurrentNotificationJSON(writer, http.StatusOK, struct {
		Updated int64 `json:"updated"`
	}{Updated: updated})
}

func (handler *currentNotificationAPIHandler) bulkDelete(writer http.ResponseWriter, request *http.Request) {
	userID, ok := currentNotificationRequestUserID(request.Context())
	if !ok {
		writeCurrentNotificationAPIError(writer, http.StatusUnauthorized, "Authentication required")
		return
	}
	ids, err := decodeCurrentNotificationBulkDelete(writer, request)
	if err != nil {
		writeCurrentNotificationAPIError(writer, http.StatusBadRequest, "Invalid notification delete")
		return
	}
	deleted, err := handler.store.BulkDelete(request.Context(), userID, ids)
	if err != nil {
		writeCurrentNotificationAPIError(writer, http.StatusInternalServerError, "Notification request failed")
		return
	}
	writeCurrentNotificationJSON(writer, http.StatusOK, struct {
		Deleted int64 `json:"deleted"`
	}{Deleted: deleted})
}

func currentNotificationListFilter(request *http.Request) (notificationapp.ListFilter, bool, error) {
	query := request.URL.Query()
	limit, err := currentNotificationQueryInt(query.Get("limit"), 10, 1, maxCurrentNotificationListLimit)
	if err != nil {
		return notificationapp.ListFilter{}, false, err
	}
	offset, err := currentNotificationQueryInt(query.Get("offset"), 0, 0, math.MaxInt32)
	if err != nil {
		return notificationapp.ListFilter{}, false, err
	}
	onlyNew, err := currentNotificationQueryBool(query.Get("only_new"))
	if err != nil {
		return notificationapp.ListFilter{}, false, err
	}
	onlyTotal, err := currentNotificationQueryBool(query.Get("only_total"))
	if err != nil {
		return notificationapp.ListFilter{}, false, err
	}
	sortBy := query.Get("sort_by")
	if sortBy == "" {
		sortBy = "created_at"
	}
	sortOrder := query.Get("sort_order")
	if sortOrder == "" {
		sortOrder = "desc"
	}
	if !currentNotificationAPISortValid(sortBy, sortOrder) {
		return notificationapp.ListFilter{}, false, errors.New("invalid notification sort")
	}
	return notificationapp.ListFilter{
		OnlyNew:     onlyNew,
		SearchWords: strings.Fields(query.Get("search")),
		EventType:   query.Get("event_type"),
		SortBy:      sortBy,
		SortOrder:   sortOrder,
		Limit:       int32(limit),
		Offset:      int32(offset),
	}, onlyTotal, nil
}

func currentNotificationQueryInt(value string, fallback, minimum, maximum int64) (int64, error) {
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 32)
	if err != nil || parsed < minimum || parsed > maximum {
		return 0, errors.New("invalid integer query")
	}
	return parsed, nil
}

func currentNotificationQueryBool(value string) (bool, error) {
	if value == "" {
		return false, nil
	}
	return strconv.ParseBool(value)
}

func currentNotificationAPISortValid(field, order string) bool {
	switch field {
	case "id", "uuid", "is_seen", "project_id", "user_id", "meta", "event_type", "created_at", "updated_at":
	default:
		return false
	}
	return order == "asc" || order == "desc"
}

func currentNotificationRequestIDs(request *http.Request) (int64, int64, bool) {
	userID, ok := currentNotificationRequestUserID(request.Context())
	if !ok {
		return 0, 0, false
	}
	notificationID, err := strconv.ParseInt(chi.URLParam(request, "notificationID"), 10, 32)
	return userID, notificationID, err == nil && notificationID > 0
}

func currentNotificationRequestUserID(ctx context.Context) (int64, bool) {
	user, ok := auth.UserFromContext(ctx)
	if !ok {
		return 0, false
	}
	userID, err := strconv.ParseInt(user.UserID, 10, 32)
	return userID, err == nil && userID > 0
}

func decodeCurrentNotificationBulkUpdate(
	writer http.ResponseWriter,
	request *http.Request,
) ([]int64, bool, bool, error) {
	var body struct {
		IDs    json.RawMessage `json:"ids"`
		IsSeen *bool           `json:"is_seen"`
	}
	if err := decodeCurrentNotificationBody(writer, request, &body); err != nil ||
		len(body.IDs) == 0 || body.IsSeen == nil {
		return nil, false, false, errors.New("invalid notification update")
	}
	if string(body.IDs) == `"all"` {
		return nil, true, *body.IsSeen, nil
	}
	var ids []int64
	if err := json.Unmarshal(body.IDs, &ids); err != nil || ids == nil || !currentNotificationAPIIDsValid(ids) {
		return nil, false, false, errors.New("invalid notification IDs")
	}
	return ids, false, *body.IsSeen, nil
}

func decodeCurrentNotificationBulkDelete(
	writer http.ResponseWriter,
	request *http.Request,
) ([]int64, error) {
	var body struct {
		IDs []int64 `json:"ids"`
	}
	if err := decodeCurrentNotificationBody(writer, request, &body); err != nil ||
		body.IDs == nil || !currentNotificationAPIIDsValid(body.IDs) {
		return nil, errors.New("invalid notification delete")
	}
	return body.IDs, nil
}

func decodeCurrentNotificationBody(writer http.ResponseWriter, request *http.Request, target any) error {
	request.Body = http.MaxBytesReader(writer, request.Body, maxCurrentNotificationRequestBytes)
	decoder := json.NewDecoder(request.Body)
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("multiple JSON values")
	}
	return nil
}

func currentNotificationAPIIDsValid(ids []int64) bool {
	for _, id := range ids {
		if id <= 0 || id > math.MaxInt32 {
			return false
		}
	}
	return true
}

func currentNotificationDTO(
	notification notificationapp.Notification,
	list bool,
) currentNotificationResponse {
	createdAt := currentNotificationDetailTime(notification.CreatedAt)
	var updatedAt *string
	if notification.UpdatedAt != nil {
		value := currentNotificationDetailTime(*notification.UpdatedAt)
		updatedAt = &value
	}
	if list {
		createdAt = notification.CreatedAt.UTC().Truncate(time.Second).Format(time.RFC3339)
		if notification.UpdatedAt != nil {
			value := notification.UpdatedAt.UTC().Truncate(time.Second).Format(time.RFC3339)
			updatedAt = &value
		}
	}
	return currentNotificationResponse{
		ID:        notification.ID,
		UUID:      notification.UUID,
		IsSeen:    notification.IsSeen,
		ProjectID: notification.ProjectID,
		UserID:    notification.UserID,
		Meta:      notification.Meta,
		CreatedAt: createdAt,
		UpdatedAt: updatedAt,
		EventType: notification.EventType,
	}
}

func currentNotificationDetailTime(value time.Time) string {
	value = value.UTC()
	if value.Nanosecond() == 0 {
		return value.Format("2006-01-02T15:04:05")
	}
	return value.Format("2006-01-02T15:04:05.000000")
}

func writeCurrentNotificationNotFound(writer http.ResponseWriter) {
	writeCurrentNotificationJSON(writer, http.StatusBadRequest, struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}{OK: false, Error: "Notification is not found"})
}

func writeCurrentNotificationAPIError(writer http.ResponseWriter, status int, message string) {
	writeCurrentNotificationJSON(writer, status, struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}{OK: false, Error: message})
}

func writeCurrentNotificationJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
