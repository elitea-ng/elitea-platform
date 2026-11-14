package runevidence

// notices.go — the guardrail notices of a run, for the chat (ADR-0043
// decision 1, issue 2012).
//
//	GET /guardrail_notices/prompt_lib/{projectID}/{runID}
//	    what the LLM gateway's guardrails did to the run's requests that the
//	    person must be told: blocked, redacted, warned, or a closed guardrail
//	    that could not run.
//
// The source is gateway.guardrail_verdicts, the table behind the run evidence
// read, so no hop is new: elitea-main already reads it. A notice names the
// stage, the action, the outcome, the guardrail (the governance entry's name)
// and the finding labels (rule ids such as `email`). It has no field that the
// inspected or the matched text can reach: the table has no such column.
//
// The route takes the permission that sends a chat message
// (models.chat.messages.create), not runs.evidence.read: a person who chats
// must learn why a message was refused, and the run evidence read is an audit
// permission. The notice is a summary of the verdict, with no step content.
// The gateway writes verdicts with its request log, up to one second after
// the request, so a client that asks at the end of a turn asks again once.

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// NoticesPermission gates the notices route.
const NoticesPermission = "models.chat.messages.create"

// NoticesPath is the route pattern, relative to the /api/v2/elitea_core group.
const NoticesPath = "/guardrail_notices/prompt_lib/{projectID}/{runID}"

// maxNoticeVerdicts bounds the aggregated groups read for one run.
const maxNoticeVerdicts = 200

// Notice kinds.
const (
	NoticeBlocked     = "blocked"
	NoticeRedacted    = "redacted"
	NoticeWarned      = "warned"
	NoticeUnavailable = "unavailable"
)

// Notice is one guardrail's effect on a run, merged over the run's requests.
type Notice struct {
	// Kind is blocked, redacted, warned or unavailable.
	Kind string `json:"kind"`
	// Stage is input (the request to the model) or output (the answer).
	Stage string `json:"stage"`
	// Action is the detector's configured action: block, mask or warn. A
	// closed guardrail that failed reports the action it was set to.
	Action string `json:"action"`
	// Guardrail is the name of the governance entry.
	Guardrail string `json:"guardrail"`
	Detector  string `json:"detector"`
	// Labels are the finding labels, for example `email`. Never a value.
	Labels []string `json:"labels"`
	// Requests is the number of the run's LLM requests the notice covers.
	Requests    int       `json:"requests"`
	FirstSeenAt time.Time `json:"first_seen_at"`
}

// NoticesResponse is the notices response.
type NoticesResponse struct {
	RunID   string   `json:"run_id"`
	Notices []Notice `json:"notices"`
}

func noticeKind(outcome string) (string, bool) {
	switch outcome {
	case "blocked":
		return NoticeBlocked, true
	case "masked":
		return NoticeRedacted, true
	case "warned":
		return NoticeWarned, true
	case "error_closed":
		return NoticeUnavailable, true
	}
	return "", false
}

// mergeNotices merges the verdicts of a run into one notice per (stage,
// guardrail, detector, kind), with the union of the labels, in first-seen
// order.
func mergeNotices(verdicts []repos.GuardrailNoticeVerdict) []Notice {
	type key struct{ stage, row, detector, kind string }
	index := map[key]int{}
	notices := []Notice{}
	for _, v := range verdicts {
		kind, ok := noticeKind(v.Outcome)
		if !ok {
			continue
		}
		k := key{v.Stage, v.Row, v.Detector, kind}
		i, seen := index[k]
		if !seen {
			i = len(notices)
			index[k] = i
			notices = append(notices, Notice{
				Kind: kind, Stage: v.Stage, Action: v.Action, Guardrail: v.Row, Detector: v.Detector,
				Labels: []string{}, FirstSeenAt: v.OccurredAt,
			})
		}
		n := &notices[i]
		n.Requests += max(v.Requests, 1)
		for _, label := range v.Labels {
			if !strings.HasPrefix(label, "slot:") && !containsLabel(n.Labels, label) {
				n.Labels = append(n.Labels, label)
			}
		}
	}
	for i := range notices {
		sort.Strings(notices[i].Labels)
	}
	return notices
}

func containsLabel(labels []string, s string) bool {
	for _, l := range labels {
		if l == s {
			return true
		}
	}
	return false
}

// GuardrailNotices answers the run's notices. A run with none, and a run that
// does not exist, answer an empty list.
func (h *Handler) GuardrailNotices(w http.ResponseWriter, r *http.Request) {
	projectID, runID, ok := h.run(w, r)
	if !ok {
		return
	}
	// A member reads the notices of their own runs only: the verdict rows carry
	// the user of the request, and the read is scoped to the caller.
	principal, _ := auth.UserFromContext(r.Context())
	userID, ok := principal.OwningUserID()
	if !ok || userID <= 0 {
		apierr.WriteStatus(w, http.StatusUnauthorized, "reading guardrail notices needs a signed-in user")
		return
	}
	verdicts, err := h.store.GuardrailNoticeVerdicts(r.Context(), projectID, userID, runID, maxNoticeVerdicts)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	writeJSON(w, http.StatusOK, NoticesResponse{RunID: runID, Notices: mergeNotices(verdicts)})
}
