package notifications

// Web push (ADR-0045 decision 6, #1437): the push configuration and the
// caller's browser subscriptions. Subscriptions are per user; the project in
// the path is the permission scope only, like the bell's routes.

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"

	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/webpush"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

const (
	PushConfigPath        = "/api/v2/notifications/push_config/prompt_lib/{projectID}"
	PushSubscriptionsPath = "/api/v2/notifications/push_subscriptions/prompt_lib/{projectID}"

	// maxPushEndpointBytes bounds a push service endpoint.
	maxPushEndpointBytes = 2048
	// maxPushUserAgentBytes bounds the stored user agent.
	maxPushUserAgentBytes = 512
)

var ErrInvalidPushRoute = errors.New("invalid web push route dependencies")

// PushConfigSource answers the push configuration (webpush.Service).
type PushConfigSource interface {
	Config(ctx context.Context) webpush.Config
}

// PushSubscriptionStore stores subscriptions (repos.WebPushRepository).
type PushSubscriptionStore interface {
	SaveSubscription(ctx context.Context, subscription repos.PushSubscription) (string, bool, error)
	DeleteSubscription(ctx context.Context, userID int64, endpoint string) error
}

// PushEndpointValidator refuses an endpoint Main must not send to (the
// egress guard: a private or reserved address).
type PushEndpointValidator func(ctx context.Context, endpoint string) error

// PushRoute serves the three web push routes.
type PushRoute struct {
	handler http.Handler
}

// NewPushRoute builds the routes. validate may be nil (tests).
func NewPushRoute(
	config PushConfigSource,
	store PushSubscriptionStore,
	validate PushEndpointValidator,
	authConfig apimw.AuthConfig,
	permissions auth.PermissionResolver,
) (*PushRoute, error) {
	if config == nil || store == nil || authConfig.PrincipalValidator == nil || permissions == nil {
		return nil, ErrInvalidPushRoute
	}
	handler := &pushHandler{config: config, store: store, validate: validate}
	router := chi.NewRouter()
	router.Method(http.MethodGet, PushConfigPath, currentNotificationEndpoint(
		handler.pushConfig, CurrentNotificationsListPermission, authConfig, permissions,
	))
	router.Method(http.MethodPost, PushSubscriptionsPath, currentNotificationEndpoint(
		handler.subscribe, CurrentNotificationUpdatePermission, authConfig, permissions,
	))
	router.Method(http.MethodDelete, PushSubscriptionsPath, currentNotificationEndpoint(
		handler.unsubscribe, CurrentNotificationUpdatePermission, authConfig, permissions,
	))
	return &PushRoute{handler: router}, nil
}

func (route *PushRoute) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if route == nil || route.handler == nil {
		http.NotFound(writer, request)
		return
	}
	route.handler.ServeHTTP(writer, request)
}

type pushHandler struct {
	config   PushConfigSource
	store    PushSubscriptionStore
	validate PushEndpointValidator
}

func (handler *pushHandler) pushConfig(writer http.ResponseWriter, request *http.Request) {
	if _, ok := currentNotificationRequestUserID(request.Context()); !ok {
		writeCurrentNotificationAPIError(writer, http.StatusUnauthorized, "Not authenticated")
		return
	}
	config := handler.config.Config(request.Context())
	writeCurrentNotificationJSON(writer, http.StatusOK, struct {
		Enabled   bool   `json:"enabled"`
		PublicKey string `json:"public_key"`
	}{Enabled: config.Enabled, PublicKey: config.PublicKey})
}

type pushSubscriptionBody struct {
	Endpoint string `json:"endpoint"`
	Keys     *struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

func (handler *pushHandler) subscribe(writer http.ResponseWriter, request *http.Request) {
	userID, ok := currentNotificationRequestUserID(request.Context())
	if !ok {
		writeCurrentNotificationAPIError(writer, http.StatusUnauthorized, "Not authenticated")
		return
	}
	var body pushSubscriptionBody
	if err := decodeCurrentNotificationBody(writer, request, &body); err != nil || body.Keys == nil {
		writeCurrentNotificationAPIError(writer, http.StatusBadRequest, "invalid_push_subscription")
		return
	}
	if !validPushEndpoint(body.Endpoint) || !webpush.ValidSubscriptionKeys(body.Keys.P256dh, body.Keys.Auth) {
		writeCurrentNotificationAPIError(writer, http.StatusBadRequest, "invalid_push_subscription")
		return
	}
	if handler.validate != nil {
		if err := handler.validate(request.Context(), body.Endpoint); err != nil {
			writeCurrentNotificationAPIError(writer, http.StatusBadRequest, "push_endpoint_refused")
			return
		}
	}
	if !handler.config.Config(request.Context()).Enabled {
		writeCurrentNotificationAPIError(writer, http.StatusConflict, "web_push_disabled")
		return
	}
	userAgent := request.UserAgent()
	if len(userAgent) > maxPushUserAgentBytes {
		userAgent = strings.ToValidUTF8(userAgent[:maxPushUserAgentBytes], "")
	}
	id, created, err := handler.store.SaveSubscription(request.Context(), repos.PushSubscription{
		UserID: userID, Endpoint: body.Endpoint, P256dh: body.Keys.P256dh, Auth: body.Keys.Auth,
		UserAgent: userAgent,
	})
	if err != nil {
		writeCurrentNotificationAPIError(writer, http.StatusInternalServerError, "Could not save the subscription")
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeCurrentNotificationJSON(writer, status, struct {
		ID string `json:"id"`
	}{ID: id})
}

func (handler *pushHandler) unsubscribe(writer http.ResponseWriter, request *http.Request) {
	userID, ok := currentNotificationRequestUserID(request.Context())
	if !ok {
		writeCurrentNotificationAPIError(writer, http.StatusUnauthorized, "Not authenticated")
		return
	}
	var body struct {
		Endpoint string `json:"endpoint"`
	}
	if err := decodeCurrentNotificationBody(writer, request, &body); err != nil || body.Endpoint == "" ||
		len(body.Endpoint) > maxPushEndpointBytes {
		writeCurrentNotificationAPIError(writer, http.StatusBadRequest, "invalid_push_subscription")
		return
	}
	if err := handler.store.DeleteSubscription(request.Context(), userID, body.Endpoint); err != nil {
		writeCurrentNotificationAPIError(writer, http.StatusInternalServerError, "Could not delete the subscription")
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

// validPushEndpoint: an absolute https URL with a host, no user info, at
// most 2 KiB.
func validPushEndpoint(endpoint string) bool {
	if endpoint == "" || len(endpoint) > maxPushEndpointBytes {
		return false
	}
	parsed, err := url.Parse(endpoint)
	return err == nil && parsed.Scheme == "https" && parsed.Hostname() != "" && parsed.User == nil &&
		parsed.Fragment == ""
}
