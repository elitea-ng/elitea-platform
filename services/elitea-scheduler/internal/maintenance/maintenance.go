// Package maintenance reads the platform maintenance switch for the
// background sweepers this daemon runs.
//
// ## What this is NOT
//
// It is not a port of pylon's `maintenance_gate.py`. That helper existed
// because pylon's maintenance splash was a gevent hook on the HTTP router,
// while cron ticks were wired straight to arbiter events — so cron work took a
// path the hook could not see, and each cron RPC had to remember to ask.
//
// The work this daemon starts never traverses elitea-main's HTTP surface, so
// the Maintenance middleware that closes the API to non-admins
// (services/elitea-main/internal/api/middleware/maintenance.go) cannot see it.
// The gate therefore lives where the work is started, and each loop asks it
// once per pass.
//
// ## History: the legacy schedule dispatcher is gone
//
// This switch was first read by a once-a-minute tick that polled
// `centry.schedule` and PUBLISHed pickle RPC payloads to the `elitea_rpc`
// Redis channel. Nothing in the Go stack consumed that channel (issue #305),
// so the tick, its Redis lock and the Redis client were deleted. Scheduled
// product work runs on elitea-main's platform scheduler; scheduled pipeline
// runs honour the same switch there
// (services/elitea-main/internal/api/v2/pipelinetriggers/schedulerun.go).
//
// ## Which loops pause, and which deliberately do not
//
// The AUDIT-EVENT RETENTION SWEEP (internal/auditretention, issue #619) and the
// SYNC-TOMBSTONE RETENTION SWEEP (internal/syncretention) pause. The daemon's
// other loops keep running through a window, for reasons specific to what
// they do:
//
//   - **Budget write-back** drains billing events for requests that have
//     ALREADY been served. Pausing it would not prevent any work; it would
//     delay the accounting for work that already happened and let the backlog
//     grow behind a window whose length nobody bounds.
//   - **Price sync** refreshes a catalogue. It starts nothing, costs nothing
//     and pausing it buys nothing.
//   - **Native auth retention** only removes credentials that can no longer
//     be used.
//
// "Maintenance" here means "stop STARTING destructive or new work", which is
// what an operator closing the platform is asking for. It does not mean "stop
// the process".
//
// The retention sweeps are on the pausing side because they DESTROY rows. An
// operator who closes the platform is usually about to migrate, back up or
// restore it, and a bulk DELETE running at that moment is the one background
// job that can make such a window worse. Suppression costs nothing: the rows a
// pass would have removed are removed by the next pass after the window
// closes (the cutoff is recomputed from the clock every pass).
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
// FAILURE IS PERMISSIVE, and here that means work CONTINUES. An unreadable
// switch must not silently halt every background job on the platform: that
// would be an outage this daemon caused rather than one an operator asked for,
// and it would look exactly like a maintenance window nobody opened.
package maintenance

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/jackc/pgx/v5"
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
// A point read on (section, key), not a section scan: this package needs one
// boolean and has no use for the splash copy an operator authored for the SPA.
const maintenanceEnabledSQL = `
	SELECT value FROM centry.platform_config
	 WHERE section = $1 AND key = $2
	 LIMIT 1`

// Querier is the slice of *pgxpool.Pool the switch uses.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Switch reads the maintenance switch. It is shared by every loop that honours
// maintenance, so there is exactly one reading of the section and key.
type Switch struct {
	pool Querier
}

// New returns a Switch over pool.
func New(pool Querier) *Switch {
	return &Switch{pool: pool}
}

// Active reports whether an operator has closed the platform.
//
// An absent row means no window — the switch has never been written, which is
// the state of every deployment that has not used the feature.
func (s *Switch) Active(ctx context.Context) bool {
	rows, err := s.pool.Query(ctx, maintenanceEnabledSQL, maintenanceSection, maintenanceEnabledKey)
	if err != nil {
		slog.Error("maintenance: switch unreadable; continuing", "err", err)
		return false
	}
	defer rows.Close()

	if !rows.Next() {
		// No row: the switch has never been written. Not an error, and by far
		// the common case.
		if err := rows.Err(); err != nil {
			slog.Error("maintenance: switch unreadable; continuing", "err", err)
		}
		return false
	}

	var raw []byte
	if err := rows.Scan(&raw); err != nil {
		slog.Error("maintenance: switch unreadable; continuing", "err", err)
		return false
	}

	var enabled bool
	if err := json.Unmarshal(raw, &enabled); err != nil {
		// A value of another type is a row this platform did not write — the
		// admin surface type-checks every field against its schema. Treating it
		// as "on" would halt the platform's background work on a malformed row.
		slog.Error("maintenance: switch is not a boolean; continuing",
			"err", err)
		return false
	}
	return enabled
}
