package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

type recordingInternalNotificationHandler struct {
	request   *http.Request
	operation string
	calls     int
}

func (handler *recordingInternalNotificationHandler) List(writer http.ResponseWriter, request *http.Request) {
	handler.record(writer, request, "list")
}

func (handler *recordingInternalNotificationHandler) Details(writer http.ResponseWriter, request *http.Request) {
	handler.record(writer, request, "details")
}

func (handler *recordingInternalNotificationHandler) MarkSeen(writer http.ResponseWriter, request *http.Request) {
	handler.record(writer, request, "mark")
}

func (handler *recordingInternalNotificationHandler) record(
	writer http.ResponseWriter,
	request *http.Request,
	operation string,
) {
	handler.calls++
	handler.request = request
	handler.operation = operation
	writer.Header().Set("Content-Type", "application/json")
	_, _ = writer.Write([]byte(`{"ok":true}`))
}

func TestInternalNotificationListProjectsBoundedQueryAndAuthenticatedActor(t *testing.T) {
	handler := &recordingInternalNotificationHandler{}
	executor := newHandlerInternalNotificationExecutor(handler)
	result, err := executor.Execute(context.Background(), 7, 41, internalListNotifications, map[string]any{
		"limit": json.Number("25"), "offset": json.Number("3"),
		"sort_by": "event_type", "sort_order": "asc",
		"only_new": true, "only_total": true,
		"search": "Budget 100%_safe", "event_type": "budget_threshold",
	})
	if err != nil {
		t.Fatalf("list notifications: %v", err)
	}
	if result.status != http.StatusOK || handler.calls != 1 || handler.operation != "list" ||
		handler.request.Method != http.MethodGet || string(result.body) != `{"ok":true}` {
		t.Fatalf("result=%+v calls=%d operation=%q request=%v", result, handler.calls, handler.operation, handler.request)
	}
	for key, want := range map[string]string{
		"limit": "25", "offset": "3", "sort_by": "event_type", "sort_order": "asc",
		"only_new": "true", "only_total": "true", "search": "Budget 100%_safe",
		"event_type": "budget_threshold",
	} {
		if got := handler.request.URL.Query().Get(key); got != want {
			t.Fatalf("query %s = %q, want %q", key, got, want)
		}
	}
	user, ok := auth.UserFromContext(handler.request.Context())
	if !ok || user.UserID != "41" {
		t.Fatalf("request actor = %+v/%v, want user 41", user, ok)
	}
	if got := chi.URLParam(handler.request, "projectID"); got != "7" {
		t.Fatalf("request project = %q, want 7", got)
	}
}

func TestInternalNotificationListSuppliesCurrentDefaults(t *testing.T) {
	handler := &recordingInternalNotificationHandler{}
	executor := newHandlerInternalNotificationExecutor(handler)
	result, err := executor.Execute(context.Background(), 7, 41, internalListNotifications, nil)
	if err != nil || result.status != http.StatusOK {
		t.Fatalf("default list: status=%d error=%v body=%s", result.status, err, result.body)
	}
	query := handler.request.URL.Query()
	if query.Get("limit") != "10" || query.Get("offset") != "0" || query.Get("sort_by") != "" ||
		query.Get("sort_order") != "" {
		t.Fatalf("default query = %v", query)
	}
}

func TestInternalNotificationDetailsAndMarkUseOnlyThePathIdentity(t *testing.T) {
	for _, test := range []struct {
		operation internalNotificationOperation
		method    string
		name      string
	}{
		{internalGetNotification, http.MethodGet, "details"},
		{internalMarkNotification, http.MethodPut, "mark"},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := &recordingInternalNotificationHandler{}
			executor := newHandlerInternalNotificationExecutor(handler)
			result, err := executor.Execute(context.Background(), 7, 41, test.operation, map[string]any{
				"notification_id": json.Number("19"), "id": json.Number("999"),
			})
			if err != nil || result.status != http.StatusOK || handler.calls != 1 ||
				handler.operation != test.name || handler.request.Method != test.method {
				t.Fatalf("status=%d error=%v calls=%d operation=%q method=%q",
					result.status, err, handler.calls, handler.operation, handler.request.Method)
			}
			if got := chi.URLParam(handler.request, "notificationID"); got != "19" {
				t.Fatalf("notification path ID = %q, want 19", got)
			}
		})
	}
}

func TestInternalNotificationValidationStopsBeforeHandlerInvocation(t *testing.T) {
	longEvent := strings.Repeat("e", internalNotificationMaxEventBytes+1)
	longWord := strings.Repeat("w", internalNotificationMaxWordBytes+1)
	manyWords := make([]string, internalNotificationMaxSearchWords+1)
	for index := range manyWords {
		manyWords[index] = strconv.Itoa(index)
	}
	tests := []struct {
		name      string
		operation internalNotificationOperation
		arguments map[string]any
	}{
		{"limit zero", internalListNotifications, map[string]any{"limit": json.Number("0")}},
		{"limit high", internalListNotifications, map[string]any{"limit": json.Number("1001")}},
		{"offset negative", internalListNotifications, map[string]any{"offset": json.Number("-1")}},
		{"boolean type", internalListNotifications, map[string]any{"only_new": "true"}},
		{"sort field", internalListNotifications, map[string]any{"sort_by": "DROP TABLE"}},
		{"sort order", internalListNotifications, map[string]any{"sort_order": "sideways"}},
		{"event length", internalListNotifications, map[string]any{"event_type": longEvent}},
		{"search type", internalListNotifications, map[string]any{"search": true}},
		{"search words", internalListNotifications, map[string]any{"search": strings.Join(manyWords, " ")}},
		{"search word length", internalListNotifications, map[string]any{"search": longWord}},
		{"missing id", internalGetNotification, map[string]any{}},
		{"invalid id", internalMarkNotification, map[string]any{"notification_id": json.Number("0")}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := &recordingInternalNotificationHandler{}
			executor := newHandlerInternalNotificationExecutor(handler)
			result, err := executor.Execute(context.Background(), 7, 41, test.operation, test.arguments)
			if err != nil {
				t.Fatalf("validate input: %v", err)
			}
			if result.status != http.StatusBadRequest || handler.calls != 0 {
				t.Fatalf("status=%d calls=%d body=%s", result.status, handler.calls, result.body)
			}
		})
	}
}
