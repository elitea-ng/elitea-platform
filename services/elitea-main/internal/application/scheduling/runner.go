package scheduling

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// TickResult reports bounded work attempted by one scheduler pass.
type TickResult struct {
	Planned    int
	Claimed    int
	Completed  int
	Released   int
	Stale      int
	PlanLeases int
	Pages      int
}

// Runner materializes and dispatches scheduled occurrences. Every Main replica
// may run it: PostgreSQL planning and occurrence leases provide logical
// ownership and takeover.
type Runner struct {
	store    Store
	registry *Registry
	config   Config
	logger   *slog.Logger
	ticking  atomic.Bool

	// controls is the optional operator registry; nil runs every job on its
	// registered schedule.
	controls   Controls
	registered bool
}

// WithControls attaches the operator registry. Call it before Run.
func (r *Runner) WithControls(controls Controls) *Runner {
	r.controls = controls
	return r
}

// NewRunner validates a bounded scheduler. The registry's lease duration must
// be the same value used here so handler deadlines cannot knowingly exceed
// ownership.
func NewRunner(store Store, registry *Registry, config Config, logger *slog.Logger) (*Runner, error) {
	if store == nil || registry == nil {
		return nil, fmt.Errorf("%w: store and registry are required", ErrInvalidConfiguration)
	}
	config = config.withDefaults()
	if err := config.validate(); err != nil {
		return nil, err
	}
	for _, job := range registry.jobs() {
		if err := job.validate(config.LeaseDuration); err != nil {
			return nil, err
		}
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Runner{store: store, registry: registry, config: config, logger: logger}, nil
}

// Run performs one immediate pass and then ticks until cancellation. A failed
// pass is logged and retried; cancellation is returned with its identity.
func (r *Runner) Run(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("%w: context is required", ErrInvalidConfiguration)
	}
	r.runOnce(ctx)
	ticker := time.NewTicker(r.config.TickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			r.runOnce(ctx)
		}
	}
}

func (r *Runner) runOnce(ctx context.Context) {
	result, err := r.Tick(ctx, time.Time{})
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		r.logger.Error("scheduled jobs tick failed", "err", err)
		return
	}
	if result.Planned > 0 || result.Claimed > 0 {
		r.logger.Info(
			"scheduled jobs tick complete",
			"planned", result.Planned,
			"claimed", result.Claimed,
			"completed", result.Completed,
			"released", result.Released,
			"stale", result.Stale,
		)
	}
}

// Tick runs one bounded pass. Supplying a non-zero time is intended for
// deterministic tests; production passes zero and uses the PostgreSQL clock.
func (r *Runner) Tick(ctx context.Context, suppliedNow time.Time) (TickResult, error) {
	if ctx == nil {
		return TickResult{}, fmt.Errorf("%w: context is required", ErrInvalidConfiguration)
	}
	if !r.ticking.CompareAndSwap(false, true) {
		return TickResult{}, nil
	}
	defer r.ticking.Store(false)

	now, err := r.authoritativeNow(ctx, suppliedNow)
	if err != nil {
		return TickResult{}, err
	}
	controls, controlsOK := r.resolveControls(ctx)
	if !controlsOK {
		// The registry is the operator's word on what may run. When it cannot
		// be read, nothing is planned or dispatched this tick; the next tick
		// retries. The occurrence ledger and the registry share a database, so
		// this costs nothing that a database outage would not.
		return TickResult{}, nil
	}
	result, planErr := r.plan(ctx, now, controls)
	if planErr != nil {
		return result, planErr
	}
	claimable := r.claimableJobs(controls)

	for page := 0; page < r.config.MaxPagesPerTick; page++ {
		claimNow, err := r.authoritativeNow(ctx, suppliedNow)
		if err != nil {
			return result, err
		}
		claimed, err := r.store.ClaimPage(
			ctx,
			claimable,
			r.config.InstanceID,
			claimNow,
			r.config.LeaseDuration,
			r.config.PageSize,
		)
		if err != nil {
			return result, fmt.Errorf("claim scheduled occurrence page: %w", err)
		}
		if len(claimed) == 0 {
			break
		}
		result.Pages++
		result.Claimed += len(claimed)
		pageResult, err := r.dispatchPage(ctx, claimed)
		result.Completed += pageResult.Completed
		result.Released += pageResult.Released
		result.Stale += pageResult.Stale
		if err != nil {
			return result, err
		}
		if len(claimed) < r.config.PageSize {
			break
		}
	}
	return result, nil
}

func (r *Runner) authoritativeNow(ctx context.Context, supplied time.Time) (time.Time, error) {
	if !supplied.IsZero() {
		return supplied.UTC(), nil
	}
	now, err := r.store.Now(ctx)
	if err != nil {
		return time.Time{}, fmt.Errorf("read scheduler database clock: %w", err)
	}
	return now.UTC(), nil
}

func (r *Runner) plan(ctx context.Context, now time.Time, controls map[string]Control) (TickResult, error) {
	var result TickResult
	for _, job := range r.registry.jobs() {
		control := controlFor(controls, job.ID)
		if control.Schedule != nil {
			job.Schedule = control.Schedule
		}
		claim, acquired, err := r.store.ClaimPlanning(
			ctx,
			RegisteredJob{ID: job.ID, Revision: job.Revision},
			r.config.InstanceID,
			now,
			r.config.LeaseDuration,
			r.config.InitialLookback,
		)
		if err != nil {
			return result, fmt.Errorf("claim scheduled job %q planning lease: %w", job.ID, err)
		}
		if !acquired {
			continue
		}
		result.PlanLeases++
		if !control.Enabled {
			// A switched-off job still advances its cursor, with no occurrences:
			// leaving it behind would make switching it back on replay every
			// slot it missed.
			if err := r.store.MaterializeAndAdvance(ctx, claim, nil, now); err != nil && !errors.Is(err, ErrStaleFence) {
				return result, fmt.Errorf("advance disabled scheduled job %q: %w", job.ID, err)
			}
			continue
		}
		seeds, through, err := materialize(job, claim.ObservedThrough, now, r.config.MaxOccurrencesPerPlan)
		if err != nil {
			return result, err
		}
		if err := r.store.MaterializeAndAdvance(ctx, claim, seeds, through); err != nil {
			if errors.Is(err, ErrStaleFence) {
				result.Stale++
				continue
			}
			return result, fmt.Errorf("materialize scheduled job %q: %w", job.ID, err)
		}
		result.Planned += len(seeds)
		r.reportNext(ctx, job, now)
	}
	return result, nil
}

// resolveControls registers the jobs once and reads the operator's standing
// instructions. ok is false when the registry cannot be read.
func (r *Runner) resolveControls(ctx context.Context) (map[string]Control, bool) {
	if r.controls == nil {
		return nil, true
	}
	if !r.registered {
		var offered []Job
		for _, job := range r.registry.jobs() {
			if job.Cadence != "" {
				offered = append(offered, job)
			}
		}
		if err := r.controls.Register(ctx, offered); err != nil {
			r.logger.Error("scheduled jobs: registering with the routine registry failed", "err", err)
			return nil, false
		}
		r.registered = true
	}
	controls, err := r.controls.Resolve(ctx)
	if err != nil {
		r.logger.Error("scheduled jobs: reading the routine registry failed", "err", err)
		return nil, false
	}
	return controls, true
}

// controlFor is the standing instruction for a job: enabled on its registered
// schedule unless the registry says otherwise.
func controlFor(controls map[string]Control, jobID string) Control {
	if control, ok := controls[jobID]; ok {
		return control
	}
	return Control{Enabled: true}
}

// claimableJobs drops the jobs an operator switched off, so their already
// planned occurrences wait in the ledger instead of running.
func (r *Runner) claimableJobs(controls map[string]Control) []RegisteredJob {
	registered := r.registry.registeredJobs()
	if r.controls == nil {
		return registered
	}
	enabled := registered[:0:0]
	for _, job := range registered {
		if controlFor(controls, job.ID).Enabled {
			enabled = append(enabled, job)
		}
	}
	return enabled
}

func (r *Runner) reportNext(ctx context.Context, job Job, now time.Time) {
	if r.controls == nil || job.Cadence == "" {
		return
	}
	next := job.Schedule.Next(now)
	if next.IsZero() {
		return
	}
	if err := r.controls.Report(ctx, RoutineReport{JobID: job.ID, NextRunAt: next}); err != nil {
		r.logger.Warn("scheduled jobs: reporting the next run failed", "job", job.ID, "err", err)
	}
}

func (r *Runner) reportRun(parent context.Context, job Job, outcome string) {
	if r.controls == nil || job.Cadence == "" {
		return
	}
	ctx, cancel := boundedSettlementContext(parent)
	defer cancel()
	report := RoutineReport{JobID: job.ID, RanAt: time.Now().UTC(), Outcome: outcome}
	if err := r.controls.Report(ctx, report); err != nil {
		r.logger.Warn("scheduled jobs: reporting the run failed", "job", job.ID, "err", err)
	}
}

func materialize(job Job, after, through time.Time, limit int) ([]OccurrenceSeed, time.Time, error) {
	if through.Before(after) {
		return nil, after, nil
	}
	seeds := make([]OccurrenceSeed, 0, min(limit, 16))
	cursor := after
	for len(seeds) < limit {
		next := job.Schedule.Next(cursor)
		if next.IsZero() || !next.After(cursor) {
			return nil, after, fmt.Errorf("scheduled job %q returned a non-advancing occurrence", job.ID)
		}
		if next.After(through) {
			return seeds, through, nil
		}
		due := next.UTC()
		seeds = append(seeds, OccurrenceSeed{
			InvocationID:     invocationID(job.ID, job.Revision, due),
			JobID:            job.ID,
			ScheduleRevision: job.Revision,
			DueAt:            due,
			Mode:             job.Mode,
		})
		cursor = next
	}
	return seeds, cursor.UTC(), nil
}

func invocationID(jobID, revision string, dueAt time.Time) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte("elitea.scheduled-occurrence.v1\x00"))
	_, _ = hash.Write([]byte(jobID))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(revision))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(dueAt.UTC().Format(time.RFC3339Nano)))
	return hex.EncodeToString(hash.Sum(nil))
}

type dispatchResult struct {
	Completed int
	Released  int
	Stale     int
}

func (r *Runner) dispatchPage(ctx context.Context, claimed []ClaimedOccurrence) (dispatchResult, error) {
	workers := min(r.config.MaxParallel, len(claimed))
	work := make(chan ClaimedOccurrence)
	results := make(chan dispatchResult, len(claimed))
	errs := make(chan error, len(claimed))
	var wait sync.WaitGroup
	wait.Add(workers)
	for range workers {
		go func() {
			defer wait.Done()
			for occurrence := range work {
				result, err := r.dispatchOne(ctx, occurrence)
				results <- result
				if err != nil {
					errs <- err
				}
			}
		}()
	}
	for _, occurrence := range claimed {
		select {
		case <-ctx.Done():
			close(work)
			wait.Wait()
			close(results)
			close(errs)
			return collectDispatch(results), ctx.Err()
		case work <- occurrence:
		}
	}
	close(work)
	wait.Wait()
	close(results)
	close(errs)
	result := collectDispatch(results)
	var joined error
	for err := range errs {
		joined = errors.Join(joined, err)
	}
	return result, joined
}

func collectDispatch(results <-chan dispatchResult) dispatchResult {
	var combined dispatchResult
	for result := range results {
		combined.Completed += result.Completed
		combined.Released += result.Released
		combined.Stale += result.Stale
	}
	return combined
}

func (r *Runner) dispatchOne(parent context.Context, claimed ClaimedOccurrence) (result dispatchResult, returnedErr error) {
	job, ok := r.registry.job(claimed.JobID, claimed.ScheduleRevision)
	if !ok {
		if err := r.release(parent, claimed, "SCHEDULED_JOB_REVISION_UNAVAILABLE"); err != nil {
			return dispatchResult{}, err
		}
		return dispatchResult{Released: 1}, nil
	}
	if claimed.Mode != job.Mode {
		if err := r.release(parent, claimed, invalidOutcomeErrorCode); err != nil {
			return dispatchResult{}, err
		}
		return dispatchResult{Released: 1}, ErrInvalidOutcome
	}
	handlerCtx, cancel := context.WithTimeout(parent, job.Timeout)
	defer cancel()

	var outcome Outcome
	var handlerErr error
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				handlerErr = errors.New("scheduled handler panicked")
			}
		}()
		outcome, handlerErr = job.Handler.Execute(handlerCtx, claimed.Occurrence)
	}()
	if handlerErr != nil {
		code := defaultSafeFailureCode
		if errors.Is(handlerErr, context.DeadlineExceeded) || errors.Is(handlerCtx.Err(), context.DeadlineExceeded) {
			code = "SCHEDULED_HANDLER_DEADLINE_EXCEEDED"
		} else if errors.Is(handlerErr, context.Canceled) || errors.Is(handlerCtx.Err(), context.Canceled) {
			code = "SCHEDULED_HANDLER_CANCELLED"
		}
		if err := r.release(parent, claimed, code); err != nil {
			return dispatchResult{}, err
		}
		r.reportRun(parent, job, "error")
		return dispatchResult{Released: 1}, nil
	}
	if !outcomeMatches(job.Mode, outcome) {
		if err := r.release(parent, claimed, invalidOutcomeErrorCode); err != nil {
			return dispatchResult{}, err
		}
		return dispatchResult{Released: 1}, ErrInvalidOutcome
	}
	settlementCtx, settlementCancel := boundedSettlementContext(parent)
	defer settlementCancel()
	if err := r.store.Complete(settlementCtx, claimed, outcome); err != nil {
		if errors.Is(err, ErrStaleFence) {
			return dispatchResult{Stale: 1}, nil
		}
		return dispatchResult{}, fmt.Errorf("complete scheduled occurrence: %w", err)
	}
	r.reportRun(parent, job, "ok")
	return dispatchResult{Completed: 1}, nil
}

func (r *Runner) release(ctx context.Context, claimed ClaimedOccurrence, code string) error {
	cleanupCtx, cancel := boundedSettlementContext(ctx)
	defer cancel()
	if err := r.store.ReleaseForRetry(cleanupCtx, claimed, code, r.config.RetryDelay); err != nil {
		if errors.Is(err, ErrStaleFence) {
			return nil
		}
		return fmt.Errorf("release scheduled occurrence: %w", err)
	}
	return nil
}

func boundedSettlementContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), defaultSettlementTimeout)
}

func outcomeMatches(mode Mode, outcome Outcome) bool {
	return (mode == ModeLocalBounded && outcome == OutcomeLocalCompleted) ||
		(mode == ModeDurableAdmission && outcome == OutcomeDurablyAdmitted)
}

var _ interface{ Run(context.Context) error } = (*Runner)(nil)
