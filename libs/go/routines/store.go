package routines

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Owner names the process that runs a routine.
type Owner string

const (
	// OwnerMain is a job on elitea-main's platform scheduler.
	OwnerMain Owner = "main"
	// OwnerScheduler is a worker of the elitea-scheduler process.
	OwnerScheduler Owner = "scheduler"
)

// The outcome words a run leaves in `last_outcome`. They are the only values
// the column holds, so the admin screen can translate them.
const (
	OutcomeOK          = "ok"
	OutcomeError       = "error"
	OutcomeMaintenance = "maintenance"
)

// Definition is what a routine registers about itself at startup.
type Definition struct {
	ID          string
	Owner       Owner
	Description string
	// DefaultSchedule is the code's (env/values) schedule, used until an
	// operator overrides it.
	DefaultSchedule string
	// MinInterval is the shortest gap an operator may configure. Zero means no
	// bound beyond the schedule parsing.
	MinInterval time.Duration
}

// Row is one registry row.
type Row struct {
	Definition
	ScheduleOverride *string
	Enabled          bool
	LastRunAt        *time.Time
	LastOutcome      *string
	NextRunAt        *time.Time
	UpdatedBy        *string
	UpdatedAt        time.Time
}

// EffectiveSchedule is the operator's override when there is one, else the
// default.
func (r Row) EffectiveSchedule() string {
	if r.ScheduleOverride != nil && *r.ScheduleOverride != "" {
		return *r.ScheduleOverride
	}
	return r.DefaultSchedule
}

// Overridden reports whether an operator's schedule is in force.
func (r Row) Overridden() bool {
	return r.ScheduleOverride != nil && *r.ScheduleOverride != ""
}

var (
	// ErrNotFound reports an id with no row. A routine appears only after its
	// owner has started once.
	ErrNotFound = errors.New("platform routine not found")
	// ErrConflict reports a row changed by someone else between the read and
	// the write of an update.
	ErrConflict = errors.New("platform routine changed concurrently")
	// ErrNothingToUpdate reports a patch that carries no field.
	ErrNothingToUpdate = errors.New("no updatable field supplied")
)

// DB is the slice of a pgx pool the store needs.
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Store reads and writes `elitea_runtime.platform_routines`.
type Store struct{ db DB }

// NewStore returns a store over db.
func NewStore(db DB) *Store { return &Store{db: db} }

const rowColumns = `id, owner, description, default_schedule, min_interval_seconds,
	schedule_override, enabled, last_run_at, last_outcome, next_run_at, updated_by, updated_at`

func scanRow(row pgx.Row) (Row, error) {
	var (
		out     Row
		owner   string
		minSecs int64
	)
	err := row.Scan(&out.ID, &owner, &out.Description, &out.DefaultSchedule, &minSecs,
		&out.ScheduleOverride, &out.Enabled, &out.LastRunAt, &out.LastOutcome,
		&out.NextRunAt, &out.UpdatedBy, &out.UpdatedAt)
	if err != nil {
		return Row{}, err
	}
	out.Owner = Owner(owner)
	out.MinInterval = time.Duration(minSecs) * time.Second
	return out, nil
}

// Register records the routines' identity and default schedule. An existing
// row keeps the operator's override, enabled flag and run history: only what
// the code says about itself is refreshed.
func (s *Store) Register(ctx context.Context, definitions ...Definition) error {
	for _, d := range definitions {
		if strings.TrimSpace(d.ID) == "" || (d.Owner != OwnerMain && d.Owner != OwnerScheduler) {
			return fmt.Errorf("routines: definition %q needs an id and a known owner", d.ID)
		}
		if _, err := ParseSchedule(d.DefaultSchedule); err != nil {
			return fmt.Errorf("routines: default schedule of %q: %w", d.ID, err)
		}
		_, err := s.db.Exec(ctx, `
INSERT INTO elitea_runtime.platform_routines
    (id, owner, description, default_schedule, min_interval_seconds)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (id) DO UPDATE SET
    owner = EXCLUDED.owner,
    description = EXCLUDED.description,
    default_schedule = EXCLUDED.default_schedule,
    min_interval_seconds = EXCLUDED.min_interval_seconds`,
			d.ID, string(d.Owner), d.Description, d.DefaultSchedule, int64(d.MinInterval/time.Second))
		if err != nil {
			return fmt.Errorf("routines: register %q: %w", d.ID, err)
		}
	}
	return nil
}

// List returns every routine, ordered by owner then id.
func (s *Store) List(ctx context.Context) ([]Row, error) {
	rows, err := s.db.Query(ctx, `SELECT `+rowColumns+
		` FROM elitea_runtime.platform_routines ORDER BY owner, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Row, 0, 8)
	for rows.Next() {
		row, err := scanRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// Get returns one routine.
func (s *Store) Get(ctx context.Context, id string) (Row, error) {
	row, err := scanRow(s.db.QueryRow(ctx, `SELECT `+rowColumns+
		` FROM elitea_runtime.platform_routines WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Row{}, ErrNotFound
	}
	return row, err
}

// Patch is an operator's edit. Nil fields are left alone.
type Patch struct {
	Enabled *bool
	// Schedule replaces the override; an empty string removes it, which puts
	// the default back in force.
	Schedule *string
	// Actor is recorded as `updated_by`.
	Actor string
}

// Update validates and applies a patch. The schedule is checked against the
// routine's own minimum interval with the same parser the runners use, so what
// is accepted here is what will run. A change of schedule, or turning a
// routine back on, re-plans `next_run_at` from now instead of firing at once.
func (s *Store) Update(ctx context.Context, id string, patch Patch, now time.Time) (Row, error) {
	if patch.Enabled == nil && patch.Schedule == nil {
		return Row{}, ErrNothingToUpdate
	}
	current, err := s.Get(ctx, id)
	if err != nil {
		return Row{}, err
	}
	enabled := current.Enabled
	if patch.Enabled != nil {
		enabled = *patch.Enabled
	}
	override := current.ScheduleOverride
	if patch.Schedule != nil {
		text := strings.TrimSpace(*patch.Schedule)
		if text == "" {
			override = nil
		} else {
			if _, err := ValidateSchedule(text, current.MinInterval, now); err != nil {
				return Row{}, err
			}
			override = &text
		}
	}
	candidate := current
	candidate.ScheduleOverride = override
	scheduleChanged := candidate.EffectiveSchedule() != current.EffectiveSchedule()

	next := current.NextRunAt
	switch {
	case !enabled:
		next = nil
	case scheduleChanged || !current.Enabled:
		parsed, err := ParseSchedule(candidate.EffectiveSchedule())
		if err != nil {
			return Row{}, err
		}
		// A schedule that never fires (only a default can get here: overrides
		// are validated above) leaves nothing planned rather than year 1.
		next = nil
		if planned := parsed.Next(now); !planned.IsZero() {
			next = &planned
		}
	}

	updated, err := scanRow(s.db.QueryRow(ctx, `
UPDATE elitea_runtime.platform_routines
SET enabled = $2, schedule_override = $3, next_run_at = $4,
    updated_by = $5, updated_at = clock_timestamp()
WHERE id = $1 AND updated_at = $6
RETURNING `+rowColumns,
		id, enabled, override, next, patch.Actor, current.UpdatedAt))
	if errors.Is(err, pgx.ErrNoRows) {
		return Row{}, ErrConflict
	}
	return updated, err
}

// Lease is a replica's hold on a routine for the length of one pass (#1414).
// Token is the fencing token: every acquisition increments it, so a holder
// whose lease ran out and was taken over can no longer renew or release.
type Lease struct {
	Token  int64
	Holder string
}

// Completion is what a released pass leaves on the row.
type Completion struct {
	// RanAt and Outcome become last_run_at and last_outcome.
	RanAt   time.Time
	Outcome string
	// Held is the slot the acquisition moved next_run_at to; Next replaces it
	// when next_run_at still holds it and Next is later. A zero Next leaves
	// next_run_at alone.
	Held time.Time
	Next time.Time
}

// ErrLeaseLost reports a renewal or release under a token the row no longer
// holds: the lease ran out and another replica took the routine over.
var ErrLeaseLost = errors.New("platform routine lease lost")

// Acquire claims a due slot and the lease to run it, in one statement. It
// moves `next_run_at` from expected to held, but only if the routine is still
// enabled, nobody moved the slot first, and no other pass holds a live lease.
// The last guard is what keeps a pass that outlasts its interval from starting
// again on another replica; the lease's expiry is what lets a replica take a
// crashed holder's routine over. Times are the database's clock.
func (s *Store) Acquire(
	ctx context.Context, id string, expected *time.Time, held time.Time, holder string, ttl time.Duration,
) (Lease, bool, error) {
	var token int64
	err := s.db.QueryRow(ctx, `
UPDATE elitea_runtime.platform_routines
SET next_run_at = $3,
    lease_token = lease_token + 1,
    lease_holder = $4,
    lease_expires_at = clock_timestamp() + $5::bigint * interval '1 millisecond'
WHERE id = $1 AND enabled
  AND next_run_at IS NOT DISTINCT FROM $2::timestamptz
  AND (lease_expires_at IS NULL OR lease_expires_at <= clock_timestamp())
RETURNING lease_token`,
		id, expected, held, holder, ttl.Milliseconds()).Scan(&token)
	if errors.Is(err, pgx.ErrNoRows) {
		return Lease{}, false, nil
	}
	if err != nil {
		return Lease{}, false, err
	}
	return Lease{Token: token, Holder: holder}, true, nil
}

// Renew extends a held lease by ttl from now. It answers [ErrLeaseLost] when
// the row's token is no longer the lease's, or when the lease has already run
// out: expiry is final, even if nobody has taken the routine over yet, because
// another replica may be acquiring it at this moment.
func (s *Store) Renew(ctx context.Context, id string, lease Lease, ttl time.Duration) error {
	tag, err := s.db.Exec(ctx, `
UPDATE elitea_runtime.platform_routines
SET lease_expires_at = clock_timestamp() + $3::bigint * interval '1 millisecond'
WHERE id = $1 AND lease_token = $2 AND lease_expires_at > clock_timestamp()`, id, lease.Token, ttl.Milliseconds())
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrLeaseLost
	}
	return nil
}

// Release ends a pass: it records the outcome, plans the next slot and drops
// the lease, all under the lease's token. It answers [ErrLeaseLost], and
// writes nothing, when another replica has taken the routine over.
func (s *Store) Release(ctx context.Context, id string, lease Lease, done Completion) error {
	var next *time.Time
	if !done.Next.IsZero() {
		next = &done.Next
	}
	tag, err := s.db.Exec(ctx, `
UPDATE elitea_runtime.platform_routines
SET last_run_at = $3,
    last_outcome = $4,
    next_run_at = CASE
        WHEN $6::timestamptz IS NOT NULL AND next_run_at = $5::timestamptz AND $6::timestamptz > $5::timestamptz
        THEN $6::timestamptz ELSE next_run_at END,
    lease_expires_at = NULL
WHERE id = $1 AND lease_token = $2`,
		id, lease.Token, done.RanAt, done.Outcome, done.Held, next)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrLeaseLost
	}
	return nil
}

// RecordRun stores the outcome of a run of a routine that holds no lease (a
// job on elitea-main's platform scheduler). A [Loop] records through
// [Store.Release] instead, under its lease's token.
func (s *Store) RecordRun(ctx context.Context, id string, at time.Time, outcome string) error {
	_, err := s.db.Exec(ctx, `
UPDATE elitea_runtime.platform_routines
SET last_run_at = $2, last_outcome = $3
WHERE id = $1`, id, at, outcome)
	return err
}

// ObserveNext stores the next planned run of a routine whose occurrences
// another component plans (the platform scheduler's cursor), for display.
func (s *Store) ObserveNext(ctx context.Context, id string, next time.Time) error {
	_, err := s.db.Exec(ctx, `
UPDATE elitea_runtime.platform_routines SET next_run_at = $2 WHERE id = $1`, id, next)
	return err
}
