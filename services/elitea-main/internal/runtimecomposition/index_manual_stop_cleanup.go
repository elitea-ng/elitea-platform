package runtimecomposition

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	indexingapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexing"
)

type currentIndexManualStopCleanupProcessor struct {
	cleaner           currentIndexManualStopCleaner
	store             currentIndexManualStopCleanupStore
	newClaimID        executionapp.IDGenerator
	claimLease        time.Duration
	concurrency       int
	reportItemFailure func(error)
}

type currentIndexManualStopCleaner interface {
	Cleanup(context.Context, indexingapp.CurrentManualStopCleanupRequest) error
}

type currentIndexManualStopCleanupStore interface {
	ClaimPendingManualStopCleanups(
		context.Context,
		string,
		int,
		time.Duration,
	) ([]indexingapp.CurrentManualStopCleanupClaim, error)
	SupersedeManualStopCleanupIfNewerInitialized(
		context.Context,
		indexingapp.CurrentManualStopCleanupClaim,
	) (bool, error)
	ResolveManualStopCleanup(
		context.Context,
		indexingapp.CurrentManualStopCleanupClaim,
		indexingapp.CurrentManualStopCleanupResolution,
	) error
	ReleaseManualStopCleanup(
		context.Context,
		indexingapp.CurrentManualStopCleanupClaim,
		string,
	) error
}

func (p *currentIndexManualStopCleanupProcessor) ReconcilePendingManualStopCleanups(
	ctx context.Context,
	limit int,
) (int, error) {
	if p == nil || p.cleaner == nil || p.store == nil ||
		p.newClaimID == nil || p.claimLease <= 0 || p.concurrency <= 0 ||
		p.reportItemFailure == nil {
		return 0, indexingapp.ErrCurrentIndexMetaMaterializationUnavailable
	}
	claimToken, err := p.newClaimID()
	if err != nil || claimToken == "" {
		return 0, indexingapp.ErrCurrentIndexMetaMaterializationUnavailable
	}
	claims, err := p.store.ClaimPendingManualStopCleanups(
		ctx,
		claimToken,
		min(limit, 2*p.concurrency),
		p.claimLease,
	)
	if err != nil {
		return 0, err
	}
	if len(claims) == 0 {
		return 0, nil
	}

	workerCount := min(p.concurrency, len(claims))
	jobs := make(chan int)
	type itemResult struct {
		requeued bool
		err      error
	}
	results := make([]itemResult, len(claims))
	var workers sync.WaitGroup
	workers.Add(workerCount)
	for range workerCount {
		go func() {
			defer workers.Done()
			for index := range jobs {
				if ctx.Err() != nil {
					return
				}
				results[index].requeued, results[index].err =
					p.applyClaim(ctx, claims[index])
			}
		}()
	}
send:
	for index := range claims {
		select {
		case jobs <- index:
		case <-ctx.Done():
			break send
		}
	}
	close(jobs)
	workers.Wait()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	itemErrors := make([]error, 0, len(results))
	for index, result := range results {
		if result.err == nil {
			continue
		}
		itemErr := fmt.Errorf(
			"reconcile index manual Stop cleanup %q generation %d: %w",
			claims[index].ExecutionID,
			claims[index].Generation,
			result.err,
		)
		if result.requeued {
			p.reportItemFailure(itemErr)
		} else {
			itemErrors = append(itemErrors, itemErr)
		}
	}
	return len(claims), errors.Join(itemErrors...)
}

func (p *currentIndexManualStopCleanupProcessor) applyClaim(
	ctx context.Context,
	claim indexingapp.CurrentManualStopCleanupClaim,
) (bool, error) {
	superseded, err :=
		p.store.SupersedeManualStopCleanupIfNewerInitialized(ctx, claim)
	if err != nil {
		releaseErr := p.store.ReleaseManualStopCleanup(
			ctx,
			claim,
			terminalEffectErrorCode(err),
		)
		if releaseErr != nil {
			return false, errors.Join(err, releaseErr)
		}
		return true, err
	}
	if superseded {
		return false, nil
	}
	if err := p.cleaner.Cleanup(
		ctx,
		claim.CurrentManualStopCleanupRequest,
	); err != nil {
		if errors.Is(err, indexingapp.ErrCurrentIndexMetaSuperseded) {
			return false, p.store.ResolveManualStopCleanup(
				ctx,
				claim,
				indexingapp.CurrentManualStopCleanupSuperseded,
			)
		}
		releaseErr := p.store.ReleaseManualStopCleanup(
			ctx,
			claim,
			terminalEffectErrorCode(err),
		)
		if releaseErr != nil {
			return false, errors.Join(err, releaseErr)
		}
		return true, err
	}
	return false, p.store.ResolveManualStopCleanup(
		ctx,
		claim,
		indexingapp.CurrentManualStopCleanupApplied,
	)
}
