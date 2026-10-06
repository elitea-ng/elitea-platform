package execution

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"
)

const (
	// MaxOutboxPublisherBatchSize bounds one database scan and the memory held
	// while its results are dispatched.
	MaxOutboxPublisherBatchSize = 256
	// MaxOutboxPublisherConcurrency bounds concurrent command bus publications from
	// one publisher instance.
	MaxOutboxPublisherConcurrency = 32
	// maxOutboxPublisherFailureBackoff bounds the delay between failed cycles.
	// Production's 250 ms poll reaches a 20-30 second jittered steady-state
	// retry window instead of synchronizing every replica on a tight cadence.
	maxOutboxPublisherFailureBackoff = 30 * time.Second
	maxOutboxPublisherBackoffFloor   = 20 * time.Second
	maxOutboxPublisherFailureCount   = 64
	MinOutboxVisibilityTimeout       = time.Second
	MaxOutboxVisibilityTimeout       = 10 * time.Minute
)

var (
	ErrInvalidPendingOutboxLimit       = errors.New("invalid pending outbox limit")
	ErrInvalidOutboxPublisherConfig    = errors.New("invalid outbox publisher configuration")
	ErrPendingOutboxBatchLimitExceeded = errors.New("pending outbox store exceeded requested limit")
)

// PendingValidationOutbox retires expired pre-authority work and discovers new
// or visibility-expired validation commands. Both operations are bounded by
// the caller's limit and use the durable store's clock. Implementations must
// return no more than limit IDs in deterministic oldest-first order and must
// exclude expired, retired and unsupported retry generations. Discovery does
// not claim ownership: duplicate discovery across publisher instances is
// expected and is handled by the dispatcher contract.
type PendingValidationOutbox interface {
	RetireNoAuthorityValidation(ctx context.Context, limit int) (int, error)
	ListPendingValidationIDs(ctx context.Context, limit int, visibilityTimeout time.Duration) ([]string, error)
}

// ValidationDispatchExecutor is the smallest dispatcher surface required by
// the publisher. Dispatch must preserve the caller's context and the durable
// outbox idempotency contract.
type ValidationDispatchExecutor interface {
	Dispatch(ctx context.Context, outboxID string) error
}

// OutboxPublisherConfig declares hard resource bounds and continuous-run error
// reporting. ReportFailure is called serially after a failed cycle; it must
// return promptly and must not retain sensitive error data.
type OutboxPublisherConfig struct {
	PollInterval      time.Duration
	VisibilityTimeout time.Duration
	BatchSize         int
	MaxConcurrent     int
	ReportFailure     func(error)
}

// OutboxPublisher reconciles durable validation outbox rows with the existing
// reference-command dispatcher. Lifecycle is caller-owned: Run blocks, context
// cancellation stops polling and propagates to active dispatches, and Run does
// not return until the current bounded worker set has exited. A caller that
// starts Run in a goroutine drains it by cancelling the context and waiting for
// Run to return; the publisher owns no background goroutine or hidden queue.
type OutboxPublisher struct {
	outbox     PendingValidationOutbox
	dispatcher ValidationDispatchExecutor
	config     OutboxPublisherConfig
	jitter     func(time.Duration) time.Duration
	wait       func(context.Context, time.Duration) error
}

func NewOutboxPublisher(outbox PendingValidationOutbox, dispatcher ValidationDispatchExecutor, config OutboxPublisherConfig) (*OutboxPublisher, error) {
	if outbox == nil || dispatcher == nil {
		return nil, errors.New("pending outbox and validation dispatcher are required")
	}
	if config.PollInterval <= 0 || config.VisibilityTimeout < MinOutboxVisibilityTimeout || config.VisibilityTimeout > MaxOutboxVisibilityTimeout || config.BatchSize <= 0 || config.BatchSize > MaxOutboxPublisherBatchSize || config.MaxConcurrent <= 0 || config.MaxConcurrent > MaxOutboxPublisherConcurrency || config.MaxConcurrent > config.BatchSize || config.ReportFailure == nil {
		return nil, ErrInvalidOutboxPublisherConfig
	}
	return &OutboxPublisher{
		outbox:     outbox,
		dispatcher: dispatcher,
		config:     config,
		jitter:     randomOutboxPublisherJitter,
		wait:       waitOutboxPublisher,
	}, nil
}

// Run polls immediately. Successful cycles wait PollInterval; consecutive
// failed cycles use bounded exponential backoff plus upward jitter, resetting
// after success. Context cancellation is terminal and retains
// context.Canceled/DeadlineExceeded identity after the active bounded worker
// set exits.
func (p *OutboxPublisher) Run(ctx context.Context) error {
	if ctx == nil {
		return errors.New("outbox publisher context is required")
	}
	consecutiveFailures := 0
	for {
		delay := p.config.PollInterval
		if err := p.RunOnce(ctx); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			if consecutiveFailures < maxOutboxPublisherFailureCount {
				consecutiveFailures++
			}
			p.config.ReportFailure(err)
			delay = outboxPublisherFailureDelay(p.config.PollInterval, consecutiveFailures, p.jitter)
		} else {
			consecutiveFailures = 0
		}

		if err := p.wait(ctx, delay); err != nil {
			return err
		}
	}
}

// RunOnce first retires at most one bounded batch using the database clock,
// then lists and dispatches at most one bounded live batch. Retirement failure
// stops the cycle so an expired row cannot race through a best-effort filter.
// Every listed item is attempted even when a sibling fails. Returned item
// errors are joined in deterministic input order.
func (p *OutboxPublisher) RunOnce(ctx context.Context) error {
	if ctx == nil {
		return errors.New("outbox publisher context is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	retired, err := p.outbox.RetireNoAuthorityValidation(ctx, p.config.BatchSize)
	if err != nil {
		return fmt.Errorf("retire terminal no-authority validation outbox: %w", err)
	}
	if retired < 0 || retired > p.config.BatchSize {
		return ErrPendingOutboxBatchLimitExceeded
	}
	if retired == p.config.BatchSize {
		return nil
	}
	outboxIDs, err := p.outbox.ListPendingValidationIDs(ctx, p.config.BatchSize, p.config.VisibilityTimeout)
	if err != nil {
		return fmt.Errorf("list pending validation outbox: %w", err)
	}
	if len(outboxIDs) > p.config.BatchSize {
		return ErrPendingOutboxBatchLimitExceeded
	}
	return p.dispatchBatch(ctx, outboxIDs)
}

func (p *OutboxPublisher) dispatchBatch(ctx context.Context, outboxIDs []string) error {
	if len(outboxIDs) == 0 {
		return nil
	}

	type batchItem struct {
		index    int
		outboxID string
	}

	workerCount := min(p.config.MaxConcurrent, len(outboxIDs))
	jobs := make(chan batchItem)
	results := make([]error, len(outboxIDs))
	var workers sync.WaitGroup
	workers.Add(workerCount)
	for range workerCount {
		go func() {
			defer workers.Done()
			for item := range jobs {
				if ctx.Err() != nil {
					return
				}
				results[item.index] = p.dispatcher.Dispatch(ctx, item.outboxID)
			}
		}()
	}

sendBatch:
	for index, outboxID := range outboxIDs {
		select {
		case jobs <- batchItem{index: index, outboxID: outboxID}:
		case <-ctx.Done():
			break sendBatch
		}
	}
	close(jobs)
	workers.Wait()

	if err := ctx.Err(); err != nil {
		return err
	}
	itemErrors := make([]error, 0, len(results))
	for index, err := range results {
		if err != nil {
			itemErrors = append(itemErrors, fmt.Errorf("dispatch pending validation outbox %q: %w", outboxIDs[index], err))
		}
	}
	return errors.Join(itemErrors...)
}

func stopTimer(timer *time.Timer) {
	if timer.Stop() {
		return
	}
	select {
	case <-timer.C:
	default:
	}
}

func outboxPublisherFailureDelay(pollInterval time.Duration, consecutiveFailures int, jitter func(time.Duration) time.Duration) time.Duration {
	if consecutiveFailures <= 0 || pollInterval >= maxOutboxPublisherFailureBackoff {
		return pollInterval
	}

	floor := pollInterval
	for failure := 1; failure < consecutiveFailures && floor < maxOutboxPublisherBackoffFloor; failure++ {
		if floor > maxOutboxPublisherBackoffFloor/2 {
			floor = maxOutboxPublisherBackoffFloor
			break
		}
		floor *= 2
	}
	jitterLimit := min(floor/2, maxOutboxPublisherFailureBackoff-floor)
	if jitterLimit <= 0 {
		return floor
	}
	offset := jitter(jitterLimit)
	if offset < 0 || offset > jitterLimit {
		// The jitter source is process-owned, but fail safely if it ever violates
		// its contract: retain backoff without exceeding the declared ceiling.
		return floor
	}
	return floor + offset
}

func randomOutboxPublisherJitter(maximum time.Duration) time.Duration {
	if maximum <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(maximum) + 1))
}

func waitOutboxPublisher(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	select {
	case <-ctx.Done():
		stopTimer(timer)
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
