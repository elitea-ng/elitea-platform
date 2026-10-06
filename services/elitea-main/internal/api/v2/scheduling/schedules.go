package scheduling

// The admin Schedules & Tasks surface's first tab: the platform cron table.
//
//	GET /scheduling/schedules/{mode}/{projectID} — the schedule list
//	PUT /scheduling/schedules/{mode}/{projectID} — enable/disable, retime, rename
//
// ## What these rows ARE
//
// One row of `centry.schedule` is a cron expression bound to the NAME of an
// internal platform RPC (`rpc_func`) plus its keyword arguments. It is not a
// tenant workload and not a pipeline. `services/elitea-scheduler` used to poll
// this table once a minute and publish each due `rpc_func` onto the
// Redis/Arbiter bus for legacy Pylon. Nothing in the Go stack consumed that
// bus (issue #305), so that dispatcher was deleted; in a Go-only deployment
// these rows are read and edited here and executed only by a legacy Pylon
// scheduling plugin, where one still runs against the same database.
//
// So this table is live and shared. It is also the mechanism the
// indexing transition itself depends on — services/elitea-scheduler/RETIREMENT.md
// records that "the current `index_scheduling` row is disabled in the hybrid
// deployment" in favour of elitea-main's own `index.schedule.scan.v1`, and
// DISABLING a row is exactly the write below.
//
// ## What was here before unit A14
//
// A third failure shape, the one that reads as working: `Schedules` had a route
// and did read the database, but it asked a different question. It queried the
// TENANT schema for application versions carrying `meta->'trigger'` and reported
// those as schedules — pipeline triggers, which per issue #193 nothing in the Go
// stack executes at all. It answered `{"items": …}` where every client of this
// path reads `{"rows": …, "total": n}`, it hardcoded `Enabled: true` so a
// disabled row could not be represented, it swallowed every query error into an
// empty list, and — the part that made the admin tab structurally dead — it
// short-circuited on `projectID == "0"`, which is the only projectID the admin
// page ever sends. The tab could not have shown a row under any circumstance.
//
// Nothing consumed that shape: a repository-wide search for `scheduling/schedules`
// found the route registration and no caller. It is replaced here rather than
// preserved alongside, because reporting triggers that never fire as active
// schedules is a statement about the system that is not true.
//
// The contract below is pylon's, not an invention — same path, same modes, same
// body keys, same row fields as legacy/plugins/scheduling/api/v2/schedules.py
// and its model in legacy/plugins/scheduling/models/schedule.py, which is what
// the existing admin_ui client already speaks to.
//
// ## The write path deliberately cannot change WHAT RUNS
//
// A scheduled run has no interactive principal. The scheduler dispatches
// `rpc_func` with no caller identity attached, onto a bus whose handlers are
// internal platform functions running with full privilege — there is no user to
// run "as". That makes `rpc_func` and `rpc_kwargs` the security boundary of this
// table, not ordinary fields: a client able to set them could name any internal
// RPC, with any arguments, and have the platform invoke it unattended and
// unauthenticated a minute later.
//
// `scheduleUpdate` therefore accepts `name`, `cron` and `active` only, and
// REFUSES a body carrying `rpc_func` or `rpc_kwargs` with 400 rather than
// ignoring them silently — a caller that thinks it retargeted a schedule and got
// a 200 is the failure this unit exists to stop.
//
// pylon's `SchedulePutModel` permits `rpc_kwargs`. Not preserving it is a
// deliberate narrowing under AGENTS.md's "compatibility never requires
// preserving a vulnerability": the admin page never sends it (it edits the cron
// cell and the active switch, nothing else), so no legitimate-user outcome is
// lost.
//
// ## Cron is validated with the parser that will RUN it
//
// The scheduler builds `cron.NewParser(Minute|Hour|Dom|Month|Dow)` — five
// fields, no `@daily` descriptors — and its `timeToRun` returns FALSE when
// parsing fails. An unparseable cron accepted here would not error at run time;
// it would silently never fire again, which on a platform job looks exactly like
// a job that has nothing to do. The same parser therefore gates the write.
//
// ## Authorisation
//
// Both routes are gated in internal/api/router.go on the permission their pylon
// counterparts declare — `configuration.scheduling.schedules.view` for the read,
// `configuration.scheduling.schedules.edit` for the write — resolved per request
// from auth_core__user_role. The admin SPA's `window.admin_ui_config.permissions`
// is presentation state and is never consulted here.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/robfig/cron/v3"
)

// scheduleCronParser is the five-field parser, WITHOUT `cron.Descriptor`,
// that services/elitea-scheduler uses to decide whether a row is due. Accepting
// an expression it cannot parse would persist a schedule that never fires.
var scheduleCronParser = cron.NewParser(
	cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow,
)

// administrationMode is the pylon mode whose handler lists EVERY schedule.
const administrationMode = "administration"

// scheduleRow is one `centry.schedule` row as the admin page reads it. Field
// names are the SQLAlchemy model's `to_json()` keys, which is what the existing
// client indexes into.
type scheduleRow struct {
	ID        int64          `json:"id"`
	Name      string         `json:"name"`
	ProjectID *int           `json:"project_id"`
	Cron      string         `json:"cron"`
	Active    bool           `json:"active"`
	RPCFunc   string         `json:"rpc_func"`
	RPCKwargs map[string]any `json:"rpc_kwargs"`
	LastRun   *time.Time     `json:"last_run"`
}

// scheduleUpdateBody is the accepted write. Every field is a POINTER so that
// "absent" is distinguishable from "set to the zero value" — pylon's
// `exclude_unset` semantics, which the page relies on when it PUTs `active`
// alone and expects `cron` to survive.
//
// RPCFunc and RPCKwargs are declared ONLY so that a body carrying them can be
// refused explicitly. They are never applied. See this file's header.
type scheduleUpdateBody struct {
	ID        *int64          `json:"id"`
	Name      *string         `json:"name"`
	Cron      *string         `json:"cron"`
	Active    *bool           `json:"active"`
	RPCFunc   *string         `json:"rpc_func"`
	RPCKwargs json.RawMessage `json:"rpc_kwargs"`
}

// requestMode reports whether this request is the ADMINISTRATION variant.
//
// It cannot simply read `chi.URLParam(r, "mode")`. The administration routes are
// registered with `administration` as a STATIC path segment — deliberately, so
// chi's trie prefers them over the `{mode}` routes whose gate is project-scoped
// — and a static segment binds no URL parameter, so `{mode}` comes back EMPTY on
// exactly the requests that are administration requests.
//
// Getting this wrong is silent in the worst direction: the handler would fall
// through to the project branch, fail to parse the admin page's `projectID=0`,
// and answer 400 to every administration call. The integration tests mount both
// registrations the way internal/api/router.go does, which is what caught it.
//
// `AdministrationSchedules`/`AdministrationSchedulesUpdate` therefore state the
// mode rather than inferring it, and the `{mode}` handlers read the parameter.
func isAdministrationRequest(r *http.Request) bool {
	return chi.URLParam(r, "mode") == administrationMode
}

// AdministrationSchedules serves `GET /scheduling/schedules/administration/{projectID}`.
// The `{projectID}` segment is part of pylon's URL shape and carries no meaning
// in this mode — the admin page sends `0` — so it is ignored rather than parsed.
func (h *Handler) AdministrationSchedules(w http.ResponseWriter, r *http.Request) {
	h.serveSchedules(w, r, true)
}

// AdministrationSchedulesUpdate serves `PUT /scheduling/schedules/administration/{projectID}`.
func (h *Handler) AdministrationSchedulesUpdate(w http.ResponseWriter, r *http.Request) {
	h.serveSchedulesUpdate(w, r, true)
}

// Schedules serves `GET /scheduling/schedules/{mode}/{projectID}`.
//
// `administration` lists the whole table (pylon's AdminAPI.get); any other mode
// lists the rows belonging to `{projectID}` (pylon's ProjectAPI.get). The
// project listing is `project_id = $1` and never `IS NULL OR = $1`: the
// project-less rows are PLATFORM jobs, and showing them inside one project's
// settings would invite an operator to disable a platform job from a page that
// looks project-scoped.
func (h *Handler) Schedules(w http.ResponseWriter, r *http.Request) {
	h.serveSchedules(w, r, isAdministrationRequest(r))
}

func (h *Handler) serveSchedules(w http.ResponseWriter, r *http.Request, administration bool) {
	if h.pool == nil {
		writeScheduleError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}

	rows, err := h.listSchedules(r.Context(), administration, chi.URLParam(r, "projectID"))
	if err != nil {
		if errors.Is(err, errInvalidProjectID) {
			writeScheduleError(w, http.StatusBadRequest, "invalid project id")
			return
		}
		// Deliberately NOT an empty list. The implementation this replaces
		// answered 200 with `[]` on every query failure, so a missing table and
		// an idle platform were the same screen.
		writeScheduleError(w, http.StatusInternalServerError, "failed to read schedules")
		return
	}

	writeScheduleJSON(w, http.StatusOK, map[string]any{"total": len(rows), "rows": rows})
}

var errInvalidProjectID = errors.New("invalid project id")

func (h *Handler) listSchedules(ctx context.Context, administration bool, projectID string) ([]scheduleRow, error) {
	const columns = `SELECT id, name, project_id, cron, COALESCE(active, false), rpc_func,
	                        COALESCE(rpc_kwargs, '{}'::jsonb), last_run
	                 FROM centry.schedule`

	// ORDER BY carries an `id` tiebreaker: `name` is not unique on this table
	// (the column has no unique constraint), so name alone is not a total order
	// and PostgreSQL may return tied rows differently between two reads of an
	// unchanged table — which the client's sort would then shuffle again.
	const ordering = ` ORDER BY name, id`

	var (
		rows interface {
			Next() bool
			Scan(...any) error
			Err() error
			Close()
		}
		err error
	)

	if administration {
		rows, err = h.pool.Query(ctx, columns+ordering)
	} else {
		id, convErr := strconv.Atoi(projectID)
		if convErr != nil || id <= 0 {
			return nil, errInvalidProjectID
		}
		rows, err = h.pool.Query(ctx, columns+` WHERE project_id = $1`+ordering, id)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Non-nil so an empty table marshals as `[]` and not `null` — the client
	// iterates it directly.
	result := make([]scheduleRow, 0)
	for rows.Next() {
		var row scheduleRow
		var kwargsRaw []byte
		if err := rows.Scan(&row.ID, &row.Name, &row.ProjectID, &row.Cron,
			&row.Active, &row.RPCFunc, &kwargsRaw, &row.LastRun); err != nil {
			// A scan failure here is a schema disagreement, not a bad row. The
			// old handler `continue`d past these, so a column-type drift showed
			// up as a shorter list rather than as an error.
			return nil, err
		}
		if len(kwargsRaw) > 0 {
			if err := json.Unmarshal(kwargsRaw, &row.RPCKwargs); err != nil {
				return nil, err
			}
		}
		if row.RPCKwargs == nil {
			row.RPCKwargs = map[string]any{}
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

// SchedulesUpdate serves `PUT /scheduling/schedules/{mode}/{projectID}`.
//
// The id travels in the BODY, not the path — pylon's shape, and what the
// existing client sends. In a non-administration mode the update is additionally
// constrained to the `{projectID}` in the path, so a project-scoped caller
// cannot retime another project's schedule (or a platform one) by guessing an id.
func (h *Handler) SchedulesUpdate(w http.ResponseWriter, r *http.Request) {
	h.serveSchedulesUpdate(w, r, isAdministrationRequest(r))
}

func (h *Handler) serveSchedulesUpdate(w http.ResponseWriter, r *http.Request, administration bool) {
	if h.pool == nil {
		writeScheduleError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}

	var body scheduleUpdateBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeScheduleError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if body.ID == nil || *body.ID <= 0 {
		writeScheduleError(w, http.StatusBadRequest, "id is required")
		return
	}
	// Refused, never ignored. See this file's header on run-as identity.
	if body.RPCFunc != nil || body.RPCKwargs != nil {
		writeScheduleError(w, http.StatusBadRequest,
			"rpc_func and rpc_kwargs cannot be changed: a schedule runs unattended with full "+
				"platform privilege and no caller identity, so what it invokes is fixed at creation")
		return
	}

	assignments := make([]string, 0, 3)
	arguments := make([]any, 0, 5)

	if body.Name != nil {
		name := strings.TrimSpace(*body.Name)
		if name == "" {
			writeScheduleError(w, http.StatusBadRequest, "name cannot be empty")
			return
		}
		// The column is VARCHAR(64); a longer name is a 500 from PostgreSQL
		// otherwise, which reads to the operator as "the save broke".
		if len(name) > 64 {
			writeScheduleError(w, http.StatusBadRequest, "name is longer than 64 characters")
			return
		}
		arguments = append(arguments, name)
		assignments = append(assignments, "name = $"+strconv.Itoa(len(arguments)))
	}
	if body.Cron != nil {
		expression := strings.TrimSpace(*body.Cron)
		if _, err := scheduleCronParser.Parse(expression); err != nil {
			writeScheduleError(w, http.StatusBadRequest,
				"cron expression is invalid: five fields are required and descriptors such as @daily are not supported")
			return
		}
		if len(expression) > 64 {
			writeScheduleError(w, http.StatusBadRequest, "cron expression is longer than 64 characters")
			return
		}
		arguments = append(arguments, expression)
		assignments = append(assignments, "cron = $"+strconv.Itoa(len(arguments)))
	}
	if body.Active != nil {
		arguments = append(arguments, *body.Active)
		assignments = append(assignments, "active = $"+strconv.Itoa(len(arguments)))
	}

	if len(assignments) == 0 {
		// A PUT with nothing to apply is a client bug, and answering 200 would
		// report a save that did not happen — the exact shape #130 shipped.
		writeScheduleError(w, http.StatusBadRequest, "no updatable field supplied")
		return
	}

	arguments = append(arguments, *body.ID)
	statement := "UPDATE centry.schedule SET " + strings.Join(assignments, ", ") +
		" WHERE id = $" + strconv.Itoa(len(arguments))

	if !administration {
		projectID, err := strconv.Atoi(chi.URLParam(r, "projectID"))
		if err != nil || projectID <= 0 {
			writeScheduleError(w, http.StatusBadRequest, "invalid project id")
			return
		}
		arguments = append(arguments, projectID)
		statement += " AND project_id = $" + strconv.Itoa(len(arguments))
	}

	tag, err := h.pool.Exec(r.Context(), statement, arguments...)
	if err != nil {
		writeScheduleError(w, http.StatusInternalServerError, "failed to update schedule")
		return
	}
	if tag.RowsAffected() == 0 {
		// Distinguishable from success on purpose: an id that matches nothing
		// (or belongs to another project) is a 404, not a 200 that changed
		// nothing.
		writeScheduleError(w, http.StatusNotFound, "schedule not found")
		return
	}

	writeScheduleJSON(w, http.StatusOK, map[string]any{"id": *body.ID})
}

func writeScheduleJSON(w http.ResponseWriter, code int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(value)
}

func writeScheduleError(w http.ResponseWriter, code int, reason string) {
	writeScheduleJSON(w, code, map[string]any{"error": reason})
}
