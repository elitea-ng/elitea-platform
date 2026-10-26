package scheduling

import (
	"context"
	"crypto/sha256"
	"errors"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type intervalSchedule time.Duration

func (s intervalSchedule) Next(after time.Time) time.Time {
	interval := time.Duration(s)
	return after.Truncate(interval).Add(interval)
}

type memoryCursor struct {
	revision string
	through  time.Time
	owner    string
	epoch    int64
	fence    [32]byte
	expires  time.Time
}

type memoryOccurrence struct {
	seed        OccurrenceSeed
	state       string
	owner       string
	epoch       int64
	fence       [32]byte
	expires     time.Time
	nextAttempt time.Time
	attempts    int
	outcome     Outcome
	errorCode   string
}

type memoryStore struct {
	mu          sync.Mutex
	now         time.Time
	cursors     map[string]*memoryCursor
	occurrences map[string]*memoryOccurrence
}

func newMemoryStore(now time.Time) *memoryStore {
	return &memoryStore{
		now: now, cursors: make(map[string]*memoryCursor),
		occurrences: make(map[string]*memoryOccurrence),
	}
}

func (s *memoryStore) Now(context.Context) (time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.now, nil
}

func (s *memoryStore) ClaimPlanning(
	_ context.Context,
	job RegisteredJob,
	owner string,
	now time.Time,
	lease, lookback time.Duration,
) (PlanningClaim, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cursor := s.cursors[job.ID]
	if cursor == nil {
		cursor = &memoryCursor{revision: job.Revision, through: now.Add(-lookback)}
		s.cursors[job.ID] = cursor
	}
	if cursor.expires.After(now) {
		return PlanningClaim{}, false, nil
	}
	if cursor.revision != job.Revision {
		for _, occurrence := range s.occurrences {
			if occurrence.seed.JobID == job.ID && occurrence.state == "PENDING" {
				occurrence.state = "SUPERSEDED"
			}
		}
		cursor.revision = job.Revision
		cursor.through = now.Add(-lookback)
	}
	cursor.owner = owner
	cursor.epoch++
	cursor.fence = testFence(job.ID, cursor.epoch)
	cursor.expires = now.Add(lease)
	return PlanningClaim{
		JobID: job.ID, ScheduleRevision: job.Revision,
		ObservedThrough: cursor.through, LeaseEpoch: cursor.epoch,
		ClaimFence: cursor.fence,
	}, true, nil
}

func (s *memoryStore) MaterializeAndAdvance(
	_ context.Context,
	claim PlanningClaim,
	seeds []OccurrenceSeed,
	through time.Time,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cursor := s.cursors[claim.JobID]
	if cursor == nil || cursor.revision != claim.ScheduleRevision ||
		cursor.epoch != claim.LeaseEpoch || cursor.fence != claim.ClaimFence {
		return ErrStaleFence
	}
	for _, seed := range seeds {
		if _, exists := s.occurrences[seed.InvocationID]; !exists {
			s.occurrences[seed.InvocationID] = &memoryOccurrence{seed: seed, state: "PENDING"}
		}
	}
	cursor.through = through
	cursor.owner = ""
	cursor.fence = [32]byte{}
	cursor.expires = time.Time{}
	return nil
}

func (s *memoryStore) DisableAndAdvance(_ context.Context, claim PlanningClaim, through time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cursor := s.cursors[claim.JobID]
	if cursor == nil || cursor.revision != claim.ScheduleRevision ||
		cursor.epoch != claim.LeaseEpoch || cursor.fence != claim.ClaimFence {
		return ErrStaleFence
	}
	for _, occurrence := range s.occurrences {
		if occurrence.seed.JobID == claim.JobID && occurrence.state == "PENDING" &&
			!occurrence.expires.After(through) {
			occurrence.state = "SUPERSEDED"
		}
	}
	cursor.through = through
	cursor.owner = ""
	cursor.fence = [32]byte{}
	cursor.expires = time.Time{}
	return nil
}

func (s *memoryStore) ClaimPage(
	_ context.Context,
	registered []RegisteredJob,
	owner string,
	now time.Time,
	lease time.Duration,
	limit int,
) ([]ClaimedOccurrence, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = now
	allowed := make(map[string]string, len(registered))
	for _, job := range registered {
		allowed[job.ID] = job.Revision
	}
	pending := make([]*memoryOccurrence, 0)
	for _, occurrence := range s.occurrences {
		if occurrence.state == "PENDING" &&
			!occurrence.expires.After(now) &&
			!occurrence.nextAttempt.After(now) &&
			allowed[occurrence.seed.JobID] == occurrence.seed.ScheduleRevision {
			pending = append(pending, occurrence)
		}
	}
	sort.Slice(pending, func(i, j int) bool {
		if pending[i].seed.DueAt.Equal(pending[j].seed.DueAt) {
			return pending[i].seed.InvocationID < pending[j].seed.InvocationID
		}
		return pending[i].seed.DueAt.Before(pending[j].seed.DueAt)
	})
	if len(pending) > limit {
		pending = pending[:limit]
	}
	claimed := make([]ClaimedOccurrence, 0, len(pending))
	for _, occurrence := range pending {
		occurrence.owner = owner
		occurrence.epoch++
		occurrence.fence = testFence(occurrence.seed.InvocationID, occurrence.epoch)
		occurrence.expires = now.Add(lease)
		occurrence.attempts++
		claimed = append(claimed, ClaimedOccurrence{
			Occurrence: Occurrence{
				InvocationID: occurrence.seed.InvocationID, JobID: occurrence.seed.JobID,
				ScheduleRevision: occurrence.seed.ScheduleRevision,
				DueAt:            occurrence.seed.DueAt, LeaseEpoch: occurrence.epoch,
				ClaimFence: "test-fence",
			},
			Mode: occurrence.seed.Mode, ClaimBytes: occurrence.fence,
		})
	}
	return claimed, nil
}

func (s *memoryStore) Complete(ctx context.Context, claim ClaimedOccurrence, outcome Outcome) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	occurrence := s.occurrences[claim.InvocationID]
	if occurrence == nil || occurrence.state != "PENDING" ||
		occurrence.epoch != claim.LeaseEpoch || occurrence.fence != claim.ClaimBytes {
		return ErrStaleFence
	}
	occurrence.state = "COMPLETED"
	occurrence.outcome = outcome
	occurrence.owner = ""
	occurrence.expires = time.Time{}
	occurrence.fence = [32]byte{}
	return nil
}

func (s *memoryStore) ReleaseForRetry(
	ctx context.Context,
	claim ClaimedOccurrence,
	code string,
	retryDelay time.Duration,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	occurrence := s.occurrences[claim.InvocationID]
	if occurrence == nil || occurrence.state != "PENDING" ||
		occurrence.epoch != claim.LeaseEpoch || occurrence.fence != claim.ClaimBytes {
		return ErrStaleFence
	}
	occurrence.owner = ""
	occurrence.expires = time.Time{}
	occurrence.fence = [32]byte{}
	occurrence.errorCode = code
	occurrence.nextAttempt = s.now.Add(retryDelay)
	return nil
}

func testFence(key string, epoch int64) [32]byte {
	return sha256.Sum256([]byte(key + time.Unix(epoch, 0).String()))
}

func TestRunnerReplicasRetryAmbiguousDurableAdmissionOnce(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	store := newMemoryStore(now)
	var calls atomic.Int32
	var durable sync.Map
	handler := HandlerFunc(func(_ context.Context, occurrence Occurrence) (Outcome, error) {
		_, _ = durable.LoadOrStore(occurrence.InvocationID, struct{}{})
		if calls.Add(1) == 1 {
			return "", errors.New("commit response lost")
		}
		return OutcomeDurablyAdmitted, nil
	})
	job := Job{
		ID: "index.schedule.scan.v1", Revision: "index-scan-r1",
		Mode: ModeDurableAdmission, Schedule: intervalSchedule(time.Minute),
		Timeout: time.Second, Handler: handler,
	}
	config := testConfig("main-a")
	config.PageSize = 1
	config.MaxPagesPerTick = 4
	registry, err := NewRegistry(config.LeaseDuration, job)
	if err != nil {
		t.Fatal(err)
	}
	first, err := NewRunner(store, registry, config, nil)
	if err != nil {
		t.Fatal(err)
	}
	config.InstanceID = "main-b"
	second, err := NewRunner(store, registry, config, nil)
	if err != nil {
		t.Fatal(err)
	}

	firstResult, err := first.Tick(context.Background(), now)
	if err != nil || firstResult.Released != 1 || firstResult.Completed != 0 {
		t.Fatalf("first tick result=%+v error=%v", firstResult, err)
	}
	blockedResult, err := second.Tick(context.Background(), now)
	if err != nil || blockedResult.Claimed != 0 {
		t.Fatalf("retry-delay tick result=%+v error=%v", blockedResult, err)
	}
	if calls.Load() != 1 {
		t.Fatalf("handler calls before retry delay=%d want 1", calls.Load())
	}
	secondResult, err := second.Tick(context.Background(), now.Add(config.RetryDelay))
	if err != nil || secondResult.Completed != 1 {
		t.Fatalf("post-delay takeover tick result=%+v error=%v", secondResult, err)
	}
	if calls.Load() != 2 {
		t.Fatalf("handler calls=%d want retry", calls.Load())
	}
	durableCount := 0
	durable.Range(func(_, _ any) bool { durableCount++; return true })
	if durableCount != 1 {
		t.Fatalf("logical durable admissions=%d want 1", durableCount)
	}
}

func TestRunnerSettlesSuccessAfterParentCancellation(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 30, 0, 0, time.UTC)
	store := newMemoryStore(now)
	ctx, cancel := context.WithCancel(context.Background())
	job := Job{
		ID: "cancel.settlement.v1", Revision: "r1",
		Mode: ModeLocalBounded, Schedule: intervalSchedule(time.Minute),
		Timeout: time.Second,
		Handler: HandlerFunc(func(context.Context, Occurrence) (Outcome, error) {
			cancel()
			return OutcomeLocalCompleted, nil
		}),
	}
	config := testConfig("main-a")
	registry, err := NewRegistry(config.LeaseDuration, job)
	if err != nil {
		t.Fatal(err)
	}
	runner, err := NewRunner(store, registry, config, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Tick(ctx, now)
	if err != nil {
		t.Fatalf("detached settlement tick: %v", err)
	}
	if result.Completed != 1 {
		t.Fatalf("detached settlement result=%+v", result)
	}
}

func TestOccurrenceLeaseTakeoverRejectsStaleFence(t *testing.T) {
	now := time.Date(2026, 7, 30, 13, 0, 0, 0, time.UTC)
	store := newMemoryStore(now)
	seed := OccurrenceSeed{
		InvocationID: invocationID("cleanup.v1", "r1", now),
		JobID:        "cleanup.v1", ScheduleRevision: "r1", DueAt: now,
		Mode: ModeLocalBounded,
	}
	store.occurrences[seed.InvocationID] = &memoryOccurrence{seed: seed, state: "PENDING"}
	registered := []RegisteredJob{{ID: seed.JobID, Revision: seed.ScheduleRevision}}
	oldClaim, err := store.ClaimPage(context.Background(), registered, "main-a", now, time.Minute, 1)
	if err != nil || len(oldClaim) != 1 {
		t.Fatalf("old claim=%v error=%v", oldClaim, err)
	}
	blocked, err := store.ClaimPage(context.Background(), registered, "main-b", now.Add(30*time.Second), time.Minute, 1)
	if err != nil || len(blocked) != 0 {
		t.Fatalf("unexpired takeover=%v error=%v", blocked, err)
	}
	newClaim, err := store.ClaimPage(context.Background(), registered, "main-b", now.Add(time.Minute), time.Minute, 1)
	if err != nil || len(newClaim) != 1 || newClaim[0].LeaseEpoch <= oldClaim[0].LeaseEpoch {
		t.Fatalf("takeover claim=%v error=%v", newClaim, err)
	}
	if err := store.Complete(context.Background(), oldClaim[0], OutcomeLocalCompleted); !errors.Is(err, ErrStaleFence) {
		t.Fatalf("old fence completion error=%v", err)
	}
	if err := store.Complete(context.Background(), newClaim[0], OutcomeLocalCompleted); err != nil {
		t.Fatalf("new fence completion: %v", err)
	}
}

func TestRunnerSlowJobDoesNotBlockIndependentJob(t *testing.T) {
	now := time.Date(2026, 7, 30, 14, 0, 0, 0, time.UTC)
	store := newMemoryStore(now)
	slowStarted := make(chan struct{})
	releaseSlow := make(chan struct{})
	fastCompleted := make(chan struct{})
	slow := Job{
		ID: "slow.v1", Revision: "r1", Mode: ModeLocalBounded,
		Schedule: intervalSchedule(time.Minute), Timeout: time.Second,
		Handler: HandlerFunc(func(context.Context, Occurrence) (Outcome, error) {
			close(slowStarted)
			<-releaseSlow
			return OutcomeLocalCompleted, nil
		}),
	}
	fast := Job{
		ID: "fast.v1", Revision: "r1", Mode: ModeLocalBounded,
		Schedule: intervalSchedule(time.Minute), Timeout: time.Second,
		Handler: HandlerFunc(func(context.Context, Occurrence) (Outcome, error) {
			close(fastCompleted)
			return OutcomeLocalCompleted, nil
		}),
	}
	config := testConfig("main-a")
	config.MaxParallel = 2
	registry, err := NewRegistry(config.LeaseDuration, slow, fast)
	if err != nil {
		t.Fatal(err)
	}
	runner, err := NewRunner(store, registry, config, nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := runner.Tick(context.Background(), now)
		done <- err
	}()
	<-slowStarted
	select {
	case <-fastCompleted:
	case <-time.After(time.Second):
		t.Fatal("independent fast job was blocked by slow job")
	}
	close(releaseSlow)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestRunnerBoundsPagesAndOccurrences(t *testing.T) {
	now := time.Date(2026, 7, 30, 15, 0, 0, 0, time.UTC)
	store := newMemoryStore(now)
	var handled atomic.Int32
	job := Job{
		ID: "bounded.v1", Revision: "r1", Mode: ModeLocalBounded,
		Schedule: intervalSchedule(time.Minute), Timeout: time.Second,
		Handler: HandlerFunc(func(context.Context, Occurrence) (Outcome, error) {
			handled.Add(1)
			return OutcomeLocalCompleted, nil
		}),
	}
	config := testConfig("main-a")
	config.InitialLookback = 10 * time.Minute
	config.MaxOccurrencesPerPlan = 10
	config.PageSize = 2
	config.MaxPagesPerTick = 2
	registry, err := NewRegistry(config.LeaseDuration, job)
	if err != nil {
		t.Fatal(err)
	}
	runner, err := NewRunner(store, registry, config, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Tick(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if result.Planned != 10 || result.Claimed != 4 || result.Pages != 2 || handled.Load() != 4 {
		t.Fatalf("bounded tick result=%+v handled=%d", result, handled.Load())
	}
}

func TestCronDSTAndClockRegressionFixtures(t *testing.T) {
	fallback, err := ParseCron("CRON_TZ=America/New_York 30 1 * * *")
	if err != nil {
		t.Fatal(err)
	}
	first := fallback.Next(time.Date(2026, 11, 1, 4, 0, 0, 0, time.UTC))
	if !first.Equal(time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC)) {
		t.Fatalf("fallback first occurrence=%s", first)
	}
	spring, err := ParseCron("CRON_TZ=America/New_York 30 2 * * *")
	if err != nil {
		t.Fatal(err)
	}
	next := spring.Next(time.Date(2026, 3, 8, 5, 0, 0, 0, time.UTC))
	if !next.Equal(time.Date(2026, 3, 9, 6, 30, 0, 0, time.UTC)) {
		t.Fatalf("nonexistent spring time was not skipped: %s", next)
	}
	job := Job{ID: "clock.v1", Revision: "r1", Mode: ModeLocalBounded, Schedule: intervalSchedule(time.Minute)}
	seeds, through, err := materialize(job, time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC), time.Date(2026, 7, 30, 11, 0, 0, 0, time.UTC), 10)
	if err != nil || len(seeds) != 0 || !through.Equal(time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("clock regression seeds=%v through=%s error=%v", seeds, through, err)
	}
}

func TestModeOutcomeMismatchIsReleased(t *testing.T) {
	now := time.Date(2026, 7, 30, 16, 0, 0, 0, time.UTC)
	store := newMemoryStore(now)
	job := Job{
		ID: "local.v1", Revision: "r1", Mode: ModeLocalBounded,
		Schedule: intervalSchedule(time.Minute), Timeout: time.Second,
		Handler: HandlerFunc(func(context.Context, Occurrence) (Outcome, error) {
			return OutcomeDurablyAdmitted, nil
		}),
	}
	config := testConfig("main-a")
	registry, err := NewRegistry(config.LeaseDuration, job)
	if err != nil {
		t.Fatal(err)
	}
	runner, err := NewRunner(store, registry, config, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Tick(context.Background(), now)
	if !errors.Is(err, ErrInvalidOutcome) || result.Released != 1 || result.Completed != 0 {
		t.Fatalf("mismatch result=%+v error=%v", result, err)
	}
}

func testConfig(instance string) Config {
	return Config{
		InstanceID: instance, TickInterval: time.Minute,
		LeaseDuration: 2 * time.Second, InitialLookback: time.Minute,
		RetryDelay:  100 * time.Millisecond,
		MaxParallel: 2, PageSize: 16, MaxPagesPerTick: 2,
		MaxOccurrencesPerPlan: 16,
	}
}

var _ Store = (*memoryStore)(nil)
