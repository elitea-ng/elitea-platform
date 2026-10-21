package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware/authtest"
	notificationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/notifications"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/go-chi/chi/v5"
)

type currentNotificationStoreStub struct {
	userID         int64
	notificationID int64
	filter         notificationapp.ListFilter
	ids            []int64
	all            bool
	isSeen         bool
	count          int64
	notifications  []notificationapp.Notification
	notification   notificationapp.Notification
	changed        int64
	err            error
	operation      string
}

func (stub *currentNotificationStoreStub) Count(
	_ context.Context,
	userID int64,
	filter notificationapp.ListFilter,
) (int64, error) {
	stub.operation = "count"
	stub.userID, stub.filter = userID, filter
	return stub.count, stub.err
}

func (stub *currentNotificationStoreStub) List(
	_ context.Context,
	userID int64,
	filter notificationapp.ListFilter,
) ([]notificationapp.Notification, error) {
	stub.operation = "list"
	stub.userID, stub.filter = userID, filter
	return append([]notificationapp.Notification(nil), stub.notifications...), stub.err
}

func (stub *currentNotificationStoreStub) Get(
	_ context.Context,
	userID,
	notificationID int64,
) (notificationapp.Notification, error) {
	stub.operation = "get"
	stub.userID, stub.notificationID = userID, notificationID
	return stub.notification, stub.err
}

func (stub *currentNotificationStoreStub) MarkSeen(
	_ context.Context,
	userID,
	notificationID int64,
) (notificationapp.Notification, error) {
	stub.operation = "mark"
	stub.userID, stub.notificationID = userID, notificationID
	return stub.notification, stub.err
}

func (stub *currentNotificationStoreStub) Delete(
	_ context.Context,
	userID,
	notificationID int64,
) error {
	stub.operation = "delete"
	stub.userID, stub.notificationID = userID, notificationID
	return stub.err
}

func (stub *currentNotificationStoreStub) BulkSetSeen(
	_ context.Context,
	userID int64,
	ids []int64,
	all,
	isSeen bool,
) (int64, error) {
	stub.operation = "bulk-update"
	stub.userID, stub.ids, stub.all, stub.isSeen = userID, append([]int64(nil), ids...), all, isSeen
	return stub.changed, stub.err
}

func (stub *currentNotificationStoreStub) BulkDelete(
	_ context.Context,
	userID int64,
	ids []int64,
) (int64, error) {
	stub.operation = "bulk-delete"
	stub.userID, stub.ids = userID, append([]int64(nil), ids...)
	return stub.changed, stub.err
}

func TestCurrentNotificationListPreservesCurrentFilterAndWireContract(t *testing.T) {
	createdAt := time.Date(2026, time.July, 31, 17, 50, 0, 123456000, time.UTC)
	updatedAt := createdAt.Add(time.Minute)
	store := &currentNotificationStoreStub{
		count: 1,
		notifications: []notificationapp.Notification{{
			ID: 15, UUID: "75cd2484-bbd0-46e7-b607-92b8df784cab", ProjectID: 2, UserID: 42,
			Meta: json.RawMessage(`{"message":"Budget index completed"}`), EventType: "budget_threshold",
			CreatedAt: createdAt, UpdatedAt: &updatedAt,
		}},
	}
	handler := &currentNotificationAPIHandler{store: store}
	request := currentNotificationAPIRequest(
		http.MethodGet,
		"/?limit=25&offset=3&sort_by=event_type&sort_order=asc&only_new=true&search=Budget+index&event_type=budget_threshold",
		nil,
		"",
	)
	response := httptest.NewRecorder()

	handler.list(response, request)

	if response.Code != http.StatusOK || store.userID != 42 || store.operation != "list" {
		t.Fatalf("status=%d operation=%q user=%d body=%s", response.Code, store.operation, store.userID, response.Body.String())
	}
	wantFilter := notificationapp.ListFilter{
		OnlyNew: true, SearchWords: []string{"Budget", "index"}, EventType: "budget_threshold",
		SortBy: "event_type", SortOrder: "asc", Limit: 25, Offset: 3,
	}
	if !reflect.DeepEqual(store.filter, wantFilter) {
		t.Fatalf("filter=%+v want=%+v", store.filter, wantFilter)
	}
	body := response.Body.String()
	for _, want := range []string{
		`"total":1`, `"id":15`, `"event_type":"budget_threshold"`,
		`"created_at":"2026-07-31T17:50:00Z"`, `"updated_at":"2026-07-31T17:51:00Z"`,
		`"meta":{"message":"Budget index completed"}`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("list body %q missing %q", body, want)
		}
	}
}

func TestCurrentNotificationOnlyTotalDoesNotReadRows(t *testing.T) {
	store := &currentNotificationStoreStub{count: 7}
	handler := &currentNotificationAPIHandler{store: store}
	response := httptest.NewRecorder()
	handler.list(response, currentNotificationAPIRequest(http.MethodGet, "/?only_total=true", nil, ""))
	if response.Code != http.StatusOK || store.operation != "count" || response.Body.String() != "{\"total\":7}\n" {
		t.Fatalf("status=%d operation=%q body=%q", response.Code, store.operation, response.Body.String())
	}
}

func TestCurrentNotificationDetailAndMarkSeenPreservePydanticTimestampShape(t *testing.T) {
	createdAt := time.Date(2026, time.July, 31, 17, 50, 0, 123456000, time.UTC)
	store := &currentNotificationStoreStub{notification: notificationapp.Notification{
		ID: 15, UUID: "75cd2484-bbd0-46e7-b607-92b8df784cab", IsSeen: true,
		ProjectID: 2, UserID: 42, Meta: json.RawMessage(`{"message":"Generic activity"}`),
		EventType: "arbitrary_activity", CreatedAt: createdAt, UpdatedAt: &createdAt,
	}}
	handler := &currentNotificationAPIHandler{store: store}
	for _, test := range []struct {
		name string
		call func(http.ResponseWriter, *http.Request)
		want string
	}{{"details", handler.details, "get"}, {"mark seen", handler.markSeen, "mark"}} {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			test.call(response, currentNotificationAPIRequest(http.MethodGet, "/", nil, "15"))
			if response.Code != http.StatusOK || store.operation != test.want || store.userID != 42 || store.notificationID != 15 {
				t.Fatalf("status=%d operation=%q user/id=%d/%d body=%s", response.Code, store.operation, store.userID, store.notificationID, response.Body.String())
			}
			if !strings.Contains(response.Body.String(), `"created_at":"2026-07-31T17:50:00.123456"`) ||
				strings.Contains(response.Body.String(), `17:50:00.123456Z`) {
				t.Fatalf("detail timestamp contract drifted: %s", response.Body.String())
			}
		})
	}
}

func TestCurrentNotificationBulkOperationsAreUserScoped(t *testing.T) {
	store := &currentNotificationStoreStub{changed: 2}
	handler := &currentNotificationAPIHandler{store: store}

	update := httptest.NewRecorder()
	handler.bulkUpdate(update, currentNotificationAPIRequest(
		http.MethodPut, "/", strings.NewReader(`{"ids":"all","is_seen":true}`), "",
	))
	if update.Code != http.StatusOK || store.operation != "bulk-update" || store.userID != 42 ||
		!store.all || !store.isSeen || len(store.ids) != 0 || update.Body.String() != "{\"updated\":2}\n" {
		t.Fatalf("bulk update state=%+v body=%s", store, update.Body.String())
	}

	deleteResponse := httptest.NewRecorder()
	handler.bulkDelete(deleteResponse, currentNotificationAPIRequest(
		http.MethodDelete, "/", strings.NewReader(`{"ids":[15,16]}`), "",
	))
	if deleteResponse.Code != http.StatusOK || store.operation != "bulk-delete" || store.userID != 42 ||
		!reflect.DeepEqual(store.ids, []int64{15, 16}) || deleteResponse.Body.String() != "{\"deleted\":2}\n" {
		t.Fatalf("bulk delete state=%+v body=%s", store, deleteResponse.Body.String())
	}
}

func TestCurrentNotificationMissingAndInvalidRequestsAreSafe(t *testing.T) {
	store := &currentNotificationStoreStub{err: notificationapp.ErrNotificationNotFound}
	handler := &currentNotificationAPIHandler{store: store}
	missing := httptest.NewRecorder()
	handler.details(missing, currentNotificationAPIRequest(http.MethodGet, "/", nil, "88"))
	if missing.Code != http.StatusBadRequest || missing.Body.String() != "{\"ok\":false,\"error\":\"Notification is not found\"}\n" {
		t.Fatalf("missing response status=%d body=%q", missing.Code, missing.Body.String())
	}

	invalid := httptest.NewRecorder()
	handler.bulkUpdate(invalid, currentNotificationAPIRequest(
		http.MethodPut, "/", strings.NewReader(`{"ids":[1]}`), "",
	))
	if invalid.Code != http.StatusBadRequest || strings.Contains(invalid.Body.String(), "validation") {
		t.Fatalf("invalid response status=%d body=%q", invalid.Code, invalid.Body.String())
	}
}

func TestCurrentNotificationRouteBindsEveryMethodToCurrentPermission(t *testing.T) {
	tests := []struct {
		method     string
		path       string
		permission string
		body       string
	}{{http.MethodGet, "/api/v2/notifications/notifications/prompt_lib/7?only_total=true", CurrentNotificationsListPermission, ""},
		{http.MethodPut, "/api/v2/notifications/notifications/prompt_lib/7", CurrentNotificationUpdatePermission, `{"ids":[],"is_seen":true}`},
		{http.MethodDelete, "/api/v2/notifications/notifications/prompt_lib/7", CurrentNotificationDeletePermission, `{"ids":[]}`},
		{http.MethodGet, "/api/v2/notifications/notification/prompt_lib/7/15", CurrentNotificationDetailsPermission, ""},
		{http.MethodPut, "/api/v2/notifications/notification/prompt_lib/7/15", CurrentNotificationUpdatePermission, ""},
		{http.MethodDelete, "/api/v2/notifications/notification/prompt_lib/7/15", CurrentNotificationDeletePermission, ""},
	}
	for _, test := range tests {
		t.Run(test.method+" "+test.path, func(t *testing.T) {
			store := &currentNotificationStoreStub{}
			route := newCurrentNotificationAPITestRoute(t, store, test.permission)
			request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
			authtest.SignIn(request, "42")
			request.RemoteAddr = "10.0.0.8:43120"
			response := httptest.NewRecorder()
			route.ServeHTTP(response, request)
			if response.Code < 200 || response.Code >= 300 || store.userID != 42 {
				t.Fatalf("status=%d user=%d operation=%q body=%s", response.Code, store.userID, store.operation, response.Body.String())
			}
		})
	}
}

func TestCurrentNotificationRouteRejectsCrossProjectPermission(t *testing.T) {
	store := &currentNotificationStoreStub{}
	route := newCurrentNotificationAPITestRoute(t, store, "different.permission")
	request := httptest.NewRequest(http.MethodGet, "/api/v2/notifications/notifications/prompt_lib/7", nil)
	authtest.SignIn(request, "42")
	request.RemoteAddr = "10.0.0.8:43120"
	response := httptest.NewRecorder()
	route.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || store.operation != "" {
		t.Fatalf("status=%d operation=%q body=%s", response.Code, store.operation, response.Body.String())
	}
}

type currentNotificationPrincipalValidatorFunc func(context.Context, auth.User) (auth.User, error)

func (function currentNotificationPrincipalValidatorFunc) ValidatePrincipal(
	ctx context.Context,
	user auth.User,
) (auth.User, error) {
	return function(ctx, user)
}

func newCurrentNotificationAPITestRoute(
	t *testing.T,
	store notificationapp.Store,
	permission string,
) *CurrentNotificationAPIRoute {
	t.Helper()
	route, err := NewCurrentNotificationAPIRoute(
		store,
		apimw.AuthConfig{
			PrincipalValidator: currentNotificationPrincipalValidatorFunc(func(_ context.Context, user auth.User) (auth.User, error) {
				if user.ID != "42" {
					return auth.User{}, errors.New("unexpected user")
				}
				return user, nil
			}),
			FormSessions: authtest.FormSessions{},
		},
		currentNotificationPermissionResolver{userID: 42, permissions: []string{permission}},
	)
	if err != nil {
		t.Fatal(err)
	}
	return route
}

func currentNotificationAPIRequest(
	method,
	target string,
	body io.Reader,
	notificationID string,
) *http.Request {
	request := httptest.NewRequest(method, target, body)
	request = request.WithContext(auth.ContextWithUser(request.Context(), auth.User{ID: "42", UserID: "42"}))
	if notificationID == "" {
		return request
	}
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("notificationID", notificationID)
	return request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, routeContext))
}
