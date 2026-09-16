package admin

// Admin › Tasks — run the personal-access-token expiry notice pass now
// (issue #940 A3).
//
// # Why this route exists
//
// The pass runs on the platform scheduler's cadence
// (runtimecomposition/pat_expiry_sweep.go). An operator who has just changed
// something about a fleet of keys — or who is diagnosing "my users say they
// were never warned" — has no way to ask the platform to do it now, and
// waiting a cadence to find out is not diagnosis. It sits on the Tasks
// surface, beside the listing of what is running, because that is the page
// that question belongs to.
//
// It runs the SAME patexpiry.Notifier the scheduler runs. That is the point:
// an operator "run now" taking a different path from the scheduled one proves
// nothing about the scheduled one, and a journey driving it would be measuring
// the wrong code.
//
// # The permission
//
// `runtime.plugins` in administration mode — the gate the whole Tasks surface
// already carries (background_jobs.go's header). No new permission string, so
// no new grant migration and no operator to re-authorise.
//
// # `within`
//
// The optional `within` query parameter is the look-ahead, capped at 90 days.
// Production is 24 hours and an omitted parameter means exactly that. It is a
// real operator control ("tell me who WOULD be warned in the next week") and
// it is also what makes the rule testable: a token minted through the API
// cannot be both "expiring within a day" and "older than a day", so a test
// widens the window rather than back-dating a row. Widening never weakens the
// total-lifetime rule — that predicate scales with the same value (see
// repos.patExpiryCandidateQuery).

import (
	"context"
	"net/http"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/patexpiry"
)

// PATExpiryNotifier is the one method this route needs.
// *patexpiry.Notifier satisfies it.
type PATExpiryNotifier interface {
	Run(ctx context.Context, now time.Time, within time.Duration) (patexpiry.Result, error)
}

// maxPATExpiryWindow bounds `within`. Past this the pass stops being "who is
// about to lose a key" and becomes a table scan of every token that will ever
// expire.
const maxPATExpiryWindow = 90 * 24 * time.Hour

// WithPATExpiryNotifier supplies the producer. Unassigned, the route answers
// 503 rather than 200-with-zero-produced: "this deployment cannot run the
// pass" and "the pass ran and nobody was due" must not render identically,
// which is the same correction background_jobs.go records for its own store.
func WithPATExpiryNotifier(notifier PATExpiryNotifier) Option {
	return func(h *Handler) { h.patExpiryNotifier = notifier }
}

// RunPATExpiryNotices performs one pass and reports what it did.
func (h *Handler) RunPATExpiryNotices(w http.ResponseWriter, r *http.Request) {
	if h.patExpiryNotifier == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "this deployment has no personal-access-token expiry producer configured",
			"code":  "pat_expiry_notifier_unavailable",
		})
		return
	}

	within, ok := parsePATExpiryWindow(r.URL.Query().Get("within"))
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "within must be a Go duration no longer than 90 days, for example 24h",
			"code":  "pat_expiry_window_invalid",
		})
		return
	}

	result, err := h.patExpiryNotifier.Run(r.Context(), time.Now().UTC(), within)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": "the personal-access-token expiry pass failed",
			"code":  "pat_expiry_run_failed",
		})
		return
	}

	// `examined`, `produced` and `skipped` are reported separately because
	// they answer different questions. A second run that examines nothing and
	// produces nothing is the dedupe holding, and it is indistinguishable from
	// a broken pass if only one number comes back.
	writeJSON(w, http.StatusOK, map[string]any{
		"examined": result.Examined,
		"produced": result.Produced,
		"skipped":  result.Skipped,
	})
}

// parsePATExpiryWindow reads the optional `within` parameter. An empty value
// means the production window; anything unparseable, zero, negative or past
// the cap is refused rather than silently clamped — a caller who asked for a
// window they did not get would read the result as an answer about the window
// they named.
func parsePATExpiryWindow(raw string) (time.Duration, bool) {
	if raw == "" {
		return patexpiry.DefaultWindow, true
	}
	within, err := time.ParseDuration(raw)
	if err != nil || within <= 0 || within > maxPATExpiryWindow {
		return 0, false
	}
	return within, true
}
