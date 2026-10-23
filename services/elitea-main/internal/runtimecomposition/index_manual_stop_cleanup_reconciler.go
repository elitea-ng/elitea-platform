package runtimecomposition

import (
	"context"
	"errors"
	"time"

	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
)

type currentIndexManualStopCleanupService interface {
	ReconcilePendingManualStopCleanups(context.Context, int) (int, error)
}

type currentIndexManualStopCleanupReconciler struct {
	service       currentIndexManualStopCleanupService
	pollInterval  time.Duration
	batchSize     int
	reportFailure func(error)
	wait          func(context.Context, time.Duration) error
}

func newCurrentIndexManualStopCleanupReconciler(
	service currentIndexManualStopCleanupService,
	pollInterval time.Duration,
	batchSize int,
	reportFailure func(error),
) (*currentIndexManualStopCleanupReconciler, error) {
	if service == nil || pollInterval <= 0 || batchSize <= 0 ||
		batchSize > executionapp.MaxOutboxPublisherBatchSize ||
		reportFailure == nil {
		return nil, errors.New(
			"current index manual Stop cleanup reconciler configuration is invalid",
		)
	}
	return &currentIndexManualStopCleanupReconciler{
		service:       service,
		pollInterval:  pollInterval,
		batchSize:     batchSize,
		reportFailure: reportFailure,
		wait:          waitCurrentIndexMetaTerminalReconciler,
	}, nil
}

func (r *currentIndexManualStopCleanupReconciler) RunOnce(
	ctx context.Context,
) error {
	if r == nil || r.service == nil || ctx == nil {
		return errors.New(
			"current index manual Stop cleanup reconciler is incomplete",
		)
	}
	_, err := r.service.ReconcilePendingManualStopCleanups(
		ctx,
		r.batchSize,
	)
	return err
}

func (r *currentIndexManualStopCleanupReconciler) Run(
	ctx context.Context,
) error {
	if r == nil || r.service == nil || r.wait == nil || ctx == nil {
		return errors.New(
			"current index manual Stop cleanup reconciler is incomplete",
		)
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
			delay = currentIndexMetaTerminalFailureDelay(
				r.pollInterval,
				failures,
			)
		} else {
			failures = 0
		}
		if err := r.wait(ctx, delay); err != nil {
			return err
		}
	}
}

var _ publisherRunner = (*currentIndexManualStopCleanupReconciler)(nil)
