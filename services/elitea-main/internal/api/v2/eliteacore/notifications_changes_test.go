package eliteacore

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The fallback notification list has no delta. It must refuse changes_since
// rather than serve a full page a syncing client would read as "nothing was
// deleted" (ADR-0025 WP6).
func TestFallbackNotificationsRefusesChangesSince(t *testing.T) {
	response := httptest.NewRecorder()
	(&Handler{}).Notifications(response, httptest.NewRequest(http.MethodGet, "/?changes_since=", nil))
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "invalid_sync_request") {
		t.Fatalf("status=%d body=%s, want 400 invalid_sync_request", response.Code, response.Body)
	}
}
