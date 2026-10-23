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

type currentIndexMetaTerminalizerStub struct {
	mu       sync.Mutex
	requests []indexingapp.CurrentIndexMetaTerminalRequest
	errors   map[string]error
}

func (s *currentIndexMetaTerminalizerStub) Terminalize(
	_ context.Context,
	request indexingapp.CurrentIndexMetaTerminalRequest,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, request)
	return s.errors[request.ExecutionID]
}

type currentIndexMetaTerminalEffectStoreStub struct {
	mu           sync.Mutex
	pending      []indexingapp.CurrentIndexMetaTerminalClaim
	claimErr     error
	supersedeErr error
	resolveErr   error
	releaseErr   error
	superseded   map[string]bool
	checked      []indexingapp.CurrentIndexMetaTerminalClaim
	resolved     []indexingapp.CurrentIndexMetaTerminalClaim
	resolutions  []indexingapp.CurrentIndexMetaTerminalResolution
	released     []indexingapp.CurrentIndexMetaTerminalClaim
	releaseCodes []string
	claimLimits  []int
	claimLeases  []time.Duration
}

func (s *currentIndexMetaTerminalEffectStoreStub) ClaimPendingTerminalEffects(
	_ context.Context,
	_ string,
	limit int,
	lease time.Duration,
) ([]indexingapp.CurrentIndexMetaTerminalClaim, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claimLimits = append(s.claimLimits, limit)
	s.claimLeases = append(s.claimLeases, lease)
	count := min(limit, len(s.pending))
	return append([]indexingapp.CurrentIndexMetaTerminalClaim(nil), s.pending[:count]...), s.claimErr
}

func (s *currentIndexMetaTerminalEffectStoreStub) SupersedeTerminalEffectIfNewerInitialized(
	_ context.Context,
	claim indexingapp.CurrentIndexMetaTerminalClaim,
) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checked = append(s.checked, claim)
	return s.superseded[claim.ExecutionID], s.supersedeErr
}

func (s *currentIndexMetaTerminalEffectStoreStub) ResolveTerminalEffect(
	_ context.Context,
	claim indexingapp.CurrentIndexMetaTerminalClaim,
	resolution indexingapp.CurrentIndexMetaTerminalResolution,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resolved = append(s.resolved, claim)
	s.resolutions = append(s.resolutions, resolution)
	return s.resolveErr
}

func (s *currentIndexMetaTerminalEffectStoreStub) ReleaseTerminalEffect(
	_ context.Context,
	claim indexingapp.CurrentIndexMetaTerminalClaim,
	errorCode string,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.released = append(s.released, claim)
	s.releaseCodes = append(s.releaseCodes, errorCode)
	return s.releaseErr
}

func newTestCurrentIndexMetaEffect(
	terminalizer currentIndexMetaTerminalizer,
	store currentIndexMetaTerminalEffectStore,
) *currentIndexMetaTerminalProcessor {
	return &currentIndexMetaTerminalProcessor{
		terminalizer: terminalizer,
		store:        store,
		newClaimID: func() (string, error) {
			return "claim-1", nil
		},
		claimLease:        time.Minute,
		concurrency:       2,
		reportItemFailure: func(error) {},
	}
}

func TestCurrentIndexMetaTerminalConcurrencyFollowsDedicatedPoolCapacity(t *testing.T) {
	for _, test := range []struct {
		maxConnections int32
		want           int
		wantError      bool
	}{
		{maxConnections: 0, wantError: true},
		{maxConnections: 1, want: 1},
		{maxConnections: 2, want: 2},
		{maxConnections: 8, want: 2},
	} {
		got, err := currentIndexMetaTerminalConcurrency(test.maxConnections)
		if (err != nil) != test.wantError {
			t.Fatalf(
				"max connections %d: error=%v, want error=%v",
				test.maxConnections,
				err,
				test.wantError,
			)
		}
		if got != test.want {
			t.Fatalf(
				"max connections %d: concurrency=%d, want=%d",
				test.maxConnections,
				got,
				test.want,
			)
		}
	}
}

func terminalClaim(
	executionID string,
	state indexingapp.CurrentIndexMetaTerminalState,
) indexingapp.CurrentIndexMetaTerminalClaim {
	request := indexingapp.CurrentIndexMetaTerminalRequest{
		ExecutionID: executionID,
		Generation:  1,
		State:       state,
		OccurredAt:  time.Date(2026, time.July, 26, 12, 13, 14, 0, time.UTC),
	}
	if state == indexingapp.CurrentIndexMetaFailed {
		request.SafeError = "Indexing failed before completion."
	}
	return indexingapp.CurrentIndexMetaTerminalClaim{
		CurrentIndexMetaTerminalRequest: request,
		ClaimToken:                      "claim-1",
	}
}

func TestCurrentIndexMetaReconcilerAppliesClaimedTerminal(t *testing.T) {
	claim := terminalClaim("execution-1", indexingapp.CurrentIndexMetaCancelled)
	terminalizer := &currentIndexMetaTerminalizerStub{}
	store := &currentIndexMetaTerminalEffectStoreStub{
		pending: []indexingapp.CurrentIndexMetaTerminalClaim{claim},
	}
	effect := newTestCurrentIndexMetaEffect(terminalizer, store)

	count, err := effect.ReconcilePendingIndexMetaTerminals(context.Background(), 8)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 || len(terminalizer.requests) != 1 ||
		len(store.resolved) != 1 || len(store.released) != 0 ||
		store.resolutions[0] != indexingapp.CurrentIndexMetaTerminalApplied {
		t.Fatalf(
			"count=%d requests=%+v resolved=%+v released=%+v resolutions=%+v",
			count,
			terminalizer.requests,
			store.resolved,
			store.released,
			store.resolutions,
		)
	}
}

func TestCurrentIndexMetaClaimBatchIsLeaseSafe(t *testing.T) {
	pending := make([]indexingapp.CurrentIndexMetaTerminalClaim, 12)
	for index := range pending {
		pending[index] = terminalClaim(
			fmt.Sprintf("execution-%d", index),
			indexingapp.CurrentIndexMetaCancelled,
		)
	}
	store := &currentIndexMetaTerminalEffectStoreStub{pending: pending}
	effect := newTestCurrentIndexMetaEffect(
		&currentIndexMetaTerminalizerStub{},
		store,
	)
	effect.concurrency = 4
	effect.claimLease = 2 * time.Minute

	count, err := effect.ReconcilePendingIndexMetaTerminals(context.Background(), 64)
	if err != nil {
		t.Fatal(err)
	}
	if count != 8 || len(store.claimLimits) != 1 || store.claimLimits[0] != 8 ||
		len(store.claimLeases) != 1 || store.claimLeases[0] != 2*time.Minute {
		t.Fatalf(
			"count=%d limits=%v leases=%v",
			count,
			store.claimLimits,
			store.claimLeases,
		)
	}
}

func TestCurrentIndexMetaPoisonTenantDoesNotBlockSibling(t *testing.T) {
	poison := terminalClaim("tenant-poison", indexingapp.CurrentIndexMetaFailed)
	healthy := terminalClaim("tenant-healthy", indexingapp.CurrentIndexMetaCancelled)
	dependencyErr := indexingapp.ErrCurrentIndexMetaMaterializationUnavailable
	terminalizer := &currentIndexMetaTerminalizerStub{
		errors: map[string]error{poison.ExecutionID: dependencyErr},
	}
	store := &currentIndexMetaTerminalEffectStoreStub{
		pending: []indexingapp.CurrentIndexMetaTerminalClaim{poison, healthy},
	}
	effect := newTestCurrentIndexMetaEffect(terminalizer, store)
	var reported []error
	effect.reportItemFailure = func(err error) {
		reported = append(reported, err)
	}

	count, err := effect.ReconcilePendingIndexMetaTerminals(context.Background(), 8)
	if count != 2 || err != nil {
		t.Fatalf("count=%d error=%v", count, err)
	}
	if len(terminalizer.requests) != 2 || len(store.released) != 1 ||
		store.released[0].ExecutionID != poison.ExecutionID ||
		len(store.resolved) != 1 ||
		store.resolved[0].ExecutionID != healthy.ExecutionID {
		t.Fatalf(
			"requests=%+v released=%+v resolved=%+v",
			terminalizer.requests,
			store.released,
			store.resolved,
		)
	}
	if len(reported) != 1 || !errors.Is(reported[0], dependencyErr) {
		t.Fatalf("reported=%v", reported)
	}
}

func TestCurrentIndexMetaExplicitLaterGenerationBecomesSuperseded(t *testing.T) {
	claim := terminalClaim("stale-generation", indexingapp.CurrentIndexMetaFailed)
	terminalizer := &currentIndexMetaTerminalizerStub{
		errors: map[string]error{
			claim.ExecutionID: indexingapp.ErrCurrentIndexMetaSuperseded,
		},
	}
	store := &currentIndexMetaTerminalEffectStoreStub{
		pending: []indexingapp.CurrentIndexMetaTerminalClaim{claim},
	}
	effect := newTestCurrentIndexMetaEffect(terminalizer, store)

	if _, err := effect.ReconcilePendingIndexMetaTerminals(
		context.Background(),
		8,
	); err != nil {
		t.Fatal(err)
	}
	if len(store.resolutions) != 1 ||
		store.resolutions[0] != indexingapp.CurrentIndexMetaTerminalSuperseded ||
		len(store.released) != 0 {
		t.Fatalf("resolutions=%+v released=%+v", store.resolutions, store.released)
	}
}

func TestCurrentIndexMetaDurablyNewerInitializedExecutionSkipsExternalWrite(t *testing.T) {
	claim := terminalClaim("stale-equal-generation", indexingapp.CurrentIndexMetaCancelled)
	terminalizer := &currentIndexMetaTerminalizerStub{}
	store := &currentIndexMetaTerminalEffectStoreStub{
		pending:    []indexingapp.CurrentIndexMetaTerminalClaim{claim},
		superseded: map[string]bool{claim.ExecutionID: true},
	}
	effect := newTestCurrentIndexMetaEffect(terminalizer, store)

	count, err := effect.ReconcilePendingIndexMetaTerminals(context.Background(), 8)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 || len(store.checked) != 1 ||
		store.checked[0].ExecutionID != claim.ExecutionID ||
		len(terminalizer.requests) != 0 || len(store.resolved) != 0 ||
		len(store.released) != 0 {
		t.Fatalf(
			"count=%d checked=%+v requests=%+v resolved=%+v released=%+v",
			count,
			store.checked,
			terminalizer.requests,
			store.resolved,
			store.released,
		)
	}
}

func TestCurrentIndexMetaSupersessionLookupFailureRequeuesBeforeExternalWrite(t *testing.T) {
	claim := terminalClaim("supersession-lookup-failure", indexingapp.CurrentIndexMetaCancelled)
	dependencyErr := errors.New("database unavailable")
	terminalizer := &currentIndexMetaTerminalizerStub{}
	store := &currentIndexMetaTerminalEffectStoreStub{
		pending:      []indexingapp.CurrentIndexMetaTerminalClaim{claim},
		supersedeErr: dependencyErr,
	}
	effect := newTestCurrentIndexMetaEffect(terminalizer, store)
	var reported []error
	effect.reportItemFailure = func(err error) {
		reported = append(reported, err)
	}

	count, err := effect.ReconcilePendingIndexMetaTerminals(context.Background(), 8)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 || len(terminalizer.requests) != 0 ||
		len(store.released) != 1 ||
		store.released[0].ExecutionID != claim.ExecutionID ||
		len(store.releaseCodes) != 1 ||
		store.releaseCodes[0] != "DEPENDENCY_UNAVAILABLE" {
		t.Fatalf(
			"count=%d requests=%+v released=%+v release_codes=%+v",
			count,
			terminalizer.requests,
			store.released,
			store.releaseCodes,
		)
	}
	if len(reported) != 1 || !errors.Is(reported[0], dependencyErr) {
		t.Fatalf("reported=%v", reported)
	}
}

func TestCurrentIndexMetaGenericConflictRequeuesAndReports(t *testing.T) {
	claim := terminalClaim("same-generation-conflict", indexingapp.CurrentIndexMetaFailed)
	terminalizer := &currentIndexMetaTerminalizerStub{
		errors: map[string]error{
			claim.ExecutionID: indexingapp.ErrCurrentIndexMetaConflict,
		},
	}
	store := &currentIndexMetaTerminalEffectStoreStub{
		pending: []indexingapp.CurrentIndexMetaTerminalClaim{claim},
	}
	effect := newTestCurrentIndexMetaEffect(terminalizer, store)
	var reported []error
	effect.reportItemFailure = func(err error) {
		reported = append(reported, err)
	}

	count, err := effect.ReconcilePendingIndexMetaTerminals(context.Background(), 8)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 || len(store.resolved) != 0 || len(store.released) != 1 ||
		store.released[0].ExecutionID != claim.ExecutionID ||
		len(store.releaseCodes) != 1 || store.releaseCodes[0] != "CONFLICT" {
		t.Fatalf(
			"count=%d resolved=%+v released=%+v release_codes=%+v",
			count,
			store.resolved,
			store.released,
			store.releaseCodes,
		)
	}
	if len(reported) != 1 ||
		!errors.Is(reported[0], indexingapp.ErrCurrentIndexMetaConflict) {
		t.Fatalf("reported=%v", reported)
	}
}

func TestCurrentIndexMetaCrashAfterExternalWriteRetriesIdempotently(t *testing.T) {
	claim := terminalClaim("crash-window", indexingapp.CurrentIndexMetaCancelled)
	terminalizer := &currentIndexMetaTerminalizerStub{}
	store := &currentIndexMetaTerminalEffectStoreStub{
		pending:    []indexingapp.CurrentIndexMetaTerminalClaim{claim},
		resolveErr: errors.New("marker response lost"),
	}
	effect := newTestCurrentIndexMetaEffect(terminalizer, store)

	if _, err := effect.ReconcilePendingIndexMetaTerminals(
		context.Background(),
		8,
	); err == nil {
		t.Fatal("lost marker response was hidden")
	}
	store.resolveErr = nil
	if _, err := effect.ReconcilePendingIndexMetaTerminals(
		context.Background(),
		8,
	); err != nil {
		t.Fatal(err)
	}
	if len(terminalizer.requests) != 2 || len(store.resolved) != 2 ||
		store.resolutions[1] != indexingapp.CurrentIndexMetaTerminalApplied {
		t.Fatalf(
			"requests=%+v resolved=%+v resolutions=%+v",
			terminalizer.requests,
			store.resolved,
			store.resolutions,
		)
	}
}
