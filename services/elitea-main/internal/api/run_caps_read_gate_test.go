package api

// The tier run caps read (issue 3137) is gated on the agent read permission:
// a caller who may read the agent gets past the gate (the database-less
// router then answers 503), a member who may not is refused with 403.

import (
	"net/http"
	"testing"
)

func TestRunCapsReadIsGatedOnTheAgentReadPermission(t *testing.T) {
	const path = "/api/v2/elitea_core/run_caps/prompt_lib/7/0"
	reader := newModeRouter(t, modeResolver{member: []string{"models.applications.application.details"}})
	if got := servePlatformGate(t, reader, http.MethodGet, path, "").Code; got == http.StatusForbidden {
		t.Errorf("holder of models.applications.application.details: GET %s = 403, want past the gate", path)
	}
	other := newModeRouter(t, modeResolver{member: []string{"models.applications.applications.create"}})
	if got := servePlatformGate(t, other, http.MethodGet, path, "").Code; got != http.StatusForbidden {
		t.Errorf("member without the read permission: GET %s = %d, want 403", path, got)
	}
}
