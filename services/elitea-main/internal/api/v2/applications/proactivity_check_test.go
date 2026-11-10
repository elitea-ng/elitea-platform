package applications

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ADR-0050 decision 2 (#1618): a version save with an invalid proactivity
// block is a typed 422; a valid one passes.
func TestCheckProactivity(t *testing.T) {
	cases := map[string]struct {
		block map[string]any
		ok    bool
	}{
		"valid":              {map[string]any{"mode": "heartbeat", "interval": "30m", "quiet_hours": "22:00-07:00 Europe/Berlin"}, true},
		"interval too short": {map[string]any{"mode": "heartbeat", "interval": "5m"}, false},
		"workspace thread":   {map[string]any{"mode": "heartbeat", "act_target": "workspace_thread:abc"}, false},
		"unknown key":        {map[string]any{"mode": "heartbeat", "cron": "* * * * *"}, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			got := checkProactivity(recorder, map[string]any{"meta": map[string]any{"proactivity": tc.block}})
			if got != tc.ok {
				t.Fatalf("checkProactivity = %v, want %v (%s)", got, tc.ok, recorder.Body.String())
			}
			if !tc.ok && (recorder.Code != http.StatusUnprocessableEntity ||
				!strings.Contains(recorder.Body.String(), `"proactivity_invalid"`)) {
				t.Fatalf("refusal = %d %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}
