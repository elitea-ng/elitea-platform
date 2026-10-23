package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	notificationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/notifications"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/go-chi/chi/v5"
)

type currentNotificationReaderStub struct {
	highWaterCalls int
	users          []int64
	cursors        []int64
	events         []notificationapp.Event
	cancel         context.CancelFunc
}

func (stub *currentNotificationReaderStub) HighWater(_ context.Context, userID int64) (int64, error) {
	stub.highWaterCalls++
	stub.users = append(stub.users, userID)
	return 4, nil
}

func (stub *currentNotificationReaderStub) ListAfter(
	_ context.Context,
	userID,
	cursor int64,
	_ int32,
) ([]notificationapp.Event, error) {
	stub.users = append(stub.users, userID)
	stub.cursors = append(stub.cursors, cursor)
	if stub.cancel != nil {
		stub.cancel()
	}
	return append([]notificationapp.Event(nil), stub.events...), nil
}

type currentNotificationPermissionResolver struct {
	userID      int64
	permissions []string
	err         error
}

func (resolver currentNotificationPermissionResolver) ResolvePermissions(
	context.Context,
	auth.User,
	string,
	string,
) (auth.PermissionResolution, error) {
	return auth.PermissionResolution{
		UserID:      resolver.userID,
		Permissions: resolver.permissions,
	}, resolver.err
}

type currentNotificationStreamingRecorder struct {
	*httptest.ResponseRecorder
}

func newCurrentNotificationStreamingRecorder() *currentNotificationStreamingRecorder {
	return &currentNotificationStreamingRecorder{ResponseRecorder: httptest.NewRecorder()}
}

func (recorder *currentNotificationStreamingRecorder) SetWriteDeadline(time.Time) error { return nil }
func (recorder *currentNotificationStreamingRecorder) Flush()                           { recorder.ResponseRecorder.Flush() }

func TestCurrentNotificationEventsAreUserScopedAndPreserveSocketPayload(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	eventTime := time.Date(2026, time.July, 31, 16, 30, 0, 987654321, time.UTC)
	reader := &currentNotificationReaderStub{
		cancel: cancel,
		events: []notificationapp.Event{{
			Cursor:    5,
			UUID:      "81816ebd-64de-4a55-815a-2d37471abf2e",
			ProjectID: 8,
			UserID:    42,
			Meta:      json.RawMessage(`{"message":"Scheduled indexing completed"}`),
			EventType: "index_data_changed",
			CreatedAt: eventTime,
		}},
	}
	handler := currentNotificationEventsHandler{
		reader: reader,
		permissions: currentNotificationPermissionResolver{
			userID:      42,
			permissions: []string{CurrentNotificationEventsPermission},
		},
		admission: newCurrentNotificationAdmission(4, 2),
	}
	request := currentNotificationRequest(ctx, "7")
	response := newCurrentNotificationStreamingRecorder()

	handler.serve(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	if response.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("content type = %q", response.Header().Get("Content-Type"))
	}
	if reader.highWaterCalls != 1 || len(reader.cursors) != 1 || reader.cursors[0] != 4 {
		t.Fatalf("high-water/list calls = %d/%v, want 1/[4]", reader.highWaterCalls, reader.cursors)
	}
	for _, userID := range reader.users {
		if userID != 42 {
			t.Fatalf("reader user = %d, want resolved owner 42", userID)
		}
	}
	body := response.Body.String()
	for _, want := range []string{
		"id: 4\nevent: notifications_ready\ndata: {\"cursor\":4}",
		"id: 5\nevent: notifications_notify\ndata: ",
		`"id":5`,
		`"uuid":"81816ebd-64de-4a55-815a-2d37471abf2e"`,
		`"project_id":8`,
		`"user_id":42`,
		`"meta":{"message":"Scheduled indexing completed"}`,
		`"created_at":"2026-07-31T16:30:00Z"`,
		`"updated_at":null`,
		`"event_type":"index_data_changed"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("SSE body %q does not contain %q", body, want)
		}
	}
}

func TestCurrentNotificationEventsResumesFromLastEventIDWithoutSnapshot(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	reader := &currentNotificationReaderStub{cancel: cancel}
	handler := currentNotificationEventsHandler{
		reader: reader,
		permissions: currentNotificationPermissionResolver{
			userID:      42,
			permissions: []string{CurrentNotificationEventsPermission},
		},
		admission: newCurrentNotificationAdmission(4, 2),
	}
	request := currentNotificationRequest(ctx, "7")
	request.Header.Set("Last-Event-ID", "19")
	response := newCurrentNotificationStreamingRecorder()

	handler.serve(response, request)

	if reader.highWaterCalls != 0 || len(reader.cursors) != 1 || reader.cursors[0] != 19 {
		t.Fatalf("high-water/list calls = %d/%v, want 0/[19]", reader.highWaterCalls, reader.cursors)
	}
	if strings.Contains(response.Body.String(), currentNotificationReadyEventName) {
		t.Fatalf("resume unexpectedly wrote a ready snapshot: %q", response.Body.String())
	}
}

func TestCurrentNotificationEventsAdvancesOversizedRowsThroughDurableResync(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	createdAt := time.Date(2026, time.July, 31, 16, 30, 0, 0, time.UTC)
	reader := &currentNotificationReaderStub{
		cancel: cancel,
		events: []notificationapp.Event{
			{
				Cursor:    5,
				UUID:      "81816ebd-64de-4a55-815a-2d37471abf2e",
				ProjectID: 7,
				UserID:    42,
				Meta: json.RawMessage(
					`{"message":"` + strings.Repeat("x", maxCurrentNotificationEventSize) + `"}`,
				),
				EventType: "index_data_changed",
				CreatedAt: createdAt,
			},
			{
				Cursor:    6,
				UUID:      "81816ebd-64de-4a55-815a-2d37471abf2f",
				ProjectID: 7,
				UserID:    42,
				Meta:      json.RawMessage(`{"message":"next"}`),
				EventType: "budget_threshold",
				CreatedAt: createdAt,
			},
		},
	}
	handler := currentNotificationEventsHandler{
		reader: reader,
		permissions: currentNotificationPermissionResolver{
			userID:      42,
			permissions: []string{CurrentNotificationEventsPermission},
		},
		admission: newCurrentNotificationAdmission(4, 2),
	}
	response := newCurrentNotificationStreamingRecorder()

	handler.serve(response, currentNotificationRequest(ctx, "7"))

	body := response.Body.String()
	if !strings.Contains(body, "id: 5\nevent: notifications_ready") {
		t.Fatalf("oversized row did not advance through durable resync: %q", body)
	}
	if strings.Contains(body, strings.Repeat("x", 1024)) {
		t.Fatal("oversized metadata leaked into the SSE response")
	}
	if !strings.Contains(body, "id: 6\nevent: notifications_notify") {
		t.Fatalf("event after oversized row was not delivered: %q", body)
	}
}

func TestCurrentNotificationEventsFailsClosed(t *testing.T) {
	tests := []struct {
		name       string
		request    func() *http.Request
		resolver   currentNotificationPermissionResolver
		wantStatus int
	}{
		{
			name: "missing authenticated provenance",
			request: func() *http.Request {
				request := httptest.NewRequest(http.MethodGet, "/", nil)
				return withCurrentNotificationProject(request, "7")
			},
			resolver: currentNotificationPermissionResolver{
				userID:      42,
				permissions: []string{CurrentNotificationEventsPermission},
			},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name: "permission missing",
			request: func() *http.Request {
				return currentNotificationRequest(context.Background(), "7")
			},
			resolver:   currentNotificationPermissionResolver{userID: 42},
			wantStatus: http.StatusForbidden,
		},
		{
			name: "permission database fails",
			request: func() *http.Request {
				return currentNotificationRequest(context.Background(), "7")
			},
			resolver: currentNotificationPermissionResolver{
				err: errors.New("database unavailable"),
			},
			wantStatus: http.StatusForbidden,
		},
		{
			name: "negative cursor",
			request: func() *http.Request {
				request := currentNotificationRequest(context.Background(), "7")
				request.Header.Set("Last-Event-ID", "-1")
				return request
			},
			resolver: currentNotificationPermissionResolver{
				userID:      42,
				permissions: []string{CurrentNotificationEventsPermission},
			},
			wantStatus: http.StatusBadRequest,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := currentNotificationEventsHandler{
				reader:      &currentNotificationReaderStub{},
				permissions: test.resolver,
				admission:   newCurrentNotificationAdmission(4, 2),
			}
			response := httptest.NewRecorder()
			handler.serve(response, test.request())
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, test.wantStatus)
			}
		})
	}
}

func TestCurrentNotificationAdmissionBoundsAndReleasesPerPrincipal(t *testing.T) {
	gate := newCurrentNotificationAdmission(2, 1)
	releaseOne, ok := gate.acquire("user-1")
	if !ok {
		t.Fatal("first user-1 stream was denied")
	}
	if _, ok := gate.acquire("user-1"); ok {
		t.Fatal("second user-1 stream exceeded the principal limit")
	}
	releaseTwo, ok := gate.acquire("user-2")
	if !ok {
		t.Fatal("user-2 stream was denied before the global limit")
	}
	if _, ok := gate.acquire("user-3"); ok {
		t.Fatal("user-3 stream exceeded the global limit")
	}
	releaseOne()
	releaseOne()
	if releaseThree, ok := gate.acquire("user-3"); !ok {
		t.Fatal("released capacity was not reclaimed")
	} else {
		releaseThree()
	}
	releaseTwo()
}

func currentNotificationRequest(ctx context.Context, projectID string) *http.Request {
	request := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
	request = withCurrentNotificationProject(request, projectID)
	request = request.WithContext(auth.ContextWithAuthenticatedUser(
		request.Context(),
		auth.User{ID: "42", UserID: "42"},
		auth.AuthenticationSourceSession,
	))
	return request
}

func withCurrentNotificationProject(request *http.Request, projectID string) *http.Request {
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("projectID", projectID)
	return request.WithContext(context.WithValue(
		request.Context(),
		chi.RouteCtxKey,
		routeContext,
	))
}
