package desktopops

import (
	"errors"
	"net/http"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/desktopwire"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/safety"
)

// writeSafetyRefusal answers a start the kill switch refused (ADR-0041
// decision 1) in the desktop wire shape: 409 execution_suspended for a
// covered run, 503 suspension_state_unavailable when the suspension state
// could not be read. It reports false for every other error.
func writeSafetyRefusal(writer http.ResponseWriter, err error) bool {
	if suspended, ok := safety.AsSuspended(err); ok {
		desktopwire.WriteError(writer, http.StatusConflict, safety.ErrorCode, suspended.Message())
		return true
	}
	if errors.Is(err, safety.ErrStateUnavailable) {
		writer.Header().Set("Retry-After", "1")
		desktopwire.WriteError(writer, http.StatusServiceUnavailable, "suspension_state_unavailable",
			"The platform cannot confirm that execution is allowed right now. Try again shortly.")
		return true
	}
	return false
}
