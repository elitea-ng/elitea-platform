package runtimecomposition

import (
	"context"
	"errors"
	"time"

	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
)

const maxCurrentIndexMetaTerminalBackoff = 30 * time.Second

type currentIndexMetaTerminalReconciliationService interface {
	ReconcilePendingIndexMetaTerminals(context.Context, int) (int, error)
}

type currentIndexMetaTerminalReconciler struct {
	service       currentIndexMetaTerminalReconciliationService
	pollInterval  time.Duration
	batchSize     int
	reportFailure func(error)
	wait          func(context.Context, time.Duration) error
}

func newCurrentIndexMetaTerminalReconciler(
	service currentIndexMetaTerminalReconciliationService,
	pollInterval time.Duration,
	batchSize int,
	reportFailure func(error),
) (*currentIndexMetaTerminalReconciler, error) {
	if service == nil || pollInterval <= 0 || batchSize <= 0 ||
		batchSize > executionapp.MaxOutboxPublisherBatchSize ||
		reportFailure == nil {
		return nil, errors.New("current index metadata terminal reconciler configuration is invalid")
	}
	return &currentIndexMetaTerminalReconciler{
		service:       service,
		pollInterval:  pollInterval,
		batchSize:     batchSize,
		reportFailure: reportFailure,
		wait:          waitCurrentIndexMetaTerminalReconciler,
	}, nil
}

func (r *currentIndexMetaTerminalReconciler) RunOnce(ctx context.Context) error {
	if r == nil || r.service == nil || ctx == nil {
		return errors.New("current index metadata terminal reconciler is incomplete")
	}
	_, err := r.service.ReconcilePendingIndexMetaTerminals(ctx, r.batchSize)
	return err
}

func (r *currentIndexMetaTerminalReconciler) Run(ctx context.Context) error {
	if r == nil || r.service == nil || r.wait == nil || ctx == nil {
		return errors.New("current index metadata terminal reconciler is incomplete")
	}
	failures := 0
	for {
		delay := r.pollInterval
		if err := r.RunOnce(ctx); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			failures++
			r.reportFailure(err)
			delay = currentIndexMetaTerminalFailureDelay(r.pollInterval, failures)
		} else {
			failures = 0
		}
		if err := r.wait(ctx, delay); err != nil {
			return err
		}
	}
}

func currentIndexMetaTerminalFailureDelay(base time.Duration, failures int) time.Duration {
	if failures <= 0 || base >= maxCurrentIndexMetaTerminalBackoff {
		return base
	}
	delay := base
	for step := 1; step < failures && delay < maxCurrentIndexMetaTerminalBackoff; step++ {
		if delay > maxCurrentIndexMetaTerminalBackoff/2 {
			return maxCurrentIndexMetaTerminalBackoff
		}
		delay *= 2
	}
	return min(delay, maxCurrentIndexMetaTerminalBackoff)
}

func waitCurrentIndexMetaTerminalReconciler(
	ctx context.Context,
	delay time.Duration,
) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

var _ publisherRunner = (*currentIndexMetaTerminalReconciler)(nil)
