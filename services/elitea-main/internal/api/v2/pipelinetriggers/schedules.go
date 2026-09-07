package pipelinetriggers

// The SETTINGS half of the schedule — issue 193.
//
//	GET    /pipeline_schedules/prompt_lib/{projectID}/{versionID}
//	PUT    /pipeline_schedules/prompt_lib/{projectID}/{versionID}
//	DELETE /pipeline_schedules/prompt_lib/{projectID}/{versionID}
//
// # WHY A TABLE AND NOT THE INDEX-SCHEDULE SHAPE
//
// Issue 193 asks this to be a decision rather than an accident. The shipped
// precedent is `elitea_tools.meta.indexes_meta[<name>].schedules[<userId>]`,
// read as `schedules[userId] ?? schedules[-1]`. It is not reused here, for
// three reasons of decreasing weight:
//
//  1. IT IS PER-USER, AND A PIPELINE SCHEDULE MUST NOT BE. That shape stores
//     one schedule PER USER against one resource, with `-1` as a project-wide
//     default. A pipeline is one thing that either runs at 03:00 or does not;
//     two people saving different crons would give one pipeline two unattended
//     run streams, each invisible to the other's author. "Whose schedule fired
//     this?" would have no answer in the row.
//  2. THE SCANNER WOULD HAVE TO READ EVERY AGENT'S JSONB. The tick asks "which
//     schedules are due" across a tenant. Against a jsonb map that is a full
//     scan with a nested key walk and no index; against a table it is one
//     partial index on `active`.
//  3. THE OUTCOME COLUMNS HAVE NOWHERE TO GO. `last_run`, `last_result` and
//     `last_execution_id` are what make an unwatched failure visible, and a
//     concurrent read-modify-write of a shared jsonb blob is exactly the wrong
//     place to keep a value the scheduler stamps every minute.
//
// The one thing that IS reused is the cron dialect: the same five-field parser,
// with no `@daily` descriptors, that elitea-scheduler validates
// `centry.schedule` rows with. See scheduleCronParser below.

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

// scheduleCronParser is the FIVE-FIELD parser, without `cron.Descriptor`.
//
// It is the parser that will RUN the row (schedulerun.go uses the same
// variable), and it gates the write for the reason
// internal/api/v2/scheduling/schedules.go records for `centry.schedule`: an
// expression the runner cannot parse would not error at run time, it would
// silently never fire — which on an unattended job looks exactly like a job
// with nothing to do.
var scheduleCronParser = cron.NewParser(
	cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow,
)

// maxScheduleInput bounds the stored text a scheduled run puts in front of the
// pipeline.
const maxScheduleInput = 4096

// scheduleView is what the settings tab reads.
type scheduleView struct {
	Configured      bool       `json:"configured"`
	Cron            string     `json:"cron,omitempty"`
	Active          bool       `json:"active"`
	Input           string     `json:"input,omitempty"`
	AuthorID        int64      `json:"author_id,omitempty"`
	CreatedAt       *time.Time `json:"created_at,omitempty"`
	UpdatedAt       *time.Time `json:"updated_at,omitempty"`
	LastRun         *time.Time `json:"last_run,omitempty"`
	LastResult      string     `json:"last_result,omitempty"`
	LastResultText  string     `json:"last_result_detail,omitempty"`
	LastResultAt    *time.Time `json:"last_result_at,omitempty"`
	LastExecutionID string     `json:"last_execution_id,omitempty"`
	// NextRun is computed, not stored. It is the answer to "when will this
	// actually fire", which a cron expression does not give a person on sight —
	// and it is computed HERE, with the parser that will fire it, so the
	// preview cannot disagree with the behaviour.
	NextRun *time.Time `json:"next_run,omitempty"`
}

func scheduleViewOf(row scheduleRow) scheduleView {
	view := scheduleView{
		Configured:   true,
		Cron:         row.Cron,
		Active:       row.Active,
		Input:        row.UserInput,
		AuthorID:     row.AuthorID,
		CreatedAt:    &row.CreatedAt,
		UpdatedAt:    &row.UpdatedAt,
		LastRun:      row.LastRun,
		LastResultAt: row.LastResultAt,
	}
	if row.LastResult != nil {
		view.LastResult = *row.LastResult
	}
	if row.LastResultText != nil {
		view.LastResultText = *row.LastResultText
	}
	if row.LastExecutionID != nil {
		view.LastExecutionID = *row.LastExecutionID
	}
	if next, ok := nextRun(row.Cron, time.Now().UTC()); ok {
		view.NextRun = &next
	}
	return view
}

// nextRun is the preview. It reports false for an expression the parser
// refuses, so the tab shows no next run rather than an invented one.
func nextRun(expression string, after time.Time) (time.Time, bool) {
	parsed, err := scheduleCronParser.Parse(expression)
	if err != nil {
		return time.Time{}, false
	}
	return parsed.Next(after), true
}

// scheduleBody is the accepted write.
//
// There is no `author_id` field, and there is no `last_*` field. The author is
// the authenticated caller (store.go states why the upsert RESETS it), and the
// outcome columns belong to the runner. A body carrying them is not refused, it
// is simply not read — the same rule inbound.go applies to its own body.
type scheduleBody struct {
	Cron   string  `json:"cron"`
	Active *bool   `json:"active"`
	Input  *string `json:"input"`
}

// GetSchedule reads the schedule. Like the trigger read it answers 200 with
// `{"configured": false}` for a pipeline that has none.
func (h *Handler) GetSchedule(w http.ResponseWriter, r *http.Request) {
	schema, _, versionID, ok := h.routeTarget(w, r)
	if !ok {
		return
	}
	row, err := h.scheduleByVersion(r.Context(), schema, versionID)
	switch {
	case errors.Is(err, ErrNotFound):
		writeJSON(w, http.StatusOK, scheduleView{})
		return
	case err != nil:
		h.log().Error("pipelinetriggers: read schedule", "err", err)
		writeError(w, http.StatusInternalServerError, "the schedule could not be read")
		return
	}
	writeJSON(w, http.StatusOK, scheduleViewOf(row))
}

// SaveSchedule creates or replaces the schedule.
func (h *Handler) SaveSchedule(w http.ResponseWriter, r *http.Request) {
	schema, _, versionID, ok := h.routeTarget(w, r)
	if !ok {
		return
	}
	actorID, ok := h.actorID(w, r)
	if !ok {
		return
	}
	var body scheduleBody
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxInboundBody))
	if err != nil || json.Unmarshal(raw, &body) != nil {
		writeError(w, http.StatusBadRequest, "the request body is not valid JSON")
		return
	}
	expression := strings.TrimSpace(body.Cron)
	if expression == "" {
		writeError(w, http.StatusBadRequest, "a cron expression is required")
		return
	}
	if _, err := scheduleCronParser.Parse(expression); err != nil {
		writeError(w, http.StatusBadRequest,
			"this cron expression cannot be parsed; five fields are required, and names such as @daily are not accepted")
		return
	}
	input := ""
	if body.Input != nil {
		input = *body.Input
	}
	if len(input) > maxScheduleInput {
		writeError(w, http.StatusBadRequest, "the scheduled run input is too long")
		return
	}
	active := false
	if body.Active != nil {
		active = *body.Active
	}

	target, err := h.resolveRunTarget(r.Context(), schema, versionID)
	switch {
	case errors.Is(err, ErrVersionNotRunnable):
		writeError(w, http.StatusNotFound, "no such pipeline version in this project")
		return
	case err != nil:
		h.log().Error("pipelinetriggers: resolve version", "err", err)
		writeError(w, http.StatusInternalServerError, "the pipeline version could not be read")
		return
	}

	row, err := h.upsertSchedule(r.Context(), schema,
		target.ApplicationID, versionID, actorID, expression, active, input)
	if err != nil {
		h.log().Error("pipelinetriggers: write schedule", "err", err)
		writeError(w, http.StatusInternalServerError, "the schedule could not be saved")
		return
	}
	h.annotate(r, versionID, target.Name, 0)
	writeJSON(w, http.StatusOK, scheduleViewOf(row))
}

// DeleteSchedule removes the schedule.
//
// Unlike a trigger, a schedule IS deleted rather than tombstoned. The two are
// not symmetric: a trigger is a credential somebody outside holds, so "when did
// it stop working" is a question about a third party, while a schedule is
// configuration its own author can see and re-create, and every fire it ever
// made is already an audit row.
func (h *Handler) DeleteSchedule(w http.ResponseWriter, r *http.Request) {
	schema, _, versionID, ok := h.routeTarget(w, r)
	if !ok {
		return
	}
	switch err := h.deleteSchedule(r.Context(), schema, versionID); {
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, "this pipeline has no schedule")
		return
	case err != nil:
		h.log().Error("pipelinetriggers: delete schedule", "err", err)
		writeError(w, http.StatusInternalServerError, "the schedule could not be deleted")
		return
	}
	h.annotate(r, versionID, "", 0)
	writeJSON(w, http.StatusOK, scheduleView{})
}
