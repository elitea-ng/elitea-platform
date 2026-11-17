package middleware

import (
	"net/http"
	"strings"
)

// delegationDriftSurfaces are the path fragments of the routes that can change
// what a callee agent holds: the agent and its versions, their toolkits and
// tools, the registry (data classes), credential references, publishing, and
// the Marketplace hire, binding, upgrade and merge. A write under one of them
// can widen the set of a delegation grant's callee (ADR-0044 amendment 1,
// A1.2 rule 3), so the drift check runs after it.
//
// The list is broad on purpose, minus the routes that only test or validate
// (delegationDriftReadOnlyWrites). The check is debounced per project and reads
// the project's approved grants; a fragment that is missing costs a late
// detection, which the check on reading the caller's grants makes good.
var delegationDriftSurfaces = []string{
	"application", "toolkit", "/tool", "/version", "publish", "marketplace_", "agent_registry",
	"change_requests", "credential_reference", "/mcp", "pipeline",
}

// delegationDriftReadOnlyWrites are the POST routes under those fragments that
// test, validate, discover or stream and change no callee.
var delegationDriftReadOnlyWrites = []string{
	"test_", "validator", "validate", "discover", "generate_", "_stream", "tools_call", "predict", "check_",
}

// DelegationDriftSurfaces returns a copy of the fragments, so a router-level
// test can assert that each still matches a mounted write route.
func DelegationDriftSurfaces() []string {
	return append([]string(nil), delegationDriftSurfaces...)
}

func driftSurface(path string) bool {
	if !strings.HasPrefix(path, "/api/v2/") {
		return false
	}
	// The delegation grant routes themselves never change a callee.
	if strings.Contains(path, "/delegation_grant") || strings.Contains(path, "/delegation_preview") {
		return false
	}
	// Routes that run, test or validate something and write nothing a callee holds.
	for _, fragment := range delegationDriftReadOnlyWrites {
		if strings.Contains(path, fragment) {
			return false
		}
	}
	for _, fragment := range delegationDriftSurfaces {
		if strings.Contains(path, fragment) {
			return true
		}
	}
	return false
}

// DelegationDrift asks for a drift check of the request's project after a
// successful write to a route that can change a callee agent. trigger is
// Trigger.ProjectChanged; nil returns next unchanged. It must sit below the
// route resolution that sets the project (like Audit) and never changes the
// response: the check runs later, in the background.
func DelegationDrift(trigger func(projectID int64)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if trigger == nil {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !mutatingMethod(r.Method) || !driftSurface(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			recording := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(recording, r)
			if recording.status >= http.StatusBadRequest {
				return
			}
			if projectID, ok := requestProjectID(r); ok {
				trigger(projectID)
			}
		})
	}
}
