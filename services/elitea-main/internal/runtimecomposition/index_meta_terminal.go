package runtimecomposition

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	indexingapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/indexing"
)

type currentIndexMetaTerminalProcessor struct {
	terminalizer      currentIndexMetaTerminalizer
	store             currentIndexMetaTerminalEffectStore
	newClaimID        executionapp.IDGenerator
	claimLease        time.Duration
	concurrency       int
	reportItemFailure func(error)
}

const maxCurrentIndexMetaTerminalConcurrency = 2

type currentIndexMetaTerminalizer interface {
	Terminalize(context.Context, indexingapp.CurrentIndexMetaTerminalRequest) error
}

type currentIndexMetaTerminalEffectStore interface {
	ClaimPendingTerminalEffects(
		context.Context,
		string,
		int,
		time.Duration,
	) ([]indexingapp.CurrentIndexMetaTerminalClaim, error)
	SupersedeTerminalEffectIfNewerInitialized(
		context.Context,
		indexingapp.CurrentIndexMetaTerminalClaim,
	) (bool, error)
	ResolveTerminalEffect(
		context.Context,
		indexingapp.CurrentIndexMetaTerminalClaim,
		indexingapp.CurrentIndexMetaTerminalResolution,
	) error
	ReleaseTerminalEffect(
		context.Context,
		indexingapp.CurrentIndexMetaTerminalClaim,
		string,
	) error
	RejectTerminalEffect(
		context.Context,
		indexingapp.CurrentIndexMetaTerminalClaimRef,
		string,
		int,
	) (bool, error)
}

// invalidTerminalEffectErrorCode is recorded on a terminal effect that failed
// validation; after MaxCurrentIndexMetaTerminalInvalidAttempts it is
// QUARANTINED with this code.
const invalidTerminalEffectErrorCode = "INVALID_TERMINAL_EFFECT"

func currentIndexMetaTerminalConcurrency(maxConnections int32) (int, error) {
	if maxConnections <= 0 {
		return 0, errors.New("current index metadata terminal pool capacity is invalid")
	}
	return min(int(maxConnections), maxCurrentIndexMetaTerminalConcurrency), nil
}

func (e *currentIndexMetaTerminalProcessor) ReconcilePendingIndexMetaTerminals(
	ctx context.Context,
	limit int,
) (int, error) {
	if e == nil || e.terminalizer == nil || e.store == nil || e.newClaimID == nil ||
		e.claimLease <= 0 || e.concurrency <= 0 || e.reportItemFailure == nil {
		return 0, indexingapp.ErrCurrentIndexMetaMaterializationUnavailable
	}
	claimToken, err := e.newClaimID()
	if err != nil || claimToken == "" {
		return 0, indexingapp.ErrCurrentIndexMetaMaterializationUnavailable
	}
	claimLimit := min(limit, 2*e.concurrency)
	claims, err := e.store.ClaimPendingTerminalEffects(
		ctx,
		claimToken,
		claimLimit,
		e.claimLease,
	)
	var rejected indexingapp.CurrentIndexMetaTerminalRejections
	if err != nil && !errors.As(err, &rejected) {
		return 0, err
	}
	// A rejected row is recorded and skipped; it never costs the valid claims
	// of the same batch their pass.
	rejectErrors := e.rejectClaims(ctx, rejected)
	if len(claims) == 0 {
		return len(rejected), errors.Join(rejectErrors...)
	}

	workerCount := min(e.concurrency, len(claims))
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
					e.applyClaim(ctx, claims[index])
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
	itemErrors := make([]error, 0, len(results)+len(rejectErrors))
	itemErrors = append(itemErrors, rejectErrors...)
	for index, result := range results {
		if result.err == nil {
			continue
		}
		itemErr := fmt.Errorf(
			"reconcile index metadata terminal %q generation %d: %w",
			claims[index].ExecutionID,
			claims[index].Generation,
			result.err,
		)
		if result.requeued {
			e.reportItemFailure(itemErr)
		} else {
			itemErrors = append(itemErrors, fmt.Errorf(
				"reconcile index metadata terminal %q generation %d: %w",
				claims[index].ExecutionID,
				claims[index].Generation,
				result.err,
			))
		}
	}
	return len(claims) + len(rejected), errors.Join(itemErrors...)
}

// rejectClaims backs off or quarantines each claimed effect that failed
// validation and reports it with its execution and the failed check. It
// returns only the failures to record a rejection, which fail the pass.
func (e *currentIndexMetaTerminalProcessor) rejectClaims(
	ctx context.Context,
	rejected indexingapp.CurrentIndexMetaTerminalRejections,
) []error {
	var failures []error
	for _, rejection := range rejected {
		quarantined, err := e.store.RejectTerminalEffect(
			ctx,
			rejection.CurrentIndexMetaTerminalClaimRef,
			invalidTerminalEffectErrorCode,
			indexingapp.MaxCurrentIndexMetaTerminalInvalidAttempts,
		)
		if err != nil {
			failures = append(failures, fmt.Errorf(
				"record rejection of %w: %w", rejection, err,
			))
			continue
		}
		outcome := "backed off"
		if quarantined {
			outcome = "quarantined"
		}
		e.reportItemFailure(fmt.Errorf("%w (%s as %s)", rejection, outcome, invalidTerminalEffectErrorCode))
	}
	return failures
}

func (e *currentIndexMetaTerminalProcessor) applyClaim(
	ctx context.Context,
	claim indexingapp.CurrentIndexMetaTerminalClaim,
) (bool, error) {
	superseded, err := e.store.SupersedeTerminalEffectIfNewerInitialized(ctx, claim)
	if err != nil {
		releaseErr := e.store.ReleaseTerminalEffect(ctx, claim, terminalEffectErrorCode(err))
		if releaseErr != nil {
			return false, errors.Join(err, releaseErr)
		}
		return true, err
	}
	if superseded {
		return false, nil
	}
	if err := e.terminalizer.Terminalize(ctx, claim.CurrentIndexMetaTerminalRequest); err != nil {
		if errors.Is(err, indexingapp.ErrCurrentIndexMetaSuperseded) {
			return false, e.store.ResolveTerminalEffect(
				ctx,
				claim,
				indexingapp.CurrentIndexMetaTerminalSuperseded,
			)
		}
		if ctx.Err() == nil && errors.Is(err, indexingapp.ErrCurrentIndexMetaInitializationInvalid) {
			// Invalid evidence does not heal on retry: record it, back off,
			// and quarantine at the attempt limit, as a claim-time rejection.
			quarantined, rejectErr := e.store.RejectTerminalEffect(
				ctx,
				indexingapp.CurrentIndexMetaTerminalClaimRef{
					ExecutionID: claim.ExecutionID,
					Generation:  claim.Generation,
					ClaimToken:  claim.ClaimToken,
				},
				invalidTerminalEffectErrorCode,
				indexingapp.MaxCurrentIndexMetaTerminalInvalidAttempts,
			)
			if rejectErr != nil {
				return false, errors.Join(err, rejectErr)
			}
			if quarantined {
				return true, fmt.Errorf("%w (quarantined as %s)", err, invalidTerminalEffectErrorCode)
			}
			return true, fmt.Errorf("%w (backed off as %s)", err, invalidTerminalEffectErrorCode)
		}
		releaseErr := e.store.ReleaseTerminalEffect(ctx, claim, terminalEffectErrorCode(err))
		if releaseErr != nil {
			return false, errors.Join(err, releaseErr)
		}
		return true, err
	}
	return false, e.store.ResolveTerminalEffect(
		ctx,
		claim,
		indexingapp.CurrentIndexMetaTerminalApplied,
	)
}

func terminalEffectErrorCode(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "CONTEXT_CANCELLED"
	case errors.Is(err, context.DeadlineExceeded):
		return "CONTEXT_DEADLINE"
	case errors.Is(err, indexingapp.ErrCurrentIndexMetaConflict):
		return "CONFLICT"
	case errors.Is(err, indexingapp.ErrCurrentIndexMetaTargetUnavailable):
		return "TARGET_UNAVAILABLE"
	default:
		return "DEPENDENCY_UNAVAILABLE"
	}
}
