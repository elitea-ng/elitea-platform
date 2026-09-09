package pipelinetriggers

// The platform-scheduler job that drives the tick.
//
// It is a `schedulingapp.Handler`, registered exactly like `index.schedule.
// scan.v1` and the artifact retention sweep, so the framework owns the clock,
// the cross-replica occurrence lease, the claim fence and the catch-up budget.
// Nothing in this file re-implements any of those. `schedulerun.go` owns the
// product decisions and nothing else.
//
// # MODE IS DURABLE ADMISSION, NOT LOCAL BOUNDED
//
// A tick's ACK means "the due pipeline runs were admitted", not "they
// finished": a pipeline may run for minutes on a worker long after this handler
// returns. `ModeLocalBounded` would claim the opposite, and the framework
// enforces the distinction — a handler returning the wrong outcome for its mode
// is refused with ErrInvalidOutcome rather than silently accepted.
//
// # A SUPPRESSED TICK IS A SUCCESS
//
// A maintenance window makes the pass start nothing. That is the outcome an
// operator asked for, so the occurrence completes rather than failing and
// retrying: a retry would re-read the same switch, get the same answer, and
// burn the framework's catch-up budget on a decision that will not change until
// the window closes.

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	schedulingapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/scheduling"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/audit"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

// The job's registered identity. The cadence is every minute, which is the
// finest resolution a five-field cron can express and therefore the coarsest
// tick that can honour one.
const (
	ScheduleJobID       = "pipeline.schedule.scan.v1"
	ScheduleJobRevision = "pipeline-schedule-scan-r1"
	ScheduleJobCadence  = "* * * * *"
	// ScheduleJobTimeout bounds one pass. It sits under the framework's
	// two-minute lease so a handler deadline can never knowingly outlive
	// ownership of the occurrence it is working on.
	ScheduleJobTimeout = 45 * time.Second
)

// ErrScheduleTickIncomplete reports a pass that could not visit everything it
// was asked to. It is an ERROR and not a quiet success: a tick that keeps
// truncating means the platform has more due schedules than one pass can carry,
// and that is an operator's problem to see.
var ErrScheduleTickIncomplete = errors.New("pipelinetriggers: the schedule pass did not complete")

// ScheduleJob is the handler the platform scheduler registers.
type ScheduleJob struct {
	handler *Handler
}

// NewScheduleJob wraps a handler as a scheduled job.
func NewScheduleJob(handler *Handler) (*ScheduleJob, error) {
	if handler == nil || handler.pool == nil {
		return nil, errors.New("pipelinetriggers: a handler with a database is required for the schedule job")
	}
	return &ScheduleJob{handler: handler}, nil
}

// Name reports the registered job id.
func (*ScheduleJob) Name() string { return ScheduleJobID }

// Execute performs one pass.
//
// The occurrence is validated the way the index scan validates its own: a job
// id, revision, due time, lease epoch and claim fence that do not match this
// registration mean the framework handed this handler somebody else's work, and
// running it would be worse than refusing it.
func (job *ScheduleJob) Execute(
	ctx context.Context, occurrence schedulingapp.Occurrence,
) (schedulingapp.Outcome, error) {
	if job == nil || job.handler == nil || ctx == nil ||
		occurrence.InvocationID == "" ||
		occurrence.JobID != ScheduleJobID ||
		occurrence.ScheduleRevision != ScheduleJobRevision ||
		occurrence.DueAt.IsZero() ||
		occurrence.LeaseEpoch <= 0 ||
		occurrence.ClaimFence == "" {
		return "", errors.New("pipelinetriggers: invalid scheduled occurrence")
	}
	result, err := job.handler.RunDueSchedules(ctx, occurrence.DueAt)
	if err != nil {
		return "", err
	}
	if result.Truncated {
		return "", ErrScheduleTickIncomplete
	}
	return schedulingapp.OutcomeDurablyAdmitted, nil
}

var _ schedulingapp.Handler = (*ScheduleJob)(nil)

// NewPlatformHandler builds the handler both call sites use.
//
// There are two of them — the router, which serves the settings and inbound
// routes, and the composition that runs the schedule tick — and they must agree
// on every dependency. One constructor is what keeps them agreeing: a tick
// wired without the permission resolver, say, would refuse every scheduled run
// while the HTTP half kept working, and nothing would report the difference.
func NewPlatformHandler(
	pool *pgxpool.Pool,
	start AgentStartUseCase,
	vault HiddenVault,
	permissions auth.PermissionResolver,
	recorder audit.Recorder,
	logger *slog.Logger,
	events EventEmitter,
) *Handler {
	options := []Option{
		WithPermissions(permissions),
		WithAuditRecorder(recorder),
		WithLogger(logger),
	}
	// The two checks are not ceremony, and neither is REFLECTION here. Go boxes
	// a typed nil pointer into a NON-NIL interface, so `start != nil` is true
	// for an absent dependency that arrived as `(*T)(nil)` — and every
	// `if h.start == nil` downstream would then read as "configured" and turn
	// an honest 503 into a nil dereference. This is the #86 trap, which this
	// repository has now met at four composition roots, and a plain nil check
	// is exactly the version of it that does not work.
	if present(start) {
		options = append(options, WithAgentStart(start))
	}
	if present(vault) {
		options = append(options, WithVault(vault))
	}
	// pipeline.run.started and schedule.fired (#876's second half). WithEvents
	// itself repeats this present() check — it is repeated here only because
	// every other optional dependency in this constructor filters before
	// appending, and a reader diffing this function against WithAgentStart's
	// and WithVault's neighbouring lines should see the same shape for the
	// same reason.
	if present(events) {
		options = append(options, WithEvents(events))
	}
	return NewHandler(pool, options...)
}

// present reports whether an interface value holds something usable.
//
// It answers false for a nil interface AND for an interface holding a nil
// pointer, map, slice, channel or function. Only the pointer case is reachable
// from this package's own call sites today; the rest are covered because the
// question this function answers is "is there something behind this", and a
// partial answer to that is how the trap gets back in.
func present(value any) bool {
	if value == nil {
		return false
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Ptr, reflect.Map, reflect.Slice, reflect.Chan, reflect.Func, reflect.Interface:
		return !reflected.IsNil()
	default:
		return true
	}
}
