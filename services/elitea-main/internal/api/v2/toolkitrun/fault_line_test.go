package toolkitrun

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/libs/go/faults"
	"github.com/elitea-ng/elitea-platform/libs/go/faults/logx"
	toolkitcalltoolapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/toolkitcalltool"
)

// FU-300 / FU-299: the tool list that failed with a silent 503 or 500 now
// logs exactly one fault line, whose support_ref is the one the body and
// the header carry, and the body never carries the cause.
func TestAFailedToolListIsLoggedOnceWithTheSupportReferenceItReturns(t *testing.T) {
	cases := map[string]struct {
		err        error
		wantStatus int
		wantCode   string
		wantStack  bool
	}{
		"settings not resolved": {fmt.Errorf("resolve toolkit 104: %w", toolkitcalltoolapp.ErrToolkitSettingsResolutionUnavailable), http.StatusServiceUnavailable, "toolkit.settings_unresolved", false},
		"anything else":         {errors.New("dial tcp 10.0.0.5:5432: connection refused"), http.StatusInternalServerError, "internal.unexpected", true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(logx.New(&logs, logx.Options{}))
			defer slog.SetDefault(previous)

			request := httptest.NewRequest(http.MethodGet, "/api/v2/elitea_core/toolkit_available_tools/prompt_lib/2/104", nil)
			request = request.WithContext(faults.WithSupportRef(request.Context(), "sr-a1b2c3d4e5f60718"))
			recorder := httptest.NewRecorder()
			WriteRequestError(recorder, request, tc.err)

			if recorder.Code != tc.wantStatus || recorder.Header().Get("X-Elitea-Support-Ref") != "sr-a1b2c3d4e5f60718" {
				t.Fatalf("answer %d, header %q", recorder.Code, recorder.Header().Get("X-Elitea-Support-Ref"))
			}
			var body map[string]any
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body["ok"] != false || body["code"] != tc.wantCode || body["support_ref"] != "sr-a1b2c3d4e5f60718" || len(body) != 4 {
				t.Fatalf("body = %v", body)
			}
			for _, leak := range []string{"10.0.0.5", "toolkit 104", "resolution is unavailable", ".go:"} {
				if strings.Contains(recorder.Body.String(), leak) {
					t.Fatalf("the body carries the cause (%q): %s", leak, recorder.Body.String())
				}
			}

			lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
			matching := 0
			for _, raw := range lines {
				var line map[string]any
				if err := json.Unmarshal([]byte(raw), &line); err != nil {
					t.Fatal(err)
				}
				if line["support_ref"] != "sr-a1b2c3d4e5f60718" {
					continue
				}
				matching++
				if line["code"] != tc.wantCode || line["msg"] != body["error"] || line["level"] != "ERROR" {
					t.Fatalf("log line = %v", line)
				}
				if _, hasStack := line["stack"]; hasStack != tc.wantStack {
					t.Fatalf("stack present = %v, want %v", hasStack, tc.wantStack)
				}
				if cause, _ := line["cause"].([]any); len(cause) == 0 {
					t.Fatalf("the log line has no cause chain: %v", line)
				}
			}
			if matching != 1 {
				t.Fatalf("log lines with the support reference = %d, want exactly 1:\n%s", matching, logs.String())
			}
		})
	}
}
