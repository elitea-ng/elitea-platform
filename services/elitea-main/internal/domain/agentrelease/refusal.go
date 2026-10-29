package agentrelease

import (
	"errors"
	"net/http"
)

// NotFoundCode is the typed refusal of a run that names a release the
// application does not have.
const NotFoundCode = "release_not_found"

// WithdrawnDependenciesRemovedCode is the typed refusal of a run of a
// withdrawn release whose embedded sub-agent copies the unpublish removed
// (ErrWithdrawnDependenciesRemoved).
const WithdrawnDependenciesRemovedCode = "release_withdrawn_dependencies_removed"

// DriftMessage is the caller-facing sentence of a DriftCode refusal.
const DriftMessage = "A toolkit or configuration this release pinned changed after it was published. " +
	"Publish a new release, or roll back to a release whose toolkits still match."

// HTTPRefusal maps a refused release run to its HTTP answer: 409 with the
// typed release_toolkit_drifted body, 409
// release_withdrawn_dependencies_removed, 404 release_not_found. ok is false for
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
	if errors.Is(err, ErrWithdrawnDependenciesRemoved) {
		return http.StatusConflict, map[string]any{
			"error": WithdrawnDependenciesRemovedCode,
			"code":  WithdrawnDependenciesRemovedCode,
			"message": "This release was withdrawn, and the unpublish removed the copies of the sub-agents it ran. " +
				"It can no longer run. Run the agent's current release, or publish the agent again.",
		}, true
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
