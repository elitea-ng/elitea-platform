package scheduler

// Maintenance mode, on the scheduler side.
//
// ## What this is NOT
//
// It is not a port of pylon's `maintenance_gate.py`. That helper existed
// because pylon's maintenance splash was a gevent hook on the HTTP router,
// while cron ticks were wired straight to arbiter events — so cron work took a
// path the hook could not see, and each cron RPC had to remember to ask. The
// shape of the problem here is different and so is the answer.
//
// This scheduler dispatches over Redis to a consumer. It never traverses
// elitea-main's HTTP surface, so the Maintenance middleware that closes the API
// to non-admins (services/elitea-main/internal/api/middleware/maintenance.go)
// cannot and should not see it. An operator who closes the platform and then
// watches scheduled agent runs and index jobs keep starting has been told the
// platform is closed and can see that it is not.
//
// So the gate lives where the work is started, and it is asked once per tick
// rather than once per job.
//
// ## Which loops pause, and which deliberately do not
//
// SCHEDULE DISPATCH pauses, and so does the AUDIT-EVENT RETENTION SWEEP
// (internal/auditretention, issue #619). The daemon runs two other loops and
// both keep running through a window, for reasons specific to what they do:
//
//   - **Budget write-back** drains billing events for requests that have
//     ALREADY been served. Pausing it would not prevent any work; it would
//     delay the accounting for work that already happened and let the backlog
//     grow behind a window whose length nobody bounds. The money has been spent
//     either way, and the ledger should say so.
//   - **Price sync** refreshes a catalogue. It starts nothing, costs nothing
//     and pausing it buys nothing.
//
// "Maintenance" here means "stop STARTING new work", which is what an operator
// closing the platform is asking for. It does not mean "stop the process".
//
// The retention sweep is on the pausing side because it DESTROYS rows. An
// operator who closes the platform is usually about to migrate, back up or
// restore it, and a bulk DELETE running against the table at that moment is
// the one background job that can make such a window worse. Suppression costs
// nothing here: the rows the pass would have removed are removed by the next
// pass after the window closes, and no state is stamped meanwhile (there is no
// cursor to consume — the cutoff is recomputed from the clock every pass).
//
// ## Why this method is exported
//
// The gate is asked by two loops in two packages now. It is exported rather
// than copied because the section and key below are a DATABASE contract with
// no compiler behind them: a second transcription of those strings is a second
// thing that can drift out of step with the admin surface, and the drift would
// be silent in both copies.
//
// ## A suppressed tick does not stamp `last_run`
//
// This is the rule `dispatch` already establishes for an undelivered publish,
// applied to the same question: `last_run` is the only record that a schedule
// ran, and both the admin listing and `timeToRun` read it as one. Writing it
// for a run that was suppressed would say "this ran" about work nothing
// performed — the defect in issue #305, reached by a different route.
//
// Leaving it alone is also what makes a window recoverable: `timeToRun` still
// answers true on the first tick after the window closes, so each suppressed
// schedule runs once, promptly, rather than having its slot silently consumed.
//
// It is once, not once per missed minute. `timeToRun` compares
// `next(last_run) <= now`, so a schedule that missed six hourly slots is due
// exactly once when the window lifts. That is the behaviour to want — a
// catch-up storm at the end of a maintenance window would land the whole
// backlog on a platform that has just come back.
//
// ## Reading the switch
//
// The rows are `centry.platform_config`, which elitea-main's admin
// Configuration page writes and its Maintenance middleware reads. The section
// and key strings are restated here because they are a DATABASE contract rather
// than a Go one: `services/elitea-main/internal/platformconfig` is an
// `internal/` package of another module and cannot be imported, by
// construction. TestMaintenanceKeysMatchTheAdminSurface pins the strings.
//
// The read happens once per tick — every minute — so no cache is needed and a
// window takes effect within one tick either way. That is the same granularity
// the scheduler already works at.
//
// FAILURE IS PERMISSIVE, and here that means work CONTINUES. An unreadable
// switch must not silently halt every scheduled job on the platform: that would
// be an outage this daemon caused rather than one an operator asked for, and it
// would look exactly like a maintenance window nobody opened.

import (
	"context"
	"encoding/json"
	"log/slog"
)

// The `centry.platform_config` coordinates of the maintenance switch. These
// mirror platformconfig.SectionMaintenance and KeyMaintenanceEnabled in
// elitea-main; see the file doc for why they are restated rather than imported.
const (
	maintenanceSection    = "maintenance"
	maintenanceEnabledKey = "maintenance_enabled"
)

// maintenanceEnabledSQL reads the one row that decides it.
//
// A point read on (section, key), not a section scan: the scheduler needs one
// boolean and has no use for the splash copy an operator authored for the SPA.
const maintenanceEnabledSQL = `
	SELECT value FROM centry.platform_config
	 WHERE section = $1 AND key = $2
	 LIMIT 1`

// MaintenanceActive reports whether an operator has closed the platform.
//
// An absent row means no window — the switch has never been written, which is
// the state of every deployment that has not used the feature.
func (s *Scheduler) MaintenanceActive(ctx context.Context) bool {
	rows, err := s.pool.Query(ctx, maintenanceEnabledSQL, maintenanceSection, maintenanceEnabledKey)
	if err != nil {
		slog.Error("scheduler: maintenance switch unreadable; continuing to dispatch", "err", err)
		return false
	}
	defer rows.Close()

	if !rows.Next() {
		// No row: the switch has never been written. Not an error, and by far
		// the common case.
		if err := rows.Err(); err != nil {
			slog.Error("scheduler: maintenance switch unreadable; continuing to dispatch", "err", err)
		}
		return false
	}

	var raw []byte
	if err := rows.Scan(&raw); err != nil {
		slog.Error("scheduler: maintenance switch unreadable; continuing to dispatch", "err", err)
		return false
	}

	var enabled bool
	if err := json.Unmarshal(raw, &enabled); err != nil {
		// A value of another type is a row this platform did not write — the
		// admin surface type-checks every field against its schema. Treating it
		// as "on" would halt the platform's scheduled work on a malformed row.
		slog.Error("scheduler: maintenance switch is not a boolean; continuing to dispatch",
			"err", err)
		return false
	}
	return enabled
}
