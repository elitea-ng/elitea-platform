package admin

// Admin › Tasks — the platform's own background jobs, and the stop button.
//
// # What this replaces, and what it does not
//
// legacy/plugins/admin's Tasks page reads an in-process Arbiter `task_node`
// (api/v2/tasks.py, gated `runtime.plugins` in `administration` mode). That
// surface has no equivalent here and `/admin/tasks/{mode}` keeps answering 501
// — see arbiterTaskNodeUnavailable in handler.go. This is NOT that surface
// under a new name.
//
// It answers the QUESTION that page answered: what is running on this platform,
// who started it, when, and can it be stopped. The rows come from the
// platform's own tables (internal/infra/db/repos/admin_background_jobs.go).
//
// # The permission
//
// `runtime.plugins`, in administration mode — the string the legacy Tasks page
// declares, and one this platform already grants (shared/0060). No new grant
// arrives with this feature, which is deliberate: an operator who could see the
// legacy Tasks page can see this one, and nobody has to be re-authorised for a
// page they already had.
//
// # The row shape
//
// The four keys legacy's listing carries are kept verbatim — `task_id`,
// `status`, `started_at`, `user` — so an operator's existing tooling still
// reads. `kind`, `name`, `finished_at`, `project_id` and `cancellable` are
// added, because pylon had one kind of task, one project, no completion time
// and a stop button that was always offered.
//
// `cancellable` is computed per row rather than per kind. A settled job cannot
// be stopped and a scheduled occurrence never can, so the page can render the
// control only where the cancel route would accept it — instead of offering
// every row a button that answers 409 for most of them.

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

// BackgroundJobsStore is the read and the runtime cancel this page needs.
type BackgroundJobsStore interface {
	ListBackgroundJobs(context.Context, repos.BackgroundJobFilter) (repos.BackgroundJobPage, error)
	CancelRuntimeJob(ctx context.Context, executionID string) error
}

// EvalRunCanceller is the evaluation half of the cancel. It is the SAME
// repository method the evaluation API's own cancel route calls, so an admin
// stop and a user stop write the same row the same way — the alternative, a
// second UPDATE written here, is how two cancels come to disagree about which
// statuses are terminal.
//
// It is narrowed to the ERROR: the repository answers the updated run as well,
// and this page reads nothing out of it. Taking the row here would make this
// package depend on the evaluation model for a value it discards, and would
// stop a future caller narrowing further.
type EvalRunCanceller interface {
	CancelRun(ctx context.Context, projectID, runID string) error
}

// WithBackgroundJobs supplies the Tasks page's store. Unassigned, the two
// routes answer 503 rather than an empty list: "nothing is running" and "this
// deployment cannot see what is running" must not render identically, which is
// the exact correction handler.go's arbiterTaskNodeUnavailable records.
func WithBackgroundJobs(store BackgroundJobsStore) Option {
	return func(h *Handler) { h.backgroundJobs = store }
}

// WithEvalRunCancel supplies the evaluation-run cancel. Unassigned, an eval
// row's cancel answers 503 while every other kind still stops.
func WithEvalRunCancel(canceller EvalRunCanceller) Option {
	return func(h *Handler) { h.evalRunCancel = canceller }
}

// backgroundJobJSON is one row on the wire.
type backgroundJobJSON struct {
	TaskID      string     `json:"task_id"`
	Kind        string     `json:"kind"`
	Name        string     `json:"name"`
	Status      string     `json:"status"`
	StartedAt   *time.Time `json:"started_at"`
	FinishedAt  *time.Time `json:"finished_at"`
	ProjectID   *int64     `json:"project_id"`
	User        string     `json:"user"`
	Cancellable bool       `json:"cancellable"`
}

// The kinds whose cancel this route can perform, by the store that performs it.
var runtimeCancellableKinds = map[string]bool{
	repos.BackgroundJobKindIndex:     true,
	repos.BackgroundJobKindAgent:     true,
	repos.BackgroundJobKindToolkit:   true,
	repos.BackgroundJobKindExecution: true,
}

func parsePositiveInt(raw string) (int, bool) {
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 {
		return 0, false
	}
	return value, true
}

// BackgroundJobs serves GET /api/v2/admin/background_jobs/administration.
func (h *Handler) BackgroundJobs(w http.ResponseWriter, r *http.Request) {
	if h.backgroundJobs == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "this deployment has no background-job store configured, so it cannot report what is running",
		})
		return
	}

	query := r.URL.Query()
	filter := repos.BackgroundJobFilter{
		Kind:   strings.TrimSpace(query.Get("kind")),
		Status: strings.TrimSpace(query.Get("status")),
	}
	if raw := query.Get("project_id"); raw != "" {
		projectID, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || projectID <= 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "project_id must be a positive integer"})
			return
		}
		filter.ProjectID = &projectID
	}
	if limit, ok := parsePositiveInt(query.Get("limit")); ok {
		filter.Limit = limit
	}
	if offset, ok := parsePositiveInt(query.Get("offset")); ok {
		filter.Offset = offset
	}

	page, err := h.backgroundJobs.ListBackgroundJobs(r.Context(), filter)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": "failed to read the background jobs",
		})
		return
	}

	rows := make([]backgroundJobJSON, 0, len(page.Rows))
	for _, row := range page.Rows {
		rows = append(rows, backgroundJobJSON{
			TaskID:      row.TaskID,
			Kind:        row.Kind,
			Name:        row.Name,
			Status:      row.Status,
			StartedAt:   row.StartedAt,
			FinishedAt:  row.FinishedAt,
			ProjectID:   row.ProjectID,
			User:        row.Principal,
			Cancellable: row.Cancellable,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total": page.Total,
		"rows":  rows,
		// Named on the wire, not only in a log. A capped window that presents
		// itself as the whole history is how an operator concludes a job is not
		// running because they could not see it.
		"truncated": page.Truncated,
	})
}

// CancelBackgroundJob serves
// POST /api/v2/admin/background_jobs/administration/{kind}/{jobID}:cancel.
//
// It DELEGATES per kind rather than writing one UPDATE that covers all of
// them: a runtime job stops by its durable desired state, an evaluation run
// stops through the evaluation repository's own CancelRun, and a scheduled
// occurrence does not stop at all.
func (h *Handler) CancelBackgroundJob(w http.ResponseWriter, r *http.Request) {
	kind := chi.URLParam(r, "kind")
	jobID := strings.TrimSuffix(chi.URLParam(r, "jobID"), ":cancel")
	if jobID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "a job id is required"})
		return
	}

	switch {
	case runtimeCancellableKinds[kind]:
		h.cancelRuntimeBackgroundJob(w, r, jobID)
	case kind == repos.BackgroundJobKindEval:
		h.cancelEvalBackgroundJob(w, r, jobID)
	case kind == repos.BackgroundJobKindSchedule:
		// Not a refusal of a capability this platform has — see
		// scheduledOccurrencesLeg's comment. Disabling the SCHEDULE is the
		// control that changes anything, and it lives on Admin › Schedules.
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "a scheduled occurrence cannot be cancelled; disable the schedule on Admin › Schedules & Tasks instead",
		})
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unknown job kind"})
	}
}

func (h *Handler) cancelRuntimeBackgroundJob(w http.ResponseWriter, r *http.Request, jobID string) {
	if h.backgroundJobs == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "this deployment has no background-job store configured",
		})
		return
	}
	err := h.backgroundJobs.CancelRuntimeJob(r.Context(), jobID)
	if errors.Is(err, repos.ErrBackgroundJobNotCancellable) {
		// 409, not 404: the job may well exist and have finished. Answering
		// "not found" for a settled job sends the operator looking for a row
		// that is on their screen.
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "this job has already settled or is already stopping",
		})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to cancel the job"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// cancelEvalBackgroundJob takes the run id in the path and the project in a
// `project_id` QUERY parameter.
//
// An evaluation run's id is unique only WITHIN its project's schema, so the
// project has to travel with it. It travels in the query rather than as a
// composite path id because the route's own `:cancel` suffix already claims
// the colon: a path id of `7:31` makes chi match `{jobID}` as `7` and then
// fail to find `cancel` — a 404 for a well-formed request. The listing serves
// `project_id` on every eval row, so the page has the value to send.
func (h *Handler) cancelEvalBackgroundJob(w http.ResponseWriter, r *http.Request, jobID string) {
	if h.evalRunCancel == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "this deployment has no evaluation store configured",
		})
		return
	}
	projectID := strings.TrimSpace(r.URL.Query().Get("project_id"))
	runID := jobID
	if projectID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "an evaluation run needs its project_id",
		})
		return
	}
	if err := h.evalRunCancel.CancelRun(r.Context(), projectID, runID); err != nil {
		// apierr.Write, not err.Error(): the evaluation repository answers a
		// TYPED conflict for a run that already finished, whose message names
		// the status the operator needs — and a database failure with the host
		// and the SQLSTATE in it for anything else. Passing the raw text
		// through would ship the second one across a trust boundary, which
		// AGENTS.md forbids; apierr.Write renders the typed message and
		// replaces the untyped one with "internal server error".
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
