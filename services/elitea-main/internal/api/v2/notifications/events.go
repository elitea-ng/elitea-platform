package notifications

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	CurrentNotificationEventsPath       = "/api/v2/notifications/events/prompt_lib/{projectID}"
	CurrentNotificationEventsMode       = auth.PermissionModeDefault
	CurrentNotificationEventsPermission = "models.notifications.notifications.list"
	CurrentNotificationEventName        = "notifications_notify"
	currentNotificationReadyEventName   = "notifications_ready"

	currentNotificationBatchSize    = int32(100)
	currentNotificationPollInterval = 2 * time.Second
	currentNotificationHeartbeat    = 15 * time.Second
	currentNotificationAuthTimeout  = 2 * time.Second
	currentNotificationWriteTimeout = 10 * time.Second
	maxCurrentNotificationEventSize = 64 * 1024
)

var ErrInvalidCurrentNotificationEventsRoute = errors.New("invalid current notification events route")

var errCurrentNotificationEventTooLarge = errors.New("current notification event exceeds the stream contract")

// CurrentNotificationEventsRoute replaces only the current user notification
// Socket.IO push. PostgreSQL remains the durable authority, which permits an
// SSE reconnect to resume from Last-Event-ID on any Main replica.
type CurrentNotificationEventsRoute struct {
	handler http.Handler
}

func NewCurrentNotificationEventsRoute(
	reader notificationapp.EventReader,
	authConfig apimw.AuthConfig,
	permissions auth.PermissionResolver,
) (*CurrentNotificationEventsRoute, error) {
	if reader == nil || permissions == nil {
		return nil, ErrInvalidCurrentNotificationEventsRoute
	}
	// PrincipalValidator and FormSessions are optional — when nil
	// the auth middleware falls back to session-cookie verification, which is
	// the only credential OIDC-only deployments have (no
	// ELITEA_AUTH_CONFIG_FILE, hence no FormGraph). Requiring them made this
	// route composable ONLY under Form auth, so `GET /api/v2/notifications/
	// events/prompt_lib/{projectID}` — the URL useNotificationsSSE actually
	// opens — 404'd in the E2E stack and in every other OIDC-only deployment
	// (#152). Same relaxation, and the same reason, as
	// v2projects.NewCurrentProjectListRoute.
	//
	// This does NOT weaken authorization: apimw.Auth still rejects an
	// unauthenticated request, serve() still requires a runtime principal, and
	// authorize() still resolves models.notifications.notifications.list
	// against the requested project.
	stream := &currentNotificationEventsHandler{
		reader:      reader,
		permissions: permissions,
		admission:   newCurrentNotificationAdmission(64, 4),
	}
	endpoint := apimw.Auth(authConfig)(http.HandlerFunc(stream.serve))
	router := chi.NewRouter()
	router.Method(http.MethodGet, CurrentNotificationEventsPath, endpoint)
	return &CurrentNotificationEventsRoute{handler: router}, nil
}

func (route *CurrentNotificationEventsRoute) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if route == nil || route.handler == nil {
		http.NotFound(writer, request)
		return
	}
	route.handler.ServeHTTP(writer, request)
}

type currentNotificationEventsHandler struct {
	reader      notificationapp.EventReader
	permissions auth.PermissionResolver
	admission   *currentNotificationAdmission
}

func (handler *currentNotificationEventsHandler) serve(writer http.ResponseWriter, request *http.Request) {
	projectID, ok := currentNotificationProjectID(chi.URLParam(request, "projectID"))
	if !ok {
		http.Error(writer, "invalid notification project", http.StatusBadRequest)
		return
	}
	principal, ok := auth.RuntimePrincipalFromContext(request.Context())
	if !ok {
		http.Error(writer, "authentication required", http.StatusUnauthorized)
		return
	}
	userID, err := handler.authorize(request.Context(), principal, projectID)
	if err != nil {
		http.Error(writer, "forbidden", http.StatusForbidden)
		return
	}
	cursor, suppliedCursor, err := currentNotificationCursor(request)
	if err != nil {
		http.Error(writer, "invalid notification cursor", http.StatusBadRequest)
		return
	}
	if !suppliedCursor {
		cursor, err = handler.reader.HighWater(request.Context(), userID)
		if err != nil {
			http.Error(writer, "notification stream unavailable", http.StatusServiceUnavailable)
			return
		}
	}
	release, admitted := handler.admission.acquire(strconv.FormatInt(userID, 10))
	if !admitted {
		writer.Header().Set("Retry-After", "2")
		http.Error(writer, "too many active notification streams", http.StatusTooManyRequests)
		return
	}
	defer release()

	events, err := handler.reader.ListAfter(
		request.Context(),
		userID,
		cursor,
		currentNotificationBatchSize,
	)
	if err != nil {
		http.Error(writer, "notification stream unavailable", http.StatusServiceUnavailable)
		return
	}
	stream, err := newCurrentNotificationSSEWriter(writer, currentNotificationWriteTimeout)
	if err != nil {
		http.Error(writer, "streaming not supported", http.StatusInternalServerError)
		return
	}
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache, no-transform")
	writer.Header().Set("X-Accel-Buffering", "no")
	if err := stream.writeAndFlush(func() error {
		writer.WriteHeader(http.StatusOK)
		if suppliedCursor {
			return nil
		}
		return writeCurrentNotificationReady(writer, cursor)
	}); err != nil {
		return
	}

	lastAuthorization := time.Now()
	lastHeartbeat := time.Now()
	for {
		if len(events) > 0 {
			if err := stream.writeAndFlush(func() error {
				for _, event := range events {
					if event.Cursor <= cursor {
						return errors.New("notification cursor is not increasing")
					}
					if writeErr := writeCurrentNotificationEvent(writer, event); writeErr != nil {
						if !errors.Is(writeErr, errCurrentNotificationEventTooLarge) {
							return writeErr
						}
						// The durable list remains authoritative for oversized metadata.
						// Advancing with ready avoids a reconnect loop while prompting the
						// UI to refresh unread state without receiving the large body.
						if readyErr := writeCurrentNotificationReady(writer, event.Cursor); readyErr != nil {
							return readyErr
						}
					}
					cursor = event.Cursor
				}
				return nil
			}); err != nil {
				return
			}
		}
		if len(events) == int(currentNotificationBatchSize) {
			events, err = handler.reader.ListAfter(
				request.Context(), userID, cursor, currentNotificationBatchSize,
			)
			if err != nil {
				return
			}
			continue
		}

		timer := time.NewTimer(currentNotificationPollInterval)
		select {
		case <-request.Context().Done():
			if !timer.Stop() {
				<-timer.C
			}
			return
		case <-timer.C:
		}
		if time.Since(lastAuthorization) >= currentNotificationHeartbeat {
			resolvedUserID, authorizeErr := handler.authorize(request.Context(), principal, projectID)
			if authorizeErr != nil || resolvedUserID != userID {
				return
			}
			lastAuthorization = time.Now()
		}
		if time.Since(lastHeartbeat) >= currentNotificationHeartbeat {
			if err := stream.writeAndFlush(func() error {
				_, writeErr := fmt.Fprint(writer, ": heartbeat\n\n")
				return writeErr
			}); err != nil {
				return
			}
			lastHeartbeat = time.Now()
		}
		events, err = handler.reader.ListAfter(
			request.Context(), userID, cursor, currentNotificationBatchSize,
		)
		if err != nil {
			return
		}
	}
}

func (handler *currentNotificationEventsHandler) authorize(
	ctx context.Context,
	principal auth.User,
	projectID string,
) (int64, error) {
	if handler == nil || handler.permissions == nil {
		return 0, ErrInvalidCurrentNotificationEventsRoute
	}
	authorizationContext, cancel := context.WithTimeout(ctx, currentNotificationAuthTimeout)
	defer cancel()
	resolution, err := handler.permissions.ResolvePermissions(
		authorizationContext,
		principal,
		CurrentNotificationEventsMode,
		projectID,
	)
	if err != nil || resolution.UserID <= 0 ||
		!currentNotificationHasPermission(resolution.Permissions) {
		return 0, errors.New("notification events are forbidden")
	}
	return resolution.UserID, nil
}

func currentNotificationHasPermission(permissions []string) bool {
	for _, permission := range permissions {
		if permission == CurrentNotificationEventsPermission {
			return true
		}
	}
	return false
}

func currentNotificationProjectID(value string) (string, bool) {
	if value == "" {
		return "", false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return "", false
		}
	}
	projectID, err := strconv.ParseInt(value, 10, 64)
	return value, err == nil && projectID > 0
}

func currentNotificationCursor(request *http.Request) (int64, bool, error) {
	header := strings.TrimSpace(request.Header.Get("Last-Event-ID"))
	query := strings.TrimSpace(request.URL.Query().Get("cursor"))
	if header != "" && query != "" && header != query {
		return 0, false, errors.New("conflicting notification cursors")
	}
	value := header
	if value == "" {
		value = query
	}
	if value == "" {
		return 0, false, nil
	}
	cursor, err := strconv.ParseInt(value, 10, 32)
	if err != nil || cursor < 0 {
		return 0, true, errors.New("invalid notification cursor")
	}
	return cursor, true, nil
}

type currentNotificationPayload struct {
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

func writeCurrentNotificationReady(writer http.ResponseWriter, cursor int64) error {
	data, err := json.Marshal(struct {
		Cursor int64 `json:"cursor"`
	}{Cursor: cursor})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(
		writer,
		"retry: 2000\nid: %d\nevent: %s\ndata: %s\n\n",
		cursor,
		currentNotificationReadyEventName,
		data,
	)
	return err
}

func writeCurrentNotificationEvent(writer http.ResponseWriter, event notificationapp.Event) error {
	if event.Cursor <= 0 || event.Cursor > math.MaxInt32 || event.UUID == "" ||
		event.ProjectID <= 0 || event.UserID <= 0 || event.EventType == "" ||
		event.CreatedAt.IsZero() || len(event.Meta) == 0 || !json.Valid(event.Meta) {
		return errors.New("invalid current notification event")
	}
	var updatedAt *string
	if event.UpdatedAt != nil {
		value := event.UpdatedAt.UTC().Format(time.RFC3339)
		updatedAt = &value
	}
	data, err := json.Marshal(currentNotificationPayload{
		ID:        int32(event.Cursor),
		UUID:      event.UUID,
		IsSeen:    event.IsSeen,
		ProjectID: event.ProjectID,
		UserID:    event.UserID,
		Meta:      event.Meta,
		CreatedAt: event.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt: updatedAt,
		EventType: event.EventType,
	})
	if err != nil {
		return err
	}
	if len(data) > maxCurrentNotificationEventSize {
		return errCurrentNotificationEventTooLarge
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, data); err != nil {
		return err
	}
	_, err = fmt.Fprintf(
		writer,
		"id: %d\nevent: %s\ndata: %s\n\n",
		event.Cursor,
		CurrentNotificationEventName,
		compact.Bytes(),
	)
	return err
}

type currentNotificationSSEWriter struct {
	controller *http.ResponseController
	timeout    time.Duration
}

func newCurrentNotificationSSEWriter(
	writer http.ResponseWriter,
	timeout time.Duration,
) (*currentNotificationSSEWriter, error) {
	if writer == nil || timeout <= 0 || !currentNotificationSupportsFlush(writer) {
		return nil, errors.New("invalid notification SSE writer")
	}
	controller := http.NewResponseController(writer)
	if err := controller.SetWriteDeadline(time.Time{}); err != nil {
		return nil, fmt.Errorf("clear notification SSE write deadline: %w", err)
	}
	return &currentNotificationSSEWriter{controller: controller, timeout: timeout}, nil
}

func currentNotificationSupportsFlush(writer http.ResponseWriter) bool {
	for depth := 0; depth < 16 && writer != nil; depth++ {
		if _, ok := writer.(interface{ FlushError() error }); ok {
			return true
		}
		if _, ok := writer.(http.Flusher); ok {
			return true
		}
		unwrapper, ok := writer.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return false
		}
		next := unwrapper.Unwrap()
		if next == writer {
			return false
		}
		writer = next
	}
	return false
}

func (writer *currentNotificationSSEWriter) writeAndFlush(write func() error) error {
	if writer == nil || writer.controller == nil || write == nil {
		return errors.New("notification SSE write is unavailable")
	}
	if err := writer.controller.SetWriteDeadline(time.Now().Add(writer.timeout)); err != nil {
		return err
	}
	writeErr := write()
	flushErr := writer.controller.Flush()
	clearErr := writer.controller.SetWriteDeadline(time.Time{})
	return errors.Join(writeErr, flushErr, clearErr)
}
