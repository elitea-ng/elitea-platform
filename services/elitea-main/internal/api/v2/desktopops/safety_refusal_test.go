package desktopops

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/safety"
)

// A desktop remote toolkit call and a desktop child turn the kill switch
// refused (ADR-0041 decision 1) answer 409 execution_suspended, or 503 when
// the suspension state could not be read, never the 500 of a failed call.
func TestDesktopRoutesMapSafetyRefusals(t *testing.T) {
	t.Parallel()
	h := &remoteToolkitHandler{}
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{fmt.Errorf("admit: %w", &safety.SuspendedError{SuspensionID: 5, Scope: safety.ScopePlatform}),
			http.StatusConflict, safety.ErrorCode},
		{fmt.Errorf("admit: %w: db down", safety.ErrStateUnavailable),
			http.StatusServiceUnavailable, "suspension_state_unavailable"},
	} {
		for name, write := range map[string]func(http.ResponseWriter, error){
			"remote toolkit": func(w http.ResponseWriter, err error) { h.writeRunError(context.Background(), w, 9, err) },
			"child turn":     func(w http.ResponseWriter, err error) { writeChildStartError(context.Background(), w, err) },
		} {
			rec := httptest.NewRecorder()
			write(rec, tc.err)
			if rec.Code != tc.status || !strings.Contains(rec.Body.String(), tc.code) {
				t.Errorf("%s %v: status %d body %s; want %d %s", name, tc.err, rec.Code, rec.Body, tc.status, tc.code)
			}
		}
	}
	if writeSafetyRefusal(httptest.NewRecorder(), errors.New("other")) {
		t.Fatal("an unrelated error was answered as a refusal")
	}
}
