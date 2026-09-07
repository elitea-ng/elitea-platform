package pipelinetriggers

// The scheduled-run TICK — issue 193.
//
// One pass over every tenant that holds pipeline schedules. It is driven by
// elitea-main's platform scheduler (internal/application/scheduling), which
// already leases each occurrence across replicas, so this file owns product
// discovery and admission and owns NO clock, no replica claim and no occurrence
// ledger. The package doc states why the tick is here and not in
// services/elitea-scheduler, and what was carried across from it.
//
// # THE THREE RULES CARRIED FROM elitea-scheduler, RESTATED WHERE THEY APPLY
//
//  1. MAINTENANCE SUPPRESSES DISPATCH AND NOTHING ELSE. The switch is read once
//     per tick, from the same two `centry.platform_config` coordinates the
//     daemon reads. On a closed platform this pass starts nothing and returns.
//     It does not stop, so the window is re-checked every tick and work resumes
//     on the first pass after it closes.
//  2. A SUPPRESSED OR SKIPPED TICK DOES NOT STAMP `last_run`. `last_run` is the
//     record that a run HAPPENED. Writing it for a run nothing performed is
//     issue 305 reached by another route, and it would silently consume the
//     schedule's slot.
//  3. NO CATCH-UP STORM. Due-ness is `next(last_run) <= now`, so a schedule
//     that missed six hourly slots is due exactly ONCE when the reason clears.
//     Firing once per missed minute would land a whole backlog on a platform
//     that has just come back.
//
// # FAILURE IS PERMISSIVE, AND HERE THAT MEANS WORK CONTINUES
//
// An unreadable maintenance switch does NOT halt every tenant's schedules. That
// would be an outage this code caused rather than one an operator asked for,
// and it would look exactly like a maintenance window nobody opened. Same for
// one tenant's error: the pass records it against that schedule and continues
// to the next tenant, because one project's broken pipeline must not stop
// everybody else's cron.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/audit"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/tenantschema"
)

// The maintenance switch's coordinates. They mirror
// platformconfig.SectionMaintenance and KeyMaintenanceEnabled, and
// elitea-scheduler restates the same two strings for the same reason: this is a
// DATABASE contract, and both readers must name the same row.
const (
	maintenanceSection    = "maintenance"
	maintenanceEnabledKey = "maintenance_enabled"
)

// maxSchedulesPerTick bounds one pass.
//
// A tick must finish inside its own cadence, and an unbounded pass over a
// platform with thousands of schedules would not. Reaching the bound is
// reported rather than hidden: the remaining rows stay due, because nothing
// stamped them, and the next pass takes them.
const maxSchedulesPerTick = 256

// TickResult reports what one pass did. Every field is a count of a DECISION,
// so a caller can tell "nothing was due" from "everything was suppressed".
type TickResult struct {
	Suppressed bool
	Tenants    int
	Considered int
	Dispatched int
	Skipped    int
	Failed     int
	Truncated  bool
}

// RunDueSchedules performs one pass.
func (h *Handler) RunDueSchedules(ctx context.Context, now time.Time) (TickResult, error) {
	var result TickResult
	if h == nil || h.pool == nil {
		return result, errors.New("pipelinetriggers: no database for the schedule tick")
	}
	if h.maintenanceActive(ctx) {
		h.log().Info("pipelinetriggers: maintenance mode is active; not dispatching pipeline schedules")
		result.Suppressed = true
		return result, nil
	}
	schemas, err := h.scheduleTenants(ctx)
	if err != nil {
		return result, err
	}
	result.Tenants = len(schemas)
	for _, tenant := range schemas {
		if result.Considered >= maxSchedulesPerTick {
			result.Truncated = true
			break
		}
		if err := ctx.Err(); err != nil {
			return result, err
		}
		h.runTenantSchedules(ctx, tenant, now, &result)
	}
	return result, nil
}

// scheduleTenant is one tenant schema the tick visits.
type scheduleTenant struct {
	ProjectID int64
	Schema    string
}

// scheduleTenants finds the tenant schemas that actually HOLD the table.
//
// It reads the PostgreSQL catalogue rather than `centry.project`, and that is
// the point: a project whose tenant schema has not yet run migration 0133 has
// no `pipeline_schedules` to scan, and asking it for one every minute would
// raise 42P01 once per project per tick during a rollout. The catalogue answers
// "which schemas can be scanned" directly.
//
// The schema NAME is re-derived through `tenantschema.Quote` from the project
// id parsed out of it, never interpolated from the catalogue string. A
// catalogue read is trusted less than it looks: this query's result becomes
// statement TEXT, and quoting it with the same helper every request path uses
// keeps one rule for that in the service instead of two.
func (h *Handler) scheduleTenants(ctx context.Context) ([]scheduleTenant, error) {
	rows, err := h.pool.Query(ctx, `
SELECT namespace.nspname
  FROM pg_class AS table_entry
  JOIN pg_namespace AS namespace ON namespace.oid = table_entry.relnamespace
 WHERE table_entry.relname = 'pipeline_schedules'
   AND table_entry.relkind = 'r'
   AND namespace.nspname ~ '^p_[0-9]+$'
 ORDER BY namespace.nspname`)
	if err != nil {
		return nil, fmt.Errorf("pipelinetriggers: list schedule tenants: %w", err)
	}
	defer rows.Close()
	var tenants []scheduleTenant
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		projectID, err := strconv.ParseInt(strings.TrimPrefix(name, "p_"), 10, 64)
		if err != nil || projectID <= 0 {
			continue
		}
		schema, err := tenantschema.Quote(strconv.FormatInt(projectID, 10))
		if err != nil {
			continue
		}
		tenants = append(tenants, scheduleTenant{ProjectID: projectID, Schema: schema})
	}
	return tenants, rows.Err()
}

func (h *Handler) runTenantSchedules(
	ctx context.Context, tenant scheduleTenant, now time.Time, result *TickResult,
) {
	schedules, err := h.activeSchedules(ctx, tenant.Schema)
	if err != nil {
		// One tenant's failure is not the platform's. Recorded and stepped
		// over; the rows stay due because nothing stamped them.
		h.log().Error("pipelinetriggers: list active schedules",
			"project_id", tenant.ProjectID, "err", err)
		return
	}
	for _, schedule := range schedules {
		if result.Considered >= maxSchedulesPerTick {
			result.Truncated = true
			return
		}
		if !timeToRun(schedule, now) {
			continue
		}
		result.Considered++
		h.fireSchedule(ctx, tenant, schedule, now, result)
	}
}

// timeToRun is elitea-scheduler's rule, unchanged: a row that has never run is
// due, and otherwise the next occurrence after the LAST RUN must have passed.
//
// An unparseable expression answers FALSE. The write path refuses one, so a row
// holding one arrived some other way; firing it on an unknown cadence would be
// worse than not firing it, and `last_result` is not touched because nothing
// about the row changed.
func timeToRun(schedule scheduleRow, now time.Time) bool {
	if schedule.LastRun == nil {
		return true
	}
	parsed, err := scheduleCronParser.Parse(schedule.Cron)
	if err != nil {
		return false
	}
	return !parsed.Next(*schedule.LastRun).After(now)
}

func (h *Handler) fireSchedule(
	ctx context.Context, tenant scheduleTenant, schedule scheduleRow, now time.Time, result *TickResult,
) {
	started := time.Now()
	record := func(outcome, detail, executionID string) {
		if err := h.recordScheduleOutcome(
			ctx, tenant.Schema, schedule.ID, outcome, detail, executionID, now,
		); err != nil {
			h.log().Error("pipelinetriggers: record schedule outcome",
				"project_id", tenant.ProjectID, "schedule_id", schedule.ID, "err", err)
		}
		h.recordScheduleAudit(ctx, tenant.ProjectID, schedule, outcome, detail, time.Since(started))
	}

	// OVERLAP. Asked before the run is authorized, because a schedule whose
	// previous run is still going must not start a second one even if
	// everything else about it is fine.
	active, err := h.previousRunActive(ctx, tenant.Schema, schedule.ID)
	if err != nil {
		h.log().Error("pipelinetriggers: overlap check", "schedule_id", schedule.ID, "err", err)
		result.Failed++
		record(resultFailed, "the previous run could not be checked", "")
		return
	}
	if active {
		result.Skipped++
		record(resultSkippedOverlap, "the previous run has not finished", "")
		return
	}

	outcome, err := h.admit(ctx, tenant.Schema, runRequest{
		ProjectID:   tenant.ProjectID,
		ActorUserID: schedule.AuthorID,
		VersionID:   schedule.VersionID,
		Input:       schedule.UserInput,
		Origin:      OriginSchedule,
		ScheduleID:  schedule.ID,
	})
	switch {
	case errors.Is(err, ErrRunForbidden):
		result.Skipped++
		record(resultSkippedAuth,
			"the schedule author no longer holds "+RunPermission+" in this project", "")
	case errors.Is(err, ErrVersionNotRunnable):
		result.Skipped++
		record(resultSkippedMissing, "the pipeline version is gone or is not a pipeline", "")
	case err != nil:
		h.log().Error("pipelinetriggers: scheduled run failed",
			"project_id", tenant.ProjectID, "schedule_id", schedule.ID, "err", err)
		result.Failed++
		record(resultFailed, "the run could not be started", "")
	default:
		result.Dispatched++
		record(resultDispatched, "", outcome.ExecutionID)
	}
}

// previousRunActive reports whether this schedule's last run is still going.
//
// It reads the CHAT PROJECTION — a response group with `is_streaming` still
// set — rather than the runtime execution row, for the same reason MCP's settle
// poll does: the projection commits the terminal state for every ending,
// including a failure, in one transaction. A run that failed leaves no terminal
// event on the replay stream, so a check built on that stream would treat a
// failed run as forever active and would suppress the schedule for good.
func (h *Handler) previousRunActive(ctx context.Context, schema string, scheduleID int64) (bool, error) {
	var active bool
	err := h.pool.QueryRow(ctx, fmt.Sprintf(`
SELECT EXISTS (
    SELECT 1
      FROM %[1]s.chat_message_group AS response
      JOIN %[1]s.chat_conversations AS conversation
        ON conversation.id = response.conversation_id
     WHERE conversation.source = $1
       AND conversation.meta ->> 'schedule_id' = $2
       AND response.is_streaming
)`, schema), TriggerConversationSource, strconv.FormatInt(scheduleID, 10)).Scan(&active)
	if err != nil {
		return false, err
	}
	return active, nil
}

// maintenanceActive reports whether an operator has closed the platform.
//
// An absent row means no window — the switch has never been written, which is
// the state of every deployment that has not used the feature. Every failure
// mode returns FALSE and lets work continue; see this file's header.
func (h *Handler) maintenanceActive(ctx context.Context) bool {
	var raw []byte
	err := h.pool.QueryRow(ctx, `
SELECT value FROM centry.platform_config
 WHERE section = $1 AND key = $2
 LIMIT 1`, maintenanceSection, maintenanceEnabledKey).Scan(&raw)
	if err != nil {
		// pgx reports "no rows" here, which is the common case and not worth a
		// log line; anything else is, but neither halts the pass.
		if !strings.Contains(err.Error(), "no rows") {
			h.log().Error("pipelinetriggers: maintenance switch unreadable; continuing to dispatch", "err", err)
		}
		return false
	}
	var enabled bool
	if err := json.Unmarshal(raw, &enabled); err != nil {
		// A value of another type is a row this platform did not write — the
		// admin surface type-checks every field. Treating it as "on" would halt
		// every tenant's scheduled work on one malformed row.
		h.log().Error("pipelinetriggers: maintenance switch is not a boolean; continuing to dispatch", "err", err)
		return false
	}
	return enabled
}

// recordScheduleAudit writes the operator-facing half of failure visibility.
//
// It is written for EVERY outcome, not only failures. "This schedule fired at
// 03:00 as user 42" is the row that answers who a run belonged to, and a trail
// that holds only the failures cannot answer it.
func (h *Handler) recordScheduleAudit(
	ctx context.Context, projectID int64, schedule scheduleRow, outcome, detail string, elapsed time.Duration,
) {
	if h == nil || h.recorder == nil {
		return
	}
	action := "pipeline schedule " + outcome
	if detail != "" {
		action += " — " + detail
	}
	status := int32(http.StatusAccepted)
	if outcome != resultDispatched {
		status = int32(http.StatusConflict)
	}
	// A duration is ALWAYS recorded. The Audit Trail's heatmap buckets rows into
	// five duration bands, and a row with no duration belongs to no band — so it
	// disappears from the view an operator uses to find slow work.
	duration := float64(elapsed.Microseconds()) / 1000
	// `schedule` is one of the two event types the admin Audit Trail files on
	// its SYSTEM tab (apps/elitea-web/src/pages/admin/auditPalette.json). A
	// scheduled pipeline run belongs there and not on the user tab: no person
	// made the request.
	h.recorder.Record(context.WithoutCancel(ctx), audit.Event{
		Timestamp:  time.Now().UTC(),
		EventType:  "schedule",
		Action:     action,
		ProjectID:  audit.ID(projectID),
		UserID:     audit.ID(schedule.AuthorID),
		StatusCode: &status,
		DurationMS: &duration,
		IsError:    outcome == resultFailed,
		EntityType: "pipeline",
		EntityID:   audit.ID(schedule.VersionID),
	})
}
