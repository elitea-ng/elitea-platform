package httperr

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/libs/go/faults"
)

// C2: the body is the sentence, the code and the support reference only.
// The cause, a panic value or a stack never reach the client; the log line
// carries them.
func TestTheBodyCarriesTheSentenceCodeAndSupportRefOnly(t *testing.T) {
	cases := map[string]struct {
		status int
		fault  *faults.Error
	}{
		"expected":   {http.StatusServiceUnavailable, faults.New(faults.ToolkitSettingsUnresolved, errors.New("secret-cause: dsn postgres://u:pw@h/db"))},
		"unexpected": {http.StatusInternalServerError, faults.Unexpected(errors.New("secret-cause: nil map in resolver"))},
		"panic":      {http.StatusInternalServerError, faults.Panic("secret-cause: token=abc")},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var logs bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&logs, nil))
			request := httptest.NewRequest(http.MethodGet, "/x", nil)
			request = request.WithContext(faults.WithSupportRef(request.Context(), "sr-00112233445566ff"))
			recorder := httptest.NewRecorder()
			Write(recorder, request, logger, tc.status, tc.fault)

			if recorder.Code != tc.status || recorder.Header().Get(SupportRefHeader) != "sr-00112233445566ff" {
				t.Fatalf("status %d header %q", recorder.Code, recorder.Header().Get(SupportRefHeader))
			}
			var body map[string]any
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if len(body) != 3 || body["error"] != tc.fault.Code().Sentence() || body["code"] != tc.fault.Code().String() || body["support_ref"] != "sr-00112233445566ff" {
				t.Fatalf("body = %v", body)
			}
			for _, leak := range []string{"secret-cause", "goroutine", ".go:", "postgres://"} {
				if strings.Contains(recorder.Body.String(), leak) {
					t.Fatalf("the body carries %q: %s", leak, recorder.Body.String())
				}
			}
			if !strings.Contains(logs.String(), tc.fault.Code().String()) {
				t.Fatalf("nothing was logged: %q", logs.String())
			}
		})
	}
}
