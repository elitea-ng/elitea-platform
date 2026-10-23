package admin

// Admin › Schedules — the platform's recurring routines (issue #1369).
//
// This replaces the screen that listed `centry.schedule`, a legacy table that
// nothing executes. The rows here are the routines the Go platform really runs:
// elitea-main's scheduled jobs and elitea-scheduler's workers, one row each in
// `elitea_runtime.platform_routines`. An operator can switch one off or give it
// another schedule; the owning process picks the change up on its next tick or
// poll, without a restart.
//
// The permissions are the pair the old screen already used,
// `configuration.scheduling.schedules.view` and `.edit`, so nobody needs a new
// grant. Every write passes through the admin audit middleware (the whole
// `/api/v2/admin/` prefix is audited) and the row records `updated_by`.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/libs/go/routines"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

// PlatformRoutineStore is the registry the page reads and edits.
type PlatformRoutineStore interface {
	List(ctx context.Context) ([]routines.Row, error)
	Update(ctx context.Context, id string, patch routines.Patch, now time.Time) (routines.Row, error)
}

// WithPlatformRoutines supplies the registry. Unassigned, the routes answer
// 503 rather than an empty list: "no routines" and "this deployment cannot see
// its routines" must not look the same.
func WithPlatformRoutines(store PlatformRoutineStore) Option {
	return func(h *Handler) { h.platformRoutines = store }
}

type platformRoutineJSON struct {
	ID               string     `json:"id"`
	Owner            string     `json:"owner"`
	Description      string     `json:"description"`
	DefaultSchedule  string     `json:"default_schedule"`
	ScheduleOverride *string    `json:"schedule_override"`
	Schedule         string     `json:"schedule"`
	MinIntervalSecs  int64      `json:"min_interval_seconds"`
	Enabled          bool       `json:"enabled"`
	LastRunAt        *time.Time `json:"last_run_at"`
	LastOutcome      *string    `json:"last_outcome"`
	NextRunAt        *time.Time `json:"next_run_at"`
	UpdatedBy        *string    `json:"updated_by"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

func platformRoutineView(row routines.Row) platformRoutineJSON {
	return platformRoutineJSON{
		ID:               row.ID,
		Owner:            string(row.Owner),
		Description:      row.Description,
		DefaultSchedule:  row.DefaultSchedule,
		ScheduleOverride: row.ScheduleOverride,
		Schedule:         row.EffectiveSchedule(),
		MinIntervalSecs:  int64(row.MinInterval / time.Second),
		Enabled:          row.Enabled,
		LastRunAt:        row.LastRunAt,
		LastOutcome:      row.LastOutcome,
		NextRunAt:        row.NextRunAt,
		UpdatedBy:        row.UpdatedBy,
		UpdatedAt:        row.UpdatedAt,
	}
}

// PlatformRoutines serves GET /api/v2/admin/platform_routines/administration.
func (h *Handler) PlatformRoutines(w http.ResponseWriter, r *http.Request) {
	if h.platformRoutines == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "this deployment has no routine registry configured, so it cannot report its schedules",
		})
		return
	}
	rows, err := h.platformRoutines.List(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to read the platform routines"})
		return
	}
	view := make([]platformRoutineJSON, 0, len(rows))
	for _, row := range rows {
		view = append(view, platformRoutineView(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": len(view), "rows": view})
}

// platformRoutineUpdate is the accepted write. Pointers keep "absent"
// distinct from false and from the empty string; an empty `schedule` removes
// the override and puts the default back.
type platformRoutineUpdate struct {
	Enabled  *bool   `json:"enabled"`
	Schedule *string `json:"schedule"`
}

// UpdatePlatformRoutine serves PUT /api/v2/admin/platform_routines/administration/{id}.
func (h *Handler) UpdatePlatformRoutine(w http.ResponseWriter, r *http.Request) {
	if h.platformRoutines == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "this deployment has no routine registry configured, so it cannot change its schedules",
		})
		return
	}
	var body platformRoutineUpdate
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request body"})
		return
	}
	principal, _ := auth.UserFromContext(r.Context())
	actor := principal.Email
	if actor == "" {
		actor = principal.ID
	}
	row, err := h.platformRoutines.Update(r.Context(), chi.URLParam(r, "id"), routines.Patch{
		Enabled:  body.Enabled,
		Schedule: body.Schedule,
		Actor:    actor,
	}, time.Now().UTC())
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, platformRoutineView(row))
	case errors.Is(err, routines.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "routine not found"})
	case errors.Is(err, routines.ErrNothingToUpdate):
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "no updatable field supplied"})
	case errors.Is(err, routines.ErrConflict):
		writeJSON(w, http.StatusConflict, map[string]any{"error": "the routine changed while you were editing it; reload and try again"})
	case errors.Is(err, routines.ErrInvalidSchedule), errors.Is(err, routines.ErrBelowMinimum):
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
	default:
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to update the routine"})
	}
}
