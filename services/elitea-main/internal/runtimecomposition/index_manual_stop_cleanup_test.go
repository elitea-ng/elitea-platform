package runtimecomposition

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	indexingapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/indexing"
)

type currentManualStopCleanerStub struct {
	mu       sync.Mutex
	requests []indexingapp.CurrentManualStopCleanupRequest
	errors   map[string]error
}

func (s *currentManualStopCleanerStub) Cleanup(
	_ context.Context,
	request indexingapp.CurrentManualStopCleanupRequest,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, request)
	return s.errors[request.ExecutionID]
}

type currentManualStopCleanupStoreStub struct {
	mu           sync.Mutex
	pending      []indexingapp.CurrentManualStopCleanupClaim
	superseded   map[string]bool
	supersedeErr map[string]error
	resolveErr   error
	releaseErr   error
	checked      []indexingapp.CurrentManualStopCleanupClaim
	resolved     []indexingapp.CurrentManualStopCleanupClaim
	resolutions  []indexingapp.CurrentManualStopCleanupResolution
	released     []indexingapp.CurrentManualStopCleanupClaim
	releaseCodes []string
	claimLimits  []int
	claimLeases  []time.Duration
}

func (s *currentManualStopCleanupStoreStub) ClaimPendingManualStopCleanups(
	_ context.Context,
	_ string,
	limit int,
	lease time.Duration,
) ([]indexingapp.CurrentManualStopCleanupClaim, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claimLimits = append(s.claimLimits, limit)
	s.claimLeases = append(s.claimLeases, lease)
	count := min(limit, len(s.pending))
	return append(
		[]indexingapp.CurrentManualStopCleanupClaim(nil),
		s.pending[:count]...,
	), nil
}

func (s *currentManualStopCleanupStoreStub) SupersedeManualStopCleanupIfNewerInitialized(
	_ context.Context,
	claim indexingapp.CurrentManualStopCleanupClaim,
) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checked = append(s.checked, claim)
	return s.superseded[claim.ExecutionID],
		s.supersedeErr[claim.ExecutionID]
}

func (s *currentManualStopCleanupStoreStub) ResolveManualStopCleanup(
	_ context.Context,
	claim indexingapp.CurrentManualStopCleanupClaim,
	resolution indexingapp.CurrentManualStopCleanupResolution,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resolved = append(s.resolved, claim)
	s.resolutions = append(s.resolutions, resolution)
	return s.resolveErr
}

func (s *currentManualStopCleanupStoreStub) ReleaseManualStopCleanup(
	_ context.Context,
	claim indexingapp.CurrentManualStopCleanupClaim,
	errorCode string,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.released = append(s.released, claim)
	s.releaseCodes = append(s.releaseCodes, errorCode)
	return s.releaseErr
}

func TestCurrentManualStopCleanupProcessorAppliesAndFencesClaims(t *testing.T) {
	applied := currentManualStopClaim("applied")
	databaseSuperseded := currentManualStopClaim("database-superseded")
	externalSuperseded := currentManualStopClaim("external-superseded")
	store := &currentManualStopCleanupStoreStub{
		pending: []indexingapp.CurrentManualStopCleanupClaim{
			applied,
			databaseSuperseded,
			externalSuperseded,
		},
		superseded: map[string]bool{
			databaseSuperseded.ExecutionID: true,
		},
	}
	cleaner := &currentManualStopCleanerStub{errors: map[string]error{
		externalSuperseded.ExecutionID: indexingapp.ErrCurrentIndexMetaSuperseded,
	}}
	processor := newTestCurrentManualStopCleanupProcessor(cleaner, store)

	count, err := processor.ReconcilePendingManualStopCleanups(
		context.Background(),
		8,
	)
	if err != nil {
		t.Fatal(err)
	}
	if count != 3 || len(store.checked) != 3 ||
		len(cleaner.requests) != 2 || len(store.resolved) != 2 ||
		len(store.released) != 0 {
		t.Fatalf(
			"count=%d checked=%+v requests=%+v resolved=%+v released=%+v",
			count,
			store.checked,
			cleaner.requests,
			store.resolved,
			store.released,
		)
	}
	got := map[string]indexingapp.CurrentManualStopCleanupResolution{}
	for index := range store.resolved {
		got[store.resolved[index].ExecutionID] = store.resolutions[index]
	}
	if got[applied.ExecutionID] !=
		indexingapp.CurrentManualStopCleanupApplied ||
		got[externalSuperseded.ExecutionID] !=
			indexingapp.CurrentManualStopCleanupSuperseded {
		t.Fatalf("resolutions=%v", got)
	}
}

func TestCurrentManualStopCleanupPoisonTargetDoesNotBlockSibling(t *testing.T) {
	poison := currentManualStopClaim("poison")
	healthy := currentManualStopClaim("healthy")
	dependencyErr := indexingapp.ErrCurrentIndexMetaMaterializationUnavailable
	store := &currentManualStopCleanupStoreStub{
		pending: []indexingapp.CurrentManualStopCleanupClaim{poison, healthy},
	}
	cleaner := &currentManualStopCleanerStub{errors: map[string]error{
		poison.ExecutionID: dependencyErr,
	}}
	processor := newTestCurrentManualStopCleanupProcessor(cleaner, store)
	var reported []error
	processor.reportItemFailure = func(err error) {
		reported = append(reported, err)
	}
	count, err := processor.ReconcilePendingManualStopCleanups(
		context.Background(),
		8,
	)
	if err != nil || count != 2 {
		t.Fatalf("count=%d err=%v", count, err)
	}
	if len(store.released) != 1 ||
		store.released[0].ExecutionID != poison.ExecutionID ||
		len(store.resolved) != 1 ||
		store.resolved[0].ExecutionID != healthy.ExecutionID ||
		len(reported) != 1 ||
		!errors.Is(reported[0], dependencyErr) {
		t.Fatalf(
			"released=%+v resolved=%+v reported=%v",
			store.released,
			store.resolved,
			reported,
		)
	}
}

func TestCurrentManualStopCleanupClaimBatchIsBoundedByConcurrency(
	t *testing.T,
) {
	pending := make([]indexingapp.CurrentManualStopCleanupClaim, 12)
	for index := range pending {
		pending[index] = currentManualStopClaim(
			fmt.Sprintf("execution-%d", index),
		)
	}
	store := &currentManualStopCleanupStoreStub{pending: pending}
	processor := newTestCurrentManualStopCleanupProcessor(
		&currentManualStopCleanerStub{},
		store,
	)
	processor.concurrency = 2
	processor.claimLease = 2 * time.Minute
	count, err := processor.ReconcilePendingManualStopCleanups(
		context.Background(),
		64,
	)
	if err != nil {
		t.Fatal(err)
	}
	if count != 4 || len(store.claimLimits) != 1 ||
		store.claimLimits[0] != 4 ||
		len(store.claimLeases) != 1 ||
		store.claimLeases[0] != 2*time.Minute {
		t.Fatalf(
			"count=%d limits=%v leases=%v",
			count,
			store.claimLimits,
			store.claimLeases,
		)
	}
}

func newTestCurrentManualStopCleanupProcessor(
	cleaner currentIndexManualStopCleaner,
	store currentIndexManualStopCleanupStore,
) *currentIndexManualStopCleanupProcessor {
	return &currentIndexManualStopCleanupProcessor{
		cleaner: cleaner,
		store:   store,
		newClaimID: func() (string, error) {
			return "claim-1", nil
		},
		claimLease:        time.Minute,
		concurrency:       2,
		reportItemFailure: func(error) {},
	}
}

func currentManualStopClaim(
	executionID string,
) indexingapp.CurrentManualStopCleanupClaim {
	return indexingapp.CurrentManualStopCleanupClaim{
		CurrentManualStopCleanupRequest: indexingapp.CurrentManualStopCleanupRequest{
			ExecutionID: executionID,
			Generation:  1,
		},
		ClaimToken: "claim-1",
	}
}
