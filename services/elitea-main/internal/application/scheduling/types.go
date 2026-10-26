// Package scheduling owns capability-neutral scheduled occurrence planning and
// dispatch. Product packages register typed jobs; this package does not inspect
// product schemas or execute worker commands directly.
package scheduling

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"
)

const (
	MaxRegisteredJobs        = 256
	MaxParallelism           = 64
	MaxPageSize              = 256
	MaxPagesPerTick          = 16
	MaxOccurrencesPerPlan    = 256
	MaxIdentifierBytes       = 128
	MaxRetryDelay            = time.Hour
	defaultTickInterval      = time.Minute
	defaultLeaseDuration     = 2 * time.Minute
	defaultHandlerTimeout    = 30 * time.Second
	defaultInitialLookback   = time.Minute
	defaultRetryDelay        = 5 * time.Second
	defaultSettlementTimeout = 5 * time.Second
	defaultMaxParallel       = 8
	defaultPageSize          = 64
	defaultMaxPagesPerTick   = 4
	defaultMaxOccurrences    = 64
	defaultSafeFailureCode   = "SCHEDULED_HANDLER_FAILED"
	invalidOutcomeErrorCode  = "SCHEDULED_INVALID_OUTCOME"
)

var identifierPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)

var (
	ErrInvalidConfiguration = errors.New("invalid scheduler configuration")
	ErrDuplicateJob         = errors.New("duplicate scheduled job")
	ErrUnknownJob           = errors.New("unknown scheduled job")
	ErrStaleFence           = errors.New("stale scheduled occurrence fence")
	ErrInvalidOutcome       = errors.New("scheduled handler returned an outcome incompatible with its mode")
)

// Mode declares the only success outcome a handler may return. LocalBounded is
// for bounded, idempotent Go work. DurableAdmission is for work whose ACK means
// a separate durable execution was admitted, not that the execution completed.
type Mode string

const (
	ModeLocalBounded     Mode = "local_bounded"
	ModeDurableAdmission Mode = "durable_admission"
)

// Outcome is persisted only after a handler returns successfully with the
// outcome required by its registered mode.
type Outcome string

const (
	OutcomeLocalCompleted  Outcome = "local_completed"
	OutcomeDurablyAdmitted Outcome = "durably_admitted"
)

// Schedule returns the first occurrence strictly after the supplied instant.
// Implementations must be deterministic for a given revision.
type Schedule interface {
	Next(time.Time) time.Time
}

// Handler executes one fenced occurrence. Implementations must use
// InvocationID as their idempotency key and must not retain Occurrence secrets;
// the occurrence contains identifiers only.
type Handler interface {
	Execute(context.Context, Occurrence) (Outcome, error)
}

// HandlerFunc adapts a function to Handler.
type HandlerFunc func(context.Context, Occurrence) (Outcome, error)

func (f HandlerFunc) Execute(ctx context.Context, occurrence Occurrence) (Outcome, error) {
	return f(ctx, occurrence)
}

// Job is an immutable registered scheduled capability.
type Job struct {
	ID       string
	Revision string
	Mode     Mode
	Schedule Schedule
	Timeout  time.Duration
	Handler  Handler

	// Description, Cadence and MinInterval describe the job to the operator
	// registry (see Controls). Cadence is the text of Schedule: a registry row
	// shows and edits text, and Schedule alone cannot be printed. A job with an
	// empty Cadence is not offered to the registry and keeps running on its
	// code schedule.
	Description string
	Cadence     string
	MinInterval time.Duration
}

func (j Job) validate(leaseDuration time.Duration) error {
	if !identifierPattern.MatchString(j.ID) {
		return fmt.Errorf("%w: job ID must be a bounded canonical identifier", ErrInvalidConfiguration)
	}
	if len(j.Revision) == 0 || len(j.Revision) > MaxIdentifierBytes {
		return fmt.Errorf("%w: job %q revision must contain 1..%d bytes", ErrInvalidConfiguration, j.ID, MaxIdentifierBytes)
	}
	if j.Mode != ModeLocalBounded && j.Mode != ModeDurableAdmission {
		return fmt.Errorf("%w: job %q has unsupported mode %q", ErrInvalidConfiguration, j.ID, j.Mode)
	}
	if j.Schedule == nil || j.Handler == nil {
		return fmt.Errorf("%w: job %q requires a schedule and handler", ErrInvalidConfiguration, j.ID)
	}
	if j.Timeout <= 0 || j.Timeout >= leaseDuration {
		return fmt.Errorf("%w: job %q timeout must be positive and shorter than the lease", ErrInvalidConfiguration, j.ID)
	}
	return nil
}

// Occurrence is the reference-only context supplied to a typed handler.
type Occurrence struct {
	InvocationID     string
	JobID            string
	ScheduleRevision string
	DueAt            time.Time
	LeaseEpoch       int64
	ClaimFence       string
}

// PlanningClaim fences one replica while it materializes due occurrences for a
// job. Store implementations compare every field before advancing the cursor.
type PlanningClaim struct {
	JobID            string
	ScheduleRevision string
	ObservedThrough  time.Time
	LeaseEpoch       int64
	ClaimFence       [32]byte
}

// OccurrenceSeed is immutable scheduled intent persisted before dispatch.
type OccurrenceSeed struct {
	InvocationID     string
	JobID            string
	ScheduleRevision string
	DueAt            time.Time
	Mode             Mode
}

// ClaimedOccurrence is a per-attempt fenced claim.
type ClaimedOccurrence struct {
	Occurrence
	Mode       Mode
	ClaimBytes [32]byte
}

// RegisteredJob identifies the exact revisions a replica can dispatch.
type RegisteredJob struct {
	ID       string
	Revision string
}

// Store is the PostgreSQL authority boundary. ClaimPlanning and
// MaterializeAndAdvance provide a fenced planning lease; ClaimPage and Complete
// provide per-occurrence takeover and settlement.
type Store interface {
	Now(context.Context) (time.Time, error)
	ClaimPlanning(
		context.Context,
		RegisteredJob,
		string,
		time.Time,
		time.Duration,
		time.Duration,
	) (PlanningClaim, bool, error)
	MaterializeAndAdvance(
		context.Context,
		PlanningClaim,
		[]OccurrenceSeed,
		time.Time,
	) error
	// DisableAndAdvance supersedes the job's pending occurrences that no
	// replica holds a live lease on and advances the cursor to the supplied
	// time, under the same fence as MaterializeAndAdvance.
	DisableAndAdvance(context.Context, PlanningClaim, time.Time) error
	ClaimPage(
		context.Context,
		[]RegisteredJob,
		string,
		time.Time,
		time.Duration,
		int,
	) ([]ClaimedOccurrence, error)
	Complete(context.Context, ClaimedOccurrence, Outcome) error
	ReleaseForRetry(context.Context, ClaimedOccurrence, string, time.Duration) error
}

// Control is an operator's standing instruction for one job.
type Control struct {
	// Enabled false stops the job from being planned or claimed.
	Enabled bool
	// Schedule replaces the job's registered schedule when non-nil.
	Schedule Schedule
	// NextRunAt is the next run the registry shows, so the runner rewrites it
	// only when it changes.
	NextRunAt *time.Time
}

// RoutineReport is what a runner tells the registry after a tick. Zero fields
// are not written.
type RoutineReport struct {
	JobID string
	// NextRunAt is the job's next planned occurrence, for display.
	NextRunAt time.Time
	// RanAt and Outcome describe a finished occurrence.
	RanAt   time.Time
	Outcome string
}

// Controls is the operator registry (issue #1369) a Runner consults. Register
// is called with every job that has a Cadence; Resolve returns the standing
// instruction per job id; Report records what happened. A job the registry
// does not know yet resolves as enabled on its code schedule.
type Controls interface {
	Register(context.Context, []Job) error
	Resolve(context.Context) (map[string]Control, error)
	Report(context.Context, RoutineReport) error
}

// Config owns all scheduler resource bounds.
type Config struct {
	InstanceID            string
	TickInterval          time.Duration
	LeaseDuration         time.Duration
	InitialLookback       time.Duration
	RetryDelay            time.Duration
	MaxParallel           int
	PageSize              int
	MaxPagesPerTick       int
	MaxOccurrencesPerPlan int
}

func (c Config) withDefaults() Config {
	if c.TickInterval == 0 {
		c.TickInterval = defaultTickInterval
	}
	if c.LeaseDuration == 0 {
		c.LeaseDuration = defaultLeaseDuration
	}
	if c.InitialLookback == 0 {
		c.InitialLookback = defaultInitialLookback
	}
	if c.RetryDelay == 0 {
		c.RetryDelay = defaultRetryDelay
	}
	if c.MaxParallel == 0 {
		c.MaxParallel = defaultMaxParallel
	}
	if c.PageSize == 0 {
		c.PageSize = defaultPageSize
	}
	if c.MaxPagesPerTick == 0 {
		c.MaxPagesPerTick = defaultMaxPagesPerTick
	}
	if c.MaxOccurrencesPerPlan == 0 {
		c.MaxOccurrencesPerPlan = defaultMaxOccurrences
	}
	return c
}

func (c Config) validate() error {
	if !identifierPattern.MatchString(c.InstanceID) {
		return fmt.Errorf("%w: instance ID must be a bounded canonical identifier", ErrInvalidConfiguration)
	}
	if c.TickInterval <= 0 || c.LeaseDuration <= 0 || c.InitialLookback <= 0 {
		return fmt.Errorf("%w: tick, lease and initial lookback must be positive", ErrInvalidConfiguration)
	}
	if c.RetryDelay < time.Millisecond || c.RetryDelay > MaxRetryDelay {
		return fmt.Errorf("%w: retry delay must be between 1ms and %s", ErrInvalidConfiguration, MaxRetryDelay)
	}
	if c.MaxParallel < 1 || c.MaxParallel > MaxParallelism {
		return fmt.Errorf("%w: max parallel must be in 1..%d", ErrInvalidConfiguration, MaxParallelism)
	}
	if c.PageSize < 1 || c.PageSize > MaxPageSize {
		return fmt.Errorf("%w: page size must be in 1..%d", ErrInvalidConfiguration, MaxPageSize)
	}
	if c.MaxPagesPerTick < 1 || c.MaxPagesPerTick > MaxPagesPerTick {
		return fmt.Errorf("%w: max pages per tick must be in 1..%d", ErrInvalidConfiguration, MaxPagesPerTick)
	}
	if c.MaxOccurrencesPerPlan < 1 || c.MaxOccurrencesPerPlan > MaxOccurrencesPerPlan {
		return fmt.Errorf("%w: max occurrences per plan must be in 1..%d", ErrInvalidConfiguration, MaxOccurrencesPerPlan)
	}
	return nil
}
