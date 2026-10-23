package runtimecomposition

import (
	"context"
	"errors"
	"io"
	"sort"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	schedulingapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/scheduling"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
)

func artifactRetentionOccurrence(dueAt time.Time) schedulingapp.Occurrence {
	return schedulingapp.Occurrence{
		InvocationID:     "fenced-occurrence-1",
		JobID:            artifactRetentionSweepCapability,
		ScheduleRevision: artifactRetentionSweepRevision,
		DueAt:            dueAt,
		LeaseEpoch:       3,
		ClaimFence:       "0123456789abcdef",
	}
}

func newArtifactRetentionSweepFixture(t *testing.T) (*artifactRetentionSweep, *artifactRetentionFakeObjectsRepo, *artifactRetentionFakeBucketsRepo, *artifactRetentionFakeNotifier, *artifactRetentionFakeStore) {
	t.Helper()
	sweep, objects, buckets, notifier, store, _ := newArtifactRetentionSweepFixtureWithGrants(t)
	return sweep, objects, buckets, notifier, store
}

func newArtifactRetentionSweepFixtureWithGrants(t *testing.T) (
	*artifactRetentionSweep,
	*artifactRetentionFakeObjectsRepo,
	*artifactRetentionFakeBucketsRepo,
	*artifactRetentionFakeNotifier,
	*artifactRetentionFakeStore,
	*artifactRetentionFakeGrantsRepo,
) {
	t.Helper()
	objects := &artifactRetentionFakeObjectsRepo{}
	buckets := &artifactRetentionFakeBucketsRepo{buckets: map[int64]repos.BucketRow{}, notFoundIDs: map[int64]bool{}}
	notifier := &artifactRetentionFakeNotifier{ownerByProject: map[int64]int64{}}
	grants := &artifactRetentionFakeGrantsRepo{}
	store := &artifactRetentionFakeStore{existing: map[string]bool{}, failKeys: map[string]bool{}}
	sweep, err := newArtifactRetentionSweep(objects, buckets, notifier, grants, store)
	if err != nil {
		t.Fatal(err)
	}
	return sweep, objects, buckets, notifier, store, grants
}

func TestArtifactRetentionSweepRejectsInvalidOccurrenceBeforeAnyWork(t *testing.T) {
	base := artifactRetentionOccurrence(time.Now())
	for _, test := range []struct {
		name   string
		mutate func(schedulingapp.Occurrence) schedulingapp.Occurrence
	}{
		{"empty invocation ID", func(o schedulingapp.Occurrence) schedulingapp.Occurrence { o.InvocationID = ""; return o }},
		{"wrong job ID", func(o schedulingapp.Occurrence) schedulingapp.Occurrence { o.JobID = "wrong"; return o }},
		{"wrong revision", func(o schedulingapp.Occurrence) schedulingapp.Occurrence { o.ScheduleRevision = "wrong"; return o }},
		{"zero due at", func(o schedulingapp.Occurrence) schedulingapp.Occurrence { o.DueAt = time.Time{}; return o }},
		{"non-positive lease epoch", func(o schedulingapp.Occurrence) schedulingapp.Occurrence { o.LeaseEpoch = 0; return o }},
		{"empty claim fence", func(o schedulingapp.Occurrence) schedulingapp.Occurrence { o.ClaimFence = ""; return o }},
	} {
		t.Run(test.name, func(t *testing.T) {
			sweep, objects, buckets, notifier, _ := newArtifactRetentionSweepFixture(t)
			outcome, err := sweep.Execute(context.Background(), test.mutate(base))
			if outcome != "" || !errors.Is(err, errArtifactRetentionSweepInvalidOccurrence) {
				t.Fatalf("outcome=%q error=%v", outcome, err)
			}
			if objects.listCalls != 0 || buckets.listNoticeCalls != 0 || notifier.notifyCalls != nil {
				t.Fatalf("expected no repository calls, got objects.listCalls=%d buckets.listNoticeCalls=%d notifyCalls=%v",
					objects.listCalls, buckets.listNoticeCalls, notifier.notifyCalls)
			}
		})
	}
}

func TestArtifactRetentionSweepCompletesWithNothingToDo(t *testing.T) {
	sweep, _, _, _, _ := newArtifactRetentionSweepFixture(t)
	outcome, err := sweep.Execute(context.Background(), artifactRetentionOccurrence(time.Now()))
	if err != nil || outcome != schedulingapp.OutcomeLocalCompleted {
		t.Fatalf("outcome=%q error=%v", outcome, err)
	}
}

func TestArtifactRetentionSweepDeletesExpiredObjectsAcrossBucketsAndLeavesUnexpiredAlone(t *testing.T) {
	sweep, objects, buckets, _, store := newArtifactRetentionSweepFixture(t)
	past := time.Now().Add(-time.Hour)
	future := time.Now().Add(time.Hour)

	buckets.seed(repos.BucketRow{ID: 1, ProjectID: 100, Name: "reports"})
	buckets.seed(repos.BucketRow{ID: 2, ProjectID: 200, Name: "tasks"})

	objects.seed(repos.ObjectRow{ID: 10, BucketID: 1, Key: "old.png", ExpiresAt: &past})
	objects.seed(repos.ObjectRow{ID: 11, BucketID: 1, Key: "fresh.png", ExpiresAt: &future})
	objects.seed(repos.ObjectRow{ID: 12, BucketID: 2, Key: "old.bin", ExpiresAt: &past})
	store.existing["p/100/b/reports/o/old.png"] = true
	store.existing["p/100/b/reports/o/fresh.png"] = true
	store.existing["p/200/b/tasks/o/old.bin"] = true

	outcome, err := sweep.Execute(context.Background(), artifactRetentionOccurrence(time.Now()))
	if err != nil || outcome != schedulingapp.OutcomeLocalCompleted {
		t.Fatalf("outcome=%q error=%v", outcome, err)
	}

	remaining := objects.snapshot()
	if len(remaining) != 1 || remaining[0].ID != 11 {
		t.Fatalf("expected only the unexpired fresh.png to remain, got %+v", remaining)
	}
	if store.existing["p/100/b/reports/o/old.png"] || store.existing["p/200/b/tasks/o/old.bin"] {
		t.Fatal("expected both expired objects' physical bytes to be deleted")
	}
	if !store.existing["p/100/b/reports/o/fresh.png"] {
		t.Fatal("expected the unexpired object's physical bytes to survive")
	}
}

func TestArtifactRetentionSweepPaginatesAcrossMultipleBatches(t *testing.T) {
	sweep, objects, buckets, _, store := newArtifactRetentionSweepFixture(t)
	past := time.Now().Add(-time.Hour)
	buckets.seed(repos.BucketRow{ID: 1, ProjectID: 100, Name: "reports"})

	const n = int(artifactRetentionSweepBatchSize) + 137
	for i := 0; i < n; i++ {
		key := "bulk-" + string(rune('a'+i%26)) + "-" + itoa(i) + ".bin"
		objects.seed(repos.ObjectRow{ID: int64(i + 1), BucketID: 1, Key: key, ExpiresAt: &past})
		store.existing["p/100/b/reports/o/"+key] = true
	}

	outcome, err := sweep.Execute(context.Background(), artifactRetentionOccurrence(time.Now()))
	if err != nil || outcome != schedulingapp.OutcomeLocalCompleted {
		t.Fatalf("outcome=%q error=%v", outcome, err)
	}
	if remaining := objects.snapshot(); len(remaining) != 0 {
		t.Fatalf("expected every expired object to be swept across batches, got %d remaining", len(remaining))
	}
	if objects.listCalls < 2 {
		t.Fatalf("expected more than one ListExpiredObjects call to exercise pagination, got %d", objects.listCalls)
	}
}

// TestArtifactRetentionSweepPreservesMetadataConsistencyOnPartialDeleteFailure
// mirrors S13's regression test for artifactbootstrap.purgeObjects: a
// partial DeleteBatch failure must not orphan the metadata row for the
// object that DID delete.
func TestArtifactRetentionSweepPreservesMetadataConsistencyOnPartialDeleteFailure(t *testing.T) {
	sweep, objects, buckets, _, store := newArtifactRetentionSweepFixture(t)
	past := time.Now().Add(-time.Hour)
	buckets.seed(repos.BucketRow{ID: 1, ProjectID: 100, Name: "reports"})
	objects.seed(repos.ObjectRow{ID: 10, BucketID: 1, Key: "ok.png", ExpiresAt: &past})
	objects.seed(repos.ObjectRow{ID: 11, BucketID: 1, Key: "stuck.png", ExpiresAt: &past})
	store.existing["p/100/b/reports/o/ok.png"] = true
	store.existing["p/100/b/reports/o/stuck.png"] = true
	store.failKeys["stuck.png"] = true

	_, err := sweep.Execute(context.Background(), artifactRetentionOccurrence(time.Now()))
	if err == nil {
		t.Fatal("expected an error from the simulated partial delete failure, got nil")
	}

	remaining := objects.snapshot()
	if len(remaining) != 1 || remaining[0].Key != "stuck.png" {
		t.Fatalf("expected only the still-undeleted stuck.png metadata row to remain (ok.png's row must be cleaned up), got %+v", remaining)
	}
	if store.existing["p/100/b/reports/o/ok.png"] {
		t.Fatal("expected ok.png's physical bytes to be deleted")
	}
	if !store.existing["p/100/b/reports/o/stuck.png"] {
		t.Fatal("expected stuck.png's physical bytes to survive the simulated failure")
	}
}

func TestArtifactRetentionSweepDeletesOrphanedMetadataForMissingBucket(t *testing.T) {
	sweep, objects, buckets, _, store := newArtifactRetentionSweepFixture(t)
	past := time.Now().Add(-time.Hour)
	buckets.notFoundIDs[1] = true
	objects.seed(repos.ObjectRow{ID: 10, BucketID: 1, Key: "orphan.png", ExpiresAt: &past})

	outcome, err := sweep.Execute(context.Background(), artifactRetentionOccurrence(time.Now()))
	if err != nil || outcome != schedulingapp.OutcomeLocalCompleted {
		t.Fatalf("outcome=%q error=%v", outcome, err)
	}
	if remaining := objects.snapshot(); len(remaining) != 0 {
		t.Fatalf("expected the orphaned metadata row to be deleted, got %+v", remaining)
	}
	if store.deleteBatchCalls != 0 {
		t.Fatalf("expected no ObjectStore.DeleteBatch call for a bucket that no longer exists, got %d", store.deleteBatchCalls)
	}
}

func TestArtifactRetentionSweepNotifiesExpiringBucketsAndMarksThemNotified(t *testing.T) {
	sweep, _, buckets, notifier, _ := newArtifactRetentionSweepFixture(t)
	expiresAt := time.Now().Add(12 * time.Hour)
	buckets.needingNotice = []repos.BucketRow{
		{ID: 5, ProjectID: 100, Name: "reports", ExpiresAt: &expiresAt},
	}
	notifier.ownerByProject[100] = 777

	outcome, err := sweep.Execute(context.Background(), artifactRetentionOccurrence(time.Now()))
	if err != nil || outcome != schedulingapp.OutcomeLocalCompleted {
		t.Fatalf("outcome=%q error=%v", outcome, err)
	}
	if len(notifier.notifyCalls) != 1 {
		t.Fatalf("expected exactly 1 notify call, got %d", len(notifier.notifyCalls))
	}
	call := notifier.notifyCalls[0]
	if call.projectID != 100 || call.userID != 777 || call.bucketID != 5 || !call.expiresAt.Equal(expiresAt) {
		t.Fatalf("unexpected notify call: %+v", call)
	}
	if len(buckets.markedNotified) != 1 || buckets.markedNotified[0] != 5 {
		t.Fatalf("expected bucket 5 to be marked notified, got %v", buckets.markedNotified)
	}
}

// TestArtifactRetentionSweepUpdatesProjectByteUsageGaugeForEveryKnownProject
// proves S18's per-project byte-usage gauge is refreshed on the sweeper
// tick, for every project that owns a bucket — not zero of them, not just
// the first. The gauge's own recorded value is asserted directly against
// the real OTel instrument in internal/infra/storage's own test suite
// (TestArtifactObservability*, S18's Verify command target); this test's
// job is proving the SWEEPER side actually calls SumProjectBytes for the
// right project set on every tick, which is the part storage's own tests
// cannot see.
func TestArtifactRetentionSweepUpdatesProjectByteUsageGaugeForEveryKnownProject(t *testing.T) {
	sweep, objects, buckets, _, _ := newArtifactRetentionSweepFixture(t)
	buckets.seed(repos.BucketRow{ID: 1, ProjectID: 100, Name: "reports"})
	buckets.seed(repos.BucketRow{ID: 2, ProjectID: 200, Name: "exports"})
	objects.seedProjectBytes(100, 4096)
	objects.seedProjectBytes(200, 8192)

	outcome, err := sweep.Execute(context.Background(), artifactRetentionOccurrence(time.Now()))
	if err != nil || outcome != schedulingapp.OutcomeLocalCompleted {
		t.Fatalf("outcome=%q error=%v", outcome, err)
	}

	got := append([]int64(nil), objects.sumProjectBytesCalls...)
	sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
	want := []int64{100, 200}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("SumProjectBytes calls = %v, want %v", got, want)
	}
}

// artifactRetentionGaugeTestReader installs a ManualReader-backed
// MeterProvider for this test binary exactly once — see
// internal/infra/storage/observability_test.go's own harness for why: OTel's
// global package delegates every otel.Meter(...)-created instrument
// (including storage.RecordProjectByteUsage's package-level gauge) to
// whichever provider otel.SetMeterProvider installs on its *first* call in
// the process, guarded by a package-level sync.Once — a second call would
// not re-delegate. This test binary (internal/runtimecomposition's own
// `go test` process) is independent of internal/infra/storage's, so
// installing a test provider here does not conflict with that package's own
// test binary.
var (
	artifactRetentionGaugeReaderOnce sync.Once
	artifactRetentionGaugeReader     *sdkmetric.ManualReader
)

func setupArtifactRetentionGaugeReader(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	artifactRetentionGaugeReaderOnce.Do(func() {
		artifactRetentionGaugeReader = sdkmetric.NewManualReader()
		otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(artifactRetentionGaugeReader)))
	})
	return artifactRetentionGaugeReader
}

// gaugeValueForProject reads storage's project_bytes_used gauge's current
// value for projectID directly, bypassing the sweeper entirely — proof this
// test observes what the sweeper actually recorded, not a value it computed
// independently.
func gaugeValueForProject(t *testing.T, reader *sdkmetric.ManualReader, projectID int64) (int64, bool) {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "artifact.storage.project_bytes_used" {
				continue
			}
			gauge, ok := m.Data.(metricdata.Gauge[int64])
			if !ok {
				t.Fatalf("project_bytes_used: data is %T, want Gauge[int64]", m.Data)
			}
			for _, dp := range gauge.DataPoints {
				pid, _ := dp.Attributes.Value("project_id")
				if pid.AsInt64() == projectID {
					return dp.Value, true
				}
			}
		}
	}
	return 0, false
}

// TestArtifactRetentionSweepRecordsTheActualByteTotalNotJustCallPresence
// closes a real gap an adversarial review found: the sibling test above only
// checks that SumProjectBytes was called for the right project IDs, which
// would still pass even if storage.RecordProjectByteUsage's two int64
// arguments were accidentally swapped at the sweeper's call site (labelling
// the gauge with a byte count and recording a project ID as the byte
// value) — nothing else in this codebase's test suite reads the gauge back
// through a real sweep. This test does, via the same OTel test-harness
// technique internal/infra/storage/observability_test.go itself uses.
func TestArtifactRetentionSweepRecordsTheActualByteTotalNotJustCallPresence(t *testing.T) {
	reader := setupArtifactRetentionGaugeReader(t)
	sweep, objects, buckets, _, _ := newArtifactRetentionSweepFixture(t)

	// Distinct, easily-misattributable project IDs and byte totals — a
	// swapped-argument regression (project ID recorded as the value, or
	// vice versa) would be immediately visible in the assertion below,
	// unlike e.g. project_id=100/total=100 where a swap is invisible.
	const projectID = int64(918_222_001)
	const wantTotal = int64(55_667_788)
	buckets.seed(repos.BucketRow{ID: 1, ProjectID: projectID, Name: "reports"})
	objects.seedProjectBytes(projectID, wantTotal)

	outcome, err := sweep.Execute(context.Background(), artifactRetentionOccurrence(time.Now()))
	if err != nil || outcome != schedulingapp.OutcomeLocalCompleted {
		t.Fatalf("outcome=%q error=%v", outcome, err)
	}

	got, ok := gaugeValueForProject(t, reader, projectID)
	if !ok {
		t.Fatalf("project_bytes_used: no data point for project_id %d", projectID)
	}
	if got != wantTotal {
		t.Fatalf("project_bytes_used for project %d = %d, want %d (SumProjectBytes's return value, unmodified)", projectID, got, wantTotal)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

type artifactRetentionFakeObjectsRepo struct {
	mu              sync.Mutex
	objects         []repos.ObjectRow
	listCalls       int
	deleteRowsCalls int
	// projectBytes and sumProjectBytesCalls back S18's SumProjectBytes —
	// set directly via seedProjectBytes rather than derived from objects
	// above: real SumProjectBytes joins through buckets to resolve
	// project_id (elitea_storage.objects has no project_id column of its
	// own, only bucket_id), a relationship this fake's sibling
	// artifactRetentionFakeBucketsRepo owns, not this one — deriving it
	// here would couple two independently-seeded fakes for no test value.
	projectBytes         map[int64]int64
	sumProjectBytesCalls []int64
}

func (r *artifactRetentionFakeObjectsRepo) seedProjectBytes(projectID, bytes int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.projectBytes == nil {
		r.projectBytes = map[int64]int64{}
	}
	r.projectBytes[projectID] = bytes
}

func (r *artifactRetentionFakeObjectsRepo) SumProjectBytes(_ context.Context, projectID int64) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sumProjectBytesCalls = append(r.sumProjectBytesCalls, projectID)
	return r.projectBytes[projectID], nil
}

func (r *artifactRetentionFakeObjectsRepo) seed(obj repos.ObjectRow) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.objects = append(r.objects, obj)
}

func (r *artifactRetentionFakeObjectsRepo) snapshot() []repos.ObjectRow {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]repos.ObjectRow(nil), r.objects...)
}

func (r *artifactRetentionFakeObjectsRepo) ListExpiredObjects(_ context.Context, olderThan time.Time, limit int32) ([]repos.ObjectRow, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.listCalls++
	var out []repos.ObjectRow
	for _, obj := range r.objects {
		if obj.ExpiresAt != nil && obj.ExpiresAt.Before(olderThan) {
			out = append(out, obj)
			if int32(len(out)) >= limit {
				break
			}
		}
	}
	return out, nil
}

func (r *artifactRetentionFakeObjectsRepo) DeleteObjectRows(_ context.Context, ids []int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deleteRowsCalls++
	idSet := make(map[int64]bool, len(ids))
	for _, id := range ids {
		idSet[id] = true
	}
	var remaining []repos.ObjectRow
	for _, obj := range r.objects {
		if !idSet[obj.ID] {
			remaining = append(remaining, obj)
		}
	}
	r.objects = remaining
	return nil
}

type artifactRetentionFakeBucketsRepo struct {
	mu              sync.Mutex
	buckets         map[int64]repos.BucketRow
	notFoundIDs     map[int64]bool
	needingNotice   []repos.BucketRow
	markedNotified  []int64
	listNoticeCalls int
}

func (r *artifactRetentionFakeBucketsRepo) seed(bucket repos.BucketRow) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buckets[bucket.ID] = bucket
}

func (r *artifactRetentionFakeBucketsRepo) GetBucketByID(_ context.Context, id int64) (repos.BucketRow, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.notFoundIDs[id] {
		return repos.BucketRow{}, storage.ErrNotFound
	}
	b, ok := r.buckets[id]
	if !ok {
		return repos.BucketRow{}, storage.ErrNotFound
	}
	return b, nil
}

func (r *artifactRetentionFakeBucketsRepo) ListBucketsNeedingExpiryNotice(_ context.Context, _ time.Duration, limit int32) ([]repos.BucketRow, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.listNoticeCalls++
	out := r.needingNotice
	if int32(len(out)) > limit {
		out = out[:limit]
	}
	return append([]repos.BucketRow(nil), out...), nil
}

func (r *artifactRetentionFakeBucketsRepo) MarkBucketNotified(_ context.Context, bucketID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.markedNotified = append(r.markedNotified, bucketID)
	return nil
}

// ListProjectIDsWithBuckets (S18) derives the distinct project set from
// r.buckets — unlike SumProjectBytes on the sibling objects fake, this one
// genuinely can be derived correctly, since every seeded BucketRow already
// carries its own ProjectID.
func (r *artifactRetentionFakeBucketsRepo) ListProjectIDsWithBuckets(_ context.Context) ([]int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	seen := map[int64]bool{}
	var ids []int64
	for _, b := range r.buckets {
		if !seen[b.ProjectID] {
			seen[b.ProjectID] = true
			ids = append(ids, b.ProjectID)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, nil
}

type artifactRetentionNotifyCall struct {
	projectID, userID, bucketID int64
	expiresAt                   time.Time
}

type artifactRetentionFakeNotifier struct {
	mu             sync.Mutex
	ownerByProject map[int64]int64
	notifyCalls    []artifactRetentionNotifyCall
}

func (n *artifactRetentionFakeNotifier) ProjectOwnerUserID(_ context.Context, projectID int64) (int64, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.ownerByProject[projectID], nil
}

func (n *artifactRetentionFakeNotifier) NotifyBucketExpiring(_ context.Context, projectID, userID, bucketID int64, expiresAt time.Time) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.notifyCalls = append(n.notifyCalls, artifactRetentionNotifyCall{projectID, userID, bucketID, expiresAt})
	return nil
}

// artifactRetentionFakeStore is a minimal storage.ObjectStore double —
// DeleteBatch is the only method this package's sweep ever calls; the rest
// exist to satisfy the interface and return storage.ErrNotSupported.
// existing/failKeys are keyed by the same "{projectID}/{bucket}/{key}"
// storage key ObjectRef.StorageKey("") produces.
type artifactRetentionFakeStore struct {
	mu               sync.Mutex
	existing         map[string]bool
	failKeys         map[string]bool
	deleteBatchCalls int
	// deleted and aborted record the grant-reclamation calls. Delete and
	// AbortMultipart returned ErrNotSupported before the grant sweep
	// existed, because nothing in the sweeper called them.
	deleted   []string
	aborted   []string
	deleteErr error
	abortErr  error
}

func (s *artifactRetentionFakeStore) DeleteBatch(_ context.Context, refs []storage.ObjectRef) (storage.BatchResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleteBatchCalls++
	result := storage.BatchResult{}
	for _, ref := range refs {
		if s.failKeys[ref.Key()] {
			result.Failed = append(result.Failed, storage.BatchError{Key: ref.Key(), Err: errors.New("simulated delete failure")})
			continue
		}
		delete(s.existing, ref.StorageKey(""))
		result.Deleted = append(result.Deleted, ref.Key())
	}
	return result, nil
}

func (s *artifactRetentionFakeStore) Put(context.Context, storage.ObjectRef, io.Reader, storage.PutOptions) (storage.ObjectInfo, error) {
	return storage.ObjectInfo{}, storage.ErrNotSupported
}

func (s *artifactRetentionFakeStore) Get(context.Context, storage.ObjectRef, *storage.ByteRange) (io.ReadCloser, storage.ObjectInfo, error) {
	return nil, storage.ObjectInfo{}, storage.ErrNotSupported
}

func (s *artifactRetentionFakeStore) Stat(context.Context, storage.ObjectRef) (storage.ObjectInfo, error) {
	return storage.ObjectInfo{}, storage.ErrNotSupported
}

func (s *artifactRetentionFakeStore) Delete(_ context.Context, ref storage.ObjectRef) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleted = append(s.deleted, ref.Key())
	if s.deleteErr != nil {
		return s.deleteErr
	}
	delete(s.existing, ref.StorageKey(""))
	return nil
}

func (s *artifactRetentionFakeStore) List(context.Context, storage.ListQuery) (storage.ListPage, error) {
	return storage.ListPage{}, storage.ErrNotSupported
}

func (s *artifactRetentionFakeStore) PresignGet(context.Context, storage.ObjectRef, time.Duration) (string, error) {
	return "", storage.ErrNotSupported
}

func (s *artifactRetentionFakeStore) PresignPut(context.Context, storage.ObjectRef, time.Duration, storage.PutOptions) (string, error) {
	return "", storage.ErrNotSupported
}

func (s *artifactRetentionFakeStore) StartMultipart(context.Context, storage.ObjectRef, storage.PutOptions) (storage.UploadID, error) {
	return "", storage.ErrNotSupported
}

func (s *artifactRetentionFakeStore) PresignPart(context.Context, storage.ObjectRef, storage.UploadID, int32, time.Duration) (string, error) {
	return "", storage.ErrNotSupported
}

func (s *artifactRetentionFakeStore) CompleteMultipart(context.Context, storage.ObjectRef, storage.UploadID, []storage.Part) (storage.ObjectInfo, error) {
	return storage.ObjectInfo{}, storage.ErrNotSupported
}

func (s *artifactRetentionFakeStore) AbortMultipart(_ context.Context, ref storage.ObjectRef, _ storage.UploadID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.aborted = append(s.aborted, ref.Key())
	return s.abortErr
}

func (s *artifactRetentionFakeStore) Capabilities() storage.Capabilities {
	return storage.Capabilities{}
}

var _ storage.ObjectStore = (*artifactRetentionFakeStore)(nil)

// artifactRetentionFakeGrantsRepo hands the sweeper one batch of claimed
// grants and then nothing. That matches the real query. The real query cannot
// return the same row twice, because it stamps consumed_at as it reads.
type artifactRetentionFakeGrantsRepo struct {
	pending    []repos.ExpiredTransferGrantRow
	claimCalls []time.Time
	claimErr   error

	// staged is what ListExpiredGrantStaging answers until ClearGrantStaging
	// removes a row, as the real query stops matching a cleared key.
	staged       []repos.GrantStagingRow
	stagingCalls []time.Time
	cleared      []string
}

func (r *artifactRetentionFakeGrantsRepo) ListExpiredGrantStaging(_ context.Context, olderThan time.Time, limit int32) ([]repos.GrantStagingRow, error) {
	r.stagingCalls = append(r.stagingCalls, olderThan)
	batch := r.staged
	if int32(len(batch)) > limit {
		batch = batch[:limit]
	}
	return append([]repos.GrantStagingRow(nil), batch...), nil
}

func (r *artifactRetentionFakeGrantsRepo) ClearGrantStaging(_ context.Context, id, key string) error {
	r.cleared = append(r.cleared, id)
	kept := r.staged[:0]
	for _, row := range r.staged {
		if row.ID != id || row.Key != key {
			kept = append(kept, row)
		}
	}
	r.staged = kept
	return nil
}

func (r *artifactRetentionFakeGrantsRepo) ClaimExpiredTransferGrants(_ context.Context, olderThan time.Time, limit int32) ([]repos.ExpiredTransferGrantRow, error) {
	r.claimCalls = append(r.claimCalls, olderThan)
	if r.claimErr != nil {
		return nil, r.claimErr
	}
	if len(r.pending) == 0 {
		return nil, nil
	}
	batch := r.pending
	if int32(len(batch)) > limit {
		batch = batch[:limit]
	}
	r.pending = r.pending[len(batch):]
	return batch, nil
}

var _ artifactRetentionGrantsRepository = (*artifactRetentionFakeGrantsRepo)(nil)

// artifactRetentionFakeAttachmentChunksRepo is S20a's cleanup-step fake —
// see artifactRetentionAttachmentChunksRepository's own doc comment for why
// this dependency is optional rather than part of newArtifactRetentionSweepFixture.
type artifactRetentionFakeAttachmentChunksRepo struct {
	deleteStaleCalls []time.Time
	deleteStaleErr   error
}

func (r *artifactRetentionFakeAttachmentChunksRepo) DeleteStaleChunks(_ context.Context, olderThan time.Time) (int64, error) {
	r.deleteStaleCalls = append(r.deleteStaleCalls, olderThan)
	if r.deleteStaleErr != nil {
		return 0, r.deleteStaleErr
	}
	return 0, nil
}

var _ artifactRetentionAttachmentChunksRepository = (*artifactRetentionFakeAttachmentChunksRepo)(nil)

// TestArtifactRetentionSweepSkipsAttachmentChunkCleanupWhenNotWired proves
// the dependency is genuinely optional: a sweep built via the plain
// newArtifactRetentionSweepFixture (no WithAttachmentChunks call) still
// completes normally — S20a's cleanup step must not become a hard
// requirement for every other caller of this sweeper.
func TestArtifactRetentionSweepSkipsAttachmentChunkCleanupWhenNotWired(t *testing.T) {
	sweep, _, _, _, _ := newArtifactRetentionSweepFixture(t)
	outcome, err := sweep.Execute(context.Background(), artifactRetentionOccurrence(time.Now()))
	if err != nil || outcome != schedulingapp.OutcomeLocalCompleted {
		t.Fatalf("outcome=%q error=%v", outcome, err)
	}
}

// TestArtifactRetentionSweepCleansUpStaleAttachmentChunksWhenWired proves
// Execute calls DeleteStaleChunks with a cutoff in the past (now minus the
// TTL) once WithAttachmentChunks is used to wire the dependency in.
func TestArtifactRetentionSweepCleansUpStaleAttachmentChunksWhenWired(t *testing.T) {
	sweep, _, _, _, _ := newArtifactRetentionSweepFixture(t)
	chunks := &artifactRetentionFakeAttachmentChunksRepo{}
	sweep = sweep.WithAttachmentChunks(chunks)

	before := time.Now()
	outcome, err := sweep.Execute(context.Background(), artifactRetentionOccurrence(time.Now()))
	if err != nil || outcome != schedulingapp.OutcomeLocalCompleted {
		t.Fatalf("outcome=%q error=%v", outcome, err)
	}

	if len(chunks.deleteStaleCalls) != 1 {
		t.Fatalf("DeleteStaleChunks calls = %d, want 1", len(chunks.deleteStaleCalls))
	}
	cutoff := chunks.deleteStaleCalls[0]
	if !cutoff.Before(before) {
		t.Fatalf("DeleteStaleChunks cutoff = %v, want a time before this test started (now - TTL)", cutoff)
	}
}

// TestArtifactRetentionSweepPropagatesAttachmentChunkCleanupFailure proves a
// cleanup failure fails the whole tick — the same failure posture every
// other Execute step already has, not a silently-ignored best-effort step.
func TestArtifactRetentionSweepPropagatesAttachmentChunkCleanupFailure(t *testing.T) {
	sweep, _, _, _, _ := newArtifactRetentionSweepFixture(t)
	chunks := &artifactRetentionFakeAttachmentChunksRepo{deleteStaleErr: errors.New("boom")}
	sweep = sweep.WithAttachmentChunks(chunks)

	outcome, err := sweep.Execute(context.Background(), artifactRetentionOccurrence(time.Now()))
	if err == nil || outcome != "" {
		t.Fatalf("outcome=%q error=%v, want a propagated error and empty outcome", outcome, err)
	}
}

// TestArtifactRetentionSweepReclaimsBytesBehindAnUncommittedGrant pins the
// grant half of the retention sweep.
//
// DEFECT: CreateTransferGrant issued a presigned PUT and wrote only a
// transfer_grants row. The elitea_storage.objects row was written by the
// commit and nowhere else. A caller could therefore ask for a grant with
// max_bytes=1, and then upload gigabytes to the signed URL. S3 enforces no
// size on a presigned PUT. The caller could then never commit. The bytes then had no objects row, so
// SumProjectBytes never counted them against max_total_bytes and
// ListExpiredObjects never saw them. Execute ran sweepExpiredObjects,
// notifyExpiringBuckets, updateProjectByteUsageGauges and
// cleanupStaleAttachmentChunks — nothing grant-related — and no query in the
// tree scanned transfer_grants for expiry. Repeating the loop filled the
// storage account with unbilled, unreclaimable data.
func TestArtifactRetentionSweepReclaimsBytesBehindAnUncommittedGrant(t *testing.T) {
	sweep, _, buckets, _, store, grants := newArtifactRetentionSweepFixtureWithGrants(t)
	buckets.buckets[7] = repos.BucketRow{ID: 7, ProjectID: 5, Name: "reports"}
	uploadID := "upload-1"
	grants.pending = []repos.ExpiredTransferGrantRow{
		{ID: "grant-put", ProjectID: 5, BucketID: 7, Key: "abandoned-put", Method: "PUT"},
		{ID: "grant-multipart", ProjectID: 5, BucketID: 7, Key: "abandoned-parts", Method: "PUT", UploadID: &uploadID},
		// A GET grant reserves no bytes: its key is a grant id nothing ever
		// wrote to. Touching the store for one would be a wasted call.
		{ID: "grant-get", ProjectID: 5, BucketID: 7, Key: "read-only", Method: "GET"},
	}

	before := time.Now()
	outcome, err := sweep.Execute(context.Background(), artifactRetentionOccurrence(time.Now()))
	if err != nil || outcome != schedulingapp.OutcomeLocalCompleted {
		t.Fatalf("outcome=%q error=%v", outcome, err)
	}

	if len(store.deleted) != 1 || store.deleted[0] != "abandoned-put" {
		t.Errorf("Delete calls = %v, want exactly [abandoned-put]", store.deleted)
	}
	if len(store.aborted) != 1 || store.aborted[0] != "abandoned-parts" {
		t.Errorf("AbortMultipart calls = %v, want exactly [abandoned-parts]", store.aborted)
	}
	if len(grants.claimCalls) == 0 {
		t.Fatal("the sweep never claimed a grant")
	}
	// The claim must lag the clock. A commit that started before the grant
	// expired streams the whole object through a SHA-256 hasher, so it can
	// still be running when expires_at passes. Claiming that grant would let
	// the sweeper delete the bytes under a commit. That commit then records a
	// metadata row for an object that no longer exists.
	if !grants.claimCalls[0].Before(before) {
		t.Errorf("claim cutoff %v is not before now; a commit in flight can be raced", grants.claimCalls[0])
	}
}

// TestArtifactRetentionSweepContinuesPastOneUnreclaimableGrant proves one
// bad bucket cannot stall the rest of the tick. The error still surfaces.
func TestArtifactRetentionSweepContinuesPastOneUnreclaimableGrant(t *testing.T) {
	sweep, _, buckets, _, store, grants := newArtifactRetentionSweepFixtureWithGrants(t)
	buckets.buckets[7] = repos.BucketRow{ID: 7, ProjectID: 5, Name: "reports"}
	store.deleteErr = errors.New("simulated backend outage")
	grants.pending = []repos.ExpiredTransferGrantRow{
		{ID: "grant-a", ProjectID: 5, BucketID: 7, Key: "first", Method: "PUT"},
		{ID: "grant-b", ProjectID: 5, BucketID: 7, Key: "second", Method: "PUT"},
	}

	if _, err := sweep.Execute(context.Background(), artifactRetentionOccurrence(time.Now())); err == nil {
		t.Fatal("expected the storage error to surface")
	}
	if len(store.deleted) != 2 {
		t.Errorf("Delete calls = %v, want both grants attempted", store.deleted)
	}
}

// TestArtifactRetentionSweepTreatsAnUnusedGrantAsReclaimed proves the common
// case is not an error: most grants are never uploaded to, so the object the
// sweep tries to delete does not exist.
func TestArtifactRetentionSweepTreatsAnUnusedGrantAsReclaimed(t *testing.T) {
	sweep, _, buckets, _, store, grants := newArtifactRetentionSweepFixtureWithGrants(t)
	buckets.buckets[7] = repos.BucketRow{ID: 7, ProjectID: 5, Name: "reports"}
	store.deleteErr = storage.ErrNotFound
	grants.pending = []repos.ExpiredTransferGrantRow{
		{ID: "grant-unused", ProjectID: 5, BucketID: 7, Key: "never-uploaded", Method: "PUT"},
	}

	outcome, err := sweep.Execute(context.Background(), artifactRetentionOccurrence(time.Now()))
	if err != nil || outcome != schedulingapp.OutcomeLocalCompleted {
		t.Fatalf("outcome=%q error=%v", outcome, err)
	}
}

// A staged grant's URL stays valid after its commit, so a PUT then
// recreates the staging object under a consumed grant that
// sweepExpiredGrants never claims. sweepGrantStaging deletes it once the URL
// has expired, and clears the marker only after the delete succeeded.
func TestArtifactRetentionSweepReclaimsCommittedGrantStaging(t *testing.T) {
	sweep, _, buckets, _, store, grants := newArtifactRetentionSweepFixtureWithGrants(t)
	buckets.buckets[7] = repos.BucketRow{ID: 7, ProjectID: 5, Name: "reports"}
	grants.staged = []repos.GrantStagingRow{{ID: "grant-1", ProjectID: 5, BucketID: 7, Key: "grant-1.upload"}}

	store.deleteErr = errors.New("simulated backend outage")
	before := time.Now()
	if _, err := sweep.Execute(context.Background(), artifactRetentionOccurrence(time.Now())); err == nil {
		t.Fatal("expected the storage error to surface")
	}
	if len(grants.cleared) != 0 {
		t.Fatalf("a failed delete cleared the staging marker: %v", grants.cleared)
	}
	if len(grants.stagingCalls) == 0 || !grants.stagingCalls[0].Before(before.Add(-artifactRetentionGrantGrace+time.Second)) {
		t.Fatalf("staging cutoff %v does not lag by the grant grace", grants.stagingCalls)
	}

	store.deleteErr = nil
	store.deleted = nil
	outcome, err := sweep.Execute(context.Background(), artifactRetentionOccurrence(time.Now()))
	if err != nil || outcome != schedulingapp.OutcomeLocalCompleted {
		t.Fatalf("outcome=%q error=%v", outcome, err)
	}
	if len(store.deleted) != 1 || store.deleted[0] != "grant-1.upload" {
		t.Fatalf("Delete calls = %v, want exactly [grant-1.upload]", store.deleted)
	}
	if len(grants.cleared) != 1 || grants.cleared[0] != "grant-1" || len(grants.staged) != 0 {
		t.Fatalf("cleared = %v, staged = %v", grants.cleared, grants.staged)
	}
}

// An uncommitted staged grant can also hold a copy at its final key: a commit
// copies before it verifies, and a commit that failed after the copy leaves
// it. The claim reclaims both.
func TestArtifactRetentionSweepReclaimsBothKeysOfAnUncommittedStagedGrant(t *testing.T) {
	sweep, _, buckets, _, store, grants := newArtifactRetentionSweepFixtureWithGrants(t)
	buckets.buckets[7] = repos.BucketRow{ID: 7, ProjectID: 5, Name: "reports"}
	grants.pending = []repos.ExpiredTransferGrantRow{
		{ID: "grant-2", ProjectID: 5, BucketID: 7, Key: "grant-2.upload", Method: "PUT"},
	}

	if _, err := sweep.Execute(context.Background(), artifactRetentionOccurrence(time.Now())); err != nil {
		t.Fatal(err)
	}
	if len(store.deleted) != 2 || store.deleted[0] != "grant-2.upload" || store.deleted[1] != "grant-2" {
		t.Fatalf("Delete calls = %v, want [grant-2.upload grant-2]", store.deleted)
	}
}
