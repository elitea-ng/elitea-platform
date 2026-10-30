package applications

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workerruncaps"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// WithRunCapsChecker makes the version saves refuse an author's run caps
// (meta.run_caps, ADR-0041 decision 3) that raise the platform's cap for the
// agent's risk tier. Nil (every composition without the platform section)
// accepts any meta.run_caps; the runtime still takes the lower of the two.
func (h *Handler) WithRunCapsChecker(checker *workerruncaps.Checker) *Handler {
	h.runCaps = checker
	return h
}

// runCapsRefusal is the typed 422 body of a refused author cap.
type runCapsRefusal struct {
	Error     string  `json:"error"`
	Code      string  `json:"code"`
	Cap       string  `json:"cap"`
	Tier      string  `json:"tier,omitempty"`
	TierCap   float64 `json:"tier_cap,omitempty"`
	Requested float64 `json:"requested,omitempty"`
}

// checkRunCaps checks body.meta.run_caps of a version save and writes the
// refusal. It answers false when it wrote a response. applicationID is ""
// on a create (the agent's tier is then the tier of a new agent, low).
func (h *Handler) checkRunCaps(w http.ResponseWriter, r *http.Request, projectID, applicationID string, body map[string]any) bool {
	if h.runCaps == nil || body == nil {
		return true
	}
	meta, _ := body["meta"].(map[string]any)
	if meta == nil {
		return true
	}
	project, _ := strconv.ParseInt(projectID, 10, 64)
	application, _ := strconv.ParseInt(applicationID, 10, 64)
	err := h.runCaps.Check(r.Context(), project, application, meta)
	if err == nil {
		return true
	}
	var refusal *workerruncaps.CapRaiseError
	if errors.As(err, &refusal) {
		writeJSON(w, http.StatusUnprocessableEntity, runCapsRefusal{
			Error:     refusal.Error(),
			Code:      refusal.Code,
			Cap:       refusal.Cap,
			Tier:      string(refusal.Tier),
			TierCap:   refusal.TierCap,
			Requested: refusal.Requested,
		})
		return false
	}
	slog.ErrorContext(r.Context(), "check the agent's run caps", "project_id", projectID,
		"application_id", applicationID, "err", err)
	apierr.Write(w, apierr.Internal("the platform run caps could not be read"))
	return false
}
