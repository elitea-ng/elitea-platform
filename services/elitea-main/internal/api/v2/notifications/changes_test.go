package notifications

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/changesync"
	notificationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/notifications"
)

type changesStoreStub struct {
	currentNotificationStoreStub
	raw   string
	limit int
	page  notificationapp.Changes
	cerr  error
}

func (stub *changesStoreStub) ListChanges(_ context.Context, userID int64, raw string, limit int) (notificationapp.Changes, error) {
	stub.userID, stub.raw, stub.limit = userID, raw, limit
	return stub.page, stub.cerr
}

func TestCurrentNotificationDeltaKeepsTheRowShapeAndAddsTheCursor(t *testing.T) {
	uuid := "75cd2484-bbd0-46e7-b607-92b8df784cab"
	store := &changesStoreStub{
		currentNotificationStoreStub: currentNotificationStoreStub{count: 4},
		page: notificationapp.Changes{
			Rows: []notificationapp.Notification{{
				ID: 15, UUID: uuid, ProjectID: 2, UserID: 42, Meta: json.RawMessage(`{}`),
				EventType: "x", CreatedAt: time.Date(2026, 7, 31, 17, 50, 0, 0, time.UTC),
			}},
			Tombstones: []changesync.Tombstone{{ID: 9, UUID: &uuid, Reason: "deleted", DeletedAt: "2026-07-31T17:51:00Z"}},
			NextCursor: "next",
			HasMore:    true,
		},
	}
	handler := &currentNotificationAPIHandler{store: store}
	response := httptest.NewRecorder()
	handler.list(response, currentNotificationAPIRequest(http.MethodGet, "/?changes_since=abc&limit=7", nil, ""))
	if response.Code != http.StatusOK || store.raw != "abc" || store.limit != 7 || store.userID != 42 {
		t.Fatalf("status=%d raw=%q limit=%d user=%d body=%s", response.Code, store.raw, store.limit, store.userID, response.Body)
	}
	body := response.Body.String()
	for _, want := range []string{`"total":4`, `"id":15`, `"created_at":"2026-07-31T17:50:00Z"`,
		`"tombstones":[{"id":9`, `"next_cursor":"next"`, `"has_more":true`} {
		if !strings.Contains(body, want) {
			t.Fatalf("delta body %s missing %s", body, want)
		}
	}
}

func TestCurrentNotificationDeltaRefusals(t *testing.T) {
	cases := map[string]struct {
		query  string
		err    error
		status int
		code   string
	}{
		"filter":  {"/?changes_since=&only_new=true", nil, http.StatusBadRequest, "invalid_sync_request"},
		"offset":  {"/?changes_since=&offset=3", nil, http.StatusBadRequest, "invalid_sync_request"},
		"limit":   {"/?changes_since=&limit=0", nil, http.StatusBadRequest, "invalid_limit"},
		"invalid": {"/?changes_since=zzz", changesync.ErrInvalidCursor, http.StatusBadRequest, changesync.CodeInvalidCursor},
		"expired": {"/?changes_since=zzz", changesync.ErrCursorExpired, http.StatusGone, changesync.CodeCursorExpired},
	}
	for name, tc := range cases {
		store := &changesStoreStub{cerr: tc.err}
		handler := &currentNotificationAPIHandler{store: store}
		response := httptest.NewRecorder()
		handler.list(response, currentNotificationAPIRequest(http.MethodGet, tc.query, nil, ""))
		if response.Code != tc.status || !strings.Contains(response.Body.String(), `"error":"`+tc.code+`"`) {
			t.Errorf("%s: status=%d body=%s, want %d %s", name, response.Code, response.Body, tc.status, tc.code)
		}
	}

	// A store without the delta half answers 501, never the legacy page.
	plain := &currentNotificationStoreStub{}
	response := httptest.NewRecorder()
	(&currentNotificationAPIHandler{store: plain}).list(response, currentNotificationAPIRequest(http.MethodGet, "/?changes_since=", nil, ""))
	if response.Code != http.StatusNotImplemented || plain.operation != "" {
		t.Fatalf("store without ListChanges: status=%d operation=%q", response.Code, plain.operation)
	}
}
