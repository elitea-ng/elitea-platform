package indexing

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
)

type indexMetaMaterializerStub struct {
	mu      sync.Mutex
	request AdmissionOutcome
	inputs  AuthoritativeInputs
	err     error
	calls   atomic.Int32
	active  atomic.Int32
	maximum atomic.Int32
	block   <-chan struct{}
}

func (s *indexMetaMaterializerStub) MaterializeInitialIndexMeta(
	ctx context.Context,
	request SubmitRequest,
	outcome AdmissionOutcome,
) error {
	s.calls.Add(1)
	active := s.active.Add(1)
	defer s.active.Add(-1)
	for {
		maximum := s.maximum.Load()
		if active <= maximum || s.maximum.CompareAndSwap(maximum, active) {
			break
		}
	}
	s.mu.Lock()
	s.request = cloneAdmissionOutcome(outcome)
	s.inputs = request.Inputs.Clone()
	s.mu.Unlock()
	if s.block != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.block:
		}
	}
	return s.err
}

type indexMetaInitializationStoreStub struct {
	mu             sync.Mutex
	work           IndexMetaInitializationWork
	pending        []IndexMetaInitializationClaim
	staleTerminal  int
	staleErr       error
	exactErr       error
	loadErr        error
	resolveErr     error
	initializedAt  time.Time
	resolved       int
	released       int
	quarantined    int
	lastErrorCode  string
	claimedExactID IndexMetaInitialization
}

func (s *indexMetaInitializationStoreStub) QuarantineExpiredTerminalIndexMetaInitializations(
	_ context.Context,
	limit int,
) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.staleErr != nil {
		return 0, s.staleErr
	}
	count := min(limit, s.staleTerminal)
	s.staleTerminal -= count
	return count, nil
}

func (s *indexMetaInitializationStoreStub) ClaimExactIndexMetaInitialization(
	_ context.Context,
	identity IndexMetaInitialization,
	token string,
	_ time.Duration,
) (IndexMetaInitializationClaim, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claimedExactID = identity
	if s.exactErr != nil {
		return IndexMetaInitializationClaim{}, s.exactErr
	}
	claim := s.work.Claim
	claim.ClaimToken = token
	s.work.Claim = claim
	return claim, nil
}

func (s *indexMetaInitializationStoreStub) ClaimPendingIndexMetaInitializations(
	_ context.Context,
	token string,
	limit int,
	_ time.Duration,
) ([]IndexMetaInitializationClaim, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit > len(s.pending) {
		limit = len(s.pending)
	}
	claims := append([]IndexMetaInitializationClaim(nil), s.pending[:limit]...)
	for index := range claims {
		claims[index].ClaimToken = token
	}
	return claims, nil
}

func (s *indexMetaInitializationStoreStub) LoadIndexMetaInitializationWork(
	_ context.Context,
	claim IndexMetaInitializationClaim,
) (IndexMetaInitializationWork, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return IndexMetaInitializationWork{}, s.loadErr
	}
	work := s.work
	work.Claim = claim
	work.Outcome.ExecutionID = claim.ExecutionID
	work.Outcome.Generation = claim.Generation
	return work, nil
}

func (s *indexMetaInitializationStoreStub) ResolveIndexMetaInitialization(
	_ context.Context,
	_ IndexMetaInitializationClaim,
) (time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resolved++
	return s.initializedAt, s.resolveErr
}

func (s *indexMetaInitializationStoreStub) ReleaseIndexMetaInitialization(
	_ context.Context,
	_ IndexMetaInitializationClaim,
	code string,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.released++
	s.lastErrorCode = code
	return nil
}

func (s *indexMetaInitializationStoreStub) QuarantineIndexMetaInitialization(
	_ context.Context,
	_ IndexMetaInitializationClaim,
	code string,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.quarantined++
	s.lastErrorCode = code
	return nil
}

func newTestDurableIndexMetaInitializer(
	t *testing.T,
	store *indexMetaInitializationStoreStub,
	materializer IndexMetaMaterializer,
	maxConcurrent int,
) *DurableIndexMetaInitializer {
	t.Helper()
	initializer, err := NewDurableIndexMetaInitializer(
		store,
		materializer,
		func() (string, error) { return "claim-token", nil },
		IndexMetaInitializationReconcilerConfig{
			PollInterval:  time.Millisecond,
			ClaimLease:    time.Minute,
			BatchSize:     8,
			MaxConcurrent: maxConcurrent,
			ReportFailure: func(error) {},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	initializer.now = func() time.Time {
		return time.Date(2025, time.July, 23, 9, 0, 0, 0, time.UTC)
	}
	return initializer
}

func validInitializationWork() IndexMetaInitializationWork {
	outcome := validStartAdmissionOutcome()
	outcome.IndexMetaInitializedAt = nil
	return IndexMetaInitializationWork{
		Claim: IndexMetaInitializationClaim{
			ExecutionID: outcome.ExecutionID,
			Generation:  outcome.Generation,
			ClaimToken:  "claim-token",
			Attempt:     1,
			ExpiresAt:   time.Now().UTC().Add(time.Minute),
		},
		Request: SubmitRequest{
			Identity:      executionIdentityForTest(),
			CorrelationID: outcome.IndexMetaCorrelationID,
			ToolkitID:     9,
			Initiator:     "user",
			Inputs:        validStartServiceInputs(),
		},
		Outcome: outcome,
	}
}

func executionIdentityForTest() executionapp.AdmissionIdentity {
	return executionapp.AdmissionIdentity{
		TenantID: "7", ResourceProjectID: "7",
		ProjectionProjectID: "7", ActorID: "11",
	}
}

func TestInitializingAdmissionSubmitterReloadsFrozenIntentBeforeOpeningGate(
	t *testing.T,
) {
	work := validInitializationWork()
	initializedAt := time.Date(2026, time.July, 23, 9, 30, 0, 0, time.UTC)
	store := &indexMetaInitializationStoreStub{
		work:          work,
		initializedAt: initializedAt,
	}
	materializer := &indexMetaMaterializerStub{}
	initializer := newTestDurableIndexMetaInitializer(
		t,
		store,
		materializer,
		1,
	)
	submitter, err := NewInitializingAdmissionSubmitter(
		&startAdmissionStub{outcome: work.Outcome},
		initializer,
	)
	if err != nil {
		t.Fatal(err)
	}
	request := work.Request
	request.Inputs.ToolParameters = []byte(`{"index_name":"caller-drift"}`)

	got, err := submitter.Submit(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if materializer.calls.Load() != 1 || store.resolved != 1 ||
		string(materializer.inputs.ToolParameters) !=
			string(work.Request.Inputs.ToolParameters) ||
		got.IndexMetaInitializedAt == nil ||
		!got.IndexMetaInitializedAt.Equal(initializedAt) {
		t.Fatalf(
			"outcome=%+v calls=%d resolved=%d inputs=%s",
			got,
			materializer.calls.Load(),
			store.resolved,
			materializer.inputs.ToolParameters,
		)
	}
}

func TestDurableIndexMetaInitializerReleasesRetryableAndQuarantinesPermanent(
	t *testing.T,
) {
	for _, test := range []struct {
		name            string
		failure         error
		wantReleased    int
		wantQuarantined int
		wantCode        string
	}{
		{
			name:         "dependency",
			failure:      ErrCurrentIndexMetaMaterializationUnavailable,
			wantReleased: 1,
			wantCode:     "INITIALIZATION_DEPENDENCY_UNAVAILABLE",
		},
		{
			name:            "frozen target invalid",
			failure:         ErrCurrentIndexMetaTargetUnavailable,
			wantQuarantined: 1,
			wantCode:        "INITIALIZATION_TARGET_INVALID",
		},
		{
			name:            "external generation superseded",
			failure:         ErrCurrentIndexMetaSuperseded,
			wantQuarantined: 1,
			wantCode:        "INITIALIZATION_EXTERNAL_SUPERSEDED",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &indexMetaInitializationStoreStub{
				work:          validInitializationWork(),
				initializedAt: time.Now(),
			}
			initializer := newTestDurableIndexMetaInitializer(
				t,
				store,
				&indexMetaMaterializerStub{err: test.failure},
				1,
			)
			_, err := initializer.Initialize(
				context.Background(),
				store.work.Outcome,
			)
			if !errors.Is(err, test.failure) ||
				store.released != test.wantReleased ||
				store.quarantined != test.wantQuarantined ||
				store.lastErrorCode != test.wantCode ||
				store.resolved != 0 {
				t.Fatalf(
					"error=%v released=%d quarantined=%d code=%q resolved=%d",
					err,
					store.released,
					store.quarantined,
					store.lastErrorCode,
					store.resolved,
				)
			}
		})
	}
}

func TestDurableIndexMetaInitializerRetriesUnknownPostCommitMarkerOutcome(
	t *testing.T,
) {
	unknown := errors.New("database result unknown")
	store := &indexMetaInitializationStoreStub{
		work:          validInitializationWork(),
		initializedAt: time.Now().UTC(),
		resolveErr:    unknown,
	}
	materializer := &indexMetaMaterializerStub{}
	initializer := newTestDurableIndexMetaInitializer(
		t,
		store,
		materializer,
		1,
	)
	if _, err := initializer.Initialize(
		context.Background(),
		store.work.Outcome,
	); !errors.Is(err, unknown) {
		t.Fatalf("first error=%v", err)
	}
	store.resolveErr = nil
	if _, err := initializer.Initialize(
		context.Background(),
		store.work.Outcome,
	); err != nil {
		t.Fatal(err)
	}
	if materializer.calls.Load() != 2 || store.resolved != 2 {
		t.Fatalf(
			"idempotent external calls=%d marker attempts=%d",
			materializer.calls.Load(),
			store.resolved,
		)
	}
}

func TestDurableIndexMetaInitializerReleasesCancelledInFlightMaterialization(
	t *testing.T,
) {
	store := &indexMetaInitializationStoreStub{
		work:          validInitializationWork(),
		initializedAt: time.Now().UTC(),
	}
	materializer := &indexMetaMaterializerStub{
		block: make(chan struct{}),
	}
	initializer := newTestDurableIndexMetaInitializer(
		t,
		store,
		materializer,
		1,
	)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := initializer.Initialize(ctx, store.work.Outcome)
		done <- err
	}()
	deadline := time.After(time.Second)
	for materializer.calls.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("materializer did not enter the in-flight call")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	err := <-done
	if !errors.Is(err, context.Canceled) ||
		store.released != 1 ||
		store.quarantined != 0 ||
		store.lastErrorCode != "INITIALIZATION_CANCELLED" ||
		store.resolved != 0 {
		t.Fatalf(
			"error=%v released=%d quarantined=%d code=%q resolved=%d",
			err,
			store.released,
			store.quarantined,
			store.lastErrorCode,
			store.resolved,
		)
	}
}

func TestDurableIndexMetaInitializerQuarantinesExpiredAbsoluteDeadline(
	t *testing.T,
) {
	work := validInitializationWork()
	work.Outcome.AdmittedAt = time.Date(
		2025,
		time.July,
		23,
		8,
		0,
		0,
		0,
		time.UTC,
	)
	work.Outcome.Deadline = time.Date(
		2025,
		time.July,
		23,
		8,
		59,
		59,
		0,
		time.UTC,
	)
	store := &indexMetaInitializationStoreStub{
		work:          work,
		initializedAt: time.Now().UTC(),
	}
	materializer := &indexMetaMaterializerStub{}
	initializer := newTestDurableIndexMetaInitializer(
		t,
		store,
		materializer,
		1,
	)
	_, err := initializer.Initialize(context.Background(), work.Outcome)
	if !errors.Is(err, context.DeadlineExceeded) ||
		materializer.calls.Load() != 0 ||
		store.released != 0 ||
		store.quarantined != 1 ||
		store.lastErrorCode != "INITIALIZATION_DEADLINE_EXCEEDED" ||
		store.resolved != 0 {
		t.Fatalf(
			"error=%v calls=%d released=%d quarantined=%d code=%q resolved=%d",
			err,
			materializer.calls.Load(),
			store.released,
			store.quarantined,
			store.lastErrorCode,
			store.resolved,
		)
	}
}

func TestDurableIndexMetaInitializerBoundsRecoveryConcurrency(t *testing.T) {
	work := validInitializationWork()
	claims := make([]IndexMetaInitializationClaim, 6)
	for index := range claims {
		claims[index] = work.Claim
		claims[index].ExecutionID += string(rune('a' + index))
	}
	release := make(chan struct{})
	store := &indexMetaInitializationStoreStub{
		work:          work,
		pending:       claims,
		initializedAt: time.Now().UTC(),
	}
	materializer := &indexMetaMaterializerStub{block: release}
	initializer := newTestDurableIndexMetaInitializer(
		t,
		store,
		materializer,
		2,
	)
	done := make(chan error, 1)
	go func() {
		_, err := initializer.Reconcile(context.Background())
		done <- err
	}()
	deadline := time.After(time.Second)
	for materializer.calls.Load() < 2 {
		select {
		case <-deadline:
			t.Fatal("bounded workers did not start")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if got := materializer.maximum.Load(); got != 2 {
		t.Fatalf("maximum concurrency=%d want=2", got)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := materializer.maximum.Load(); got > 2 {
		t.Fatalf("maximum concurrency=%d exceeded bound", got)
	}
}

func TestDurableIndexMetaInitializerQuarantinesTerminalDeadlineBeforeRecovery(
	t *testing.T,
) {
	work := validInitializationWork()
	store := &indexMetaInitializationStoreStub{
		work:          work,
		staleTerminal: 2,
		pending:       []IndexMetaInitializationClaim{work.Claim},
		initializedAt: time.Now().UTC(),
	}
	materializer := &indexMetaMaterializerStub{}
	initializer := newTestDurableIndexMetaInitializer(
		t,
		store,
		materializer,
		1,
	)
	initializer.config.BatchSize = 2

	count, err := initializer.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 || store.staleTerminal != 0 ||
		materializer.calls.Load() != 0 {
		t.Fatalf(
			"count=%d stale=%d materializations=%d",
			count,
			store.staleTerminal,
			materializer.calls.Load(),
		)
	}
}

func TestDurableIndexMetaInitializerStopsMaterializationBeforeLeaseReentry(
	t *testing.T,
) {
	const claimLease = 400 * time.Millisecond
	firstStartedAt := time.Now().UTC()
	firstWork := validInitializationWork()
	firstWork.Outcome.AdmittedAt = firstStartedAt.Add(-time.Minute)
	firstWork.Outcome.Deadline = firstStartedAt.Add(time.Minute)
	firstWork.Claim.ExpiresAt = firstStartedAt.Add(claimLease)
	firstStore := &indexMetaInitializationStoreStub{
		work:          firstWork,
		initializedAt: firstStartedAt,
	}
	release := make(chan struct{})
	materializer := &indexMetaMaterializerStub{block: release}
	newInitializer := func(
		store *indexMetaInitializationStoreStub,
		claimToken string,
	) *DurableIndexMetaInitializer {
		initializer, err := NewDurableIndexMetaInitializer(
			store,
			materializer,
			func() (string, error) { return claimToken, nil },
			IndexMetaInitializationReconcilerConfig{
				PollInterval:  time.Millisecond,
				ClaimLease:    claimLease,
				BatchSize:     1,
				MaxConcurrent: 1,
				ReportFailure: func(error) {},
			},
		)
		if err != nil {
			t.Fatal(err)
		}
		return initializer
	}

	firstInitializer := newInitializer(firstStore, "first-claim")
	firstDone := make(chan error, 1)
	go func() {
		_, err := firstInitializer.Initialize(
			context.Background(),
			firstWork.Outcome,
		)
		firstDone <- err
	}()
	waitForMaterializerCalls(t, materializer, 1)

	select {
	case err := <-firstDone:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("first materialization error=%v", err)
		}
	case <-time.After(claimLease):
		close(release)
		t.Fatal("first materialization outlived its claim lease")
	}
	if firstStore.released != 1 ||
		firstStore.lastErrorCode != "INITIALIZATION_ATTEMPT_DEADLINE" ||
		firstStore.resolved != 0 {
		t.Fatalf(
			"first released=%d code=%q resolved=%d",
			firstStore.released,
			firstStore.lastErrorCode,
			firstStore.resolved,
		)
	}

	if remaining := time.Until(firstWork.Claim.ExpiresAt); remaining > 0 {
		time.Sleep(remaining)
	}
	secondStartedAt := time.Now().UTC()
	secondWork := validInitializationWork()
	secondWork.Outcome.AdmittedAt = secondStartedAt.Add(-time.Minute)
	secondWork.Outcome.Deadline = secondStartedAt.Add(time.Minute)
	secondWork.Claim.ExpiresAt = secondStartedAt.Add(claimLease)
	secondStore := &indexMetaInitializationStoreStub{
		work:          secondWork,
		initializedAt: secondStartedAt,
	}
	secondInitializer := newInitializer(secondStore, "second-claim")
	secondDone := make(chan error, 1)
	go func() {
		_, err := secondInitializer.Initialize(
			context.Background(),
			secondWork.Outcome,
		)
		secondDone <- err
	}()
	waitForMaterializerCalls(t, materializer, 2)
	close(release)
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
	if materializer.maximum.Load() != 1 || secondStore.resolved != 1 {
		t.Fatalf(
			"maximum concurrency=%d second resolved=%d",
			materializer.maximum.Load(),
			secondStore.resolved,
		)
	}
}

func TestDurableIndexMetaInitializerUsesEarliestMaterializationDeadline(
	t *testing.T,
) {
	now := time.Date(2026, time.July, 27, 12, 0, 0, 0, time.UTC)
	initializer := &DurableIndexMetaInitializer{
		config: IndexMetaInitializationReconcilerConfig{
			ClaimLease: 40 * time.Second,
		},
		now: func() time.Time { return now },
	}
	claim := IndexMetaInitializationClaim{
		ExpiresAt: now.Add(40 * time.Second),
	}
	for _, test := range []struct {
		name        string
		jobDeadline time.Time
		want        time.Time
		wantOK      bool
	}{
		{
			name:        "job deadline",
			jobDeadline: now.Add(20 * time.Second),
			want:        now.Add(20 * time.Second),
			wantOK:      true,
		},
		{
			name:        "claim lease reserve",
			jobDeadline: now.Add(time.Minute),
			want:        now.Add(35 * time.Second),
			wantOK:      true,
		},
		{
			name:        "expired claim reserve",
			jobDeadline: now.Add(time.Minute),
			want:        now,
			wantOK:      false,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := claim
			if !test.wantOK {
				candidate.ExpiresAt = now.Add(5 * time.Second)
			}
			got, ok := initializer.materializationDeadline(
				candidate,
				test.jobDeadline,
			)
			if ok != test.wantOK || !got.Equal(test.want) {
				t.Fatalf(
					"deadline=%v ok=%t want=%v wantOK=%t",
					got,
					ok,
					test.want,
					test.wantOK,
				)
			}
		})
	}
}

func waitForMaterializerCalls(
	t *testing.T,
	materializer *indexMetaMaterializerStub,
	want int32,
) {
	t.Helper()
	timeout := time.After(time.Second)
	for materializer.calls.Load() < want {
		select {
		case <-timeout:
			t.Fatalf(
				"materializer calls=%d want=%d",
				materializer.calls.Load(),
				want,
			)
		default:
			time.Sleep(time.Millisecond)
		}
	}
}
