package agentrelease

import (
	"errors"
	"net/http"
)

// NotFoundCode is the typed refusal of a run that names a release the
// application does not have.
const NotFoundCode = "release_not_found"

// DriftMessage is the caller-facing sentence of a DriftCode refusal.
const DriftMessage = "A toolkit or configuration this release pinned changed after it was published. " +
	"Publish a new release, or roll back to a release whose toolkits still match."

// HTTPRefusal maps a refused release run to its HTTP answer: 409 with the
// typed release_toolkit_drifted body, 404 release_not_found. ok is false for
// every other error. The body names the toolkit id and the reason, never a
// setting or a configuration value.
func HTTPRefusal(err error) (status int, body map[string]any, ok bool) {
	var drift *DriftError
	if errors.As(err, &drift) {
		body := map[string]any{
			"error":   DriftCode,
			"code":    DriftCode,
			"reason":  drift.Reason,
			"message": DriftMessage,
		}
		if drift.ToolkitID > 0 {
			body["toolkit_id"] = drift.ToolkitID
		}
		return http.StatusConflict, body, true
	}
	if errors.Is(err, ErrNotFound) {
		return http.StatusNotFound, map[string]any{
			"error":   NotFoundCode,
			"code":    NotFoundCode,
			"message": "The release is not a release of this agent.",
		}, true
	}
	return 0, nil, false
}
