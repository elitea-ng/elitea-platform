package storage

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

// observability.go's instruments are package-level vars created in init(),
// which runs before any test — against whatever MeterProvider is the OTel
// global default at that point (a no-op stub, absent an explicit
// otel.SetMeterProvider call). otel's global package delegates every
// instrument created before the FIRST otel.SetMeterProvider call to
// whichever provider that first call installs (see
// go.opentelemetry.io/otel/internal/global/state.go: SetMeterProvider guards
// the actual re-delegation with a package-level sync.Once, so a second call
// in this same test binary would NOT re-delegate the already-delegated
// instruments — it would only change what a future otel.Meter(...) call
// returns). So the test MeterProvider must be installed exactly once, shared
// by every test in this package, and each test disambiguates its own
// recorded data points with a unique attribute value (backend or
// project_id) rather than relying on isolated per-test readers.
var (
	testMeterOnce   sync.Once
	testMeterReader *sdkmetric.ManualReader
)

func setupTestMeterProvider(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	testMeterOnce.Do(func() {
		testMeterReader = sdkmetric.NewManualReader()
		otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(testMeterReader)))
	})
	return testMeterReader
}

func collectMetric(t *testing.T, reader *sdkmetric.ManualReader, name string) (metricdata.Metrics, bool) {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				return m, true
			}
		}
	}
	return metricdata.Metrics{}, false
}

// observabilityFakeStore is a minimal ObjectStore double whose every method
// returns caller-configured, fixed results — this test exercises
// instrumentedStore's own wrapping behavior, not any real backend, so the
// inner store only needs to be programmable, not realistic.
type observabilityFakeStore struct {
	putInfo           ObjectInfo
	putErr            error
	getBody           io.ReadCloser
	getInfo           ObjectInfo
	getErr            error
	statErr           error
	deleteErr         error
	deleteBatchResult BatchResult
	deleteBatchErr    error
}

func (f *observabilityFakeStore) Put(context.Context, ObjectRef, io.Reader, PutOptions) (ObjectInfo, error) {
	return f.putInfo, f.putErr
}

func (f *observabilityFakeStore) Get(context.Context, ObjectRef, *ByteRange) (io.ReadCloser, ObjectInfo, error) {
	return f.getBody, f.getInfo, f.getErr
}

func (f *observabilityFakeStore) Stat(context.Context, ObjectRef) (ObjectInfo, error) {
	return ObjectInfo{}, f.statErr
}

func (f *observabilityFakeStore) Delete(context.Context, ObjectRef) error {
	return f.deleteErr
}

func (f *observabilityFakeStore) DeleteBatch(context.Context, []ObjectRef) (BatchResult, error) {
	return f.deleteBatchResult, f.deleteBatchErr
}

func (f *observabilityFakeStore) List(context.Context, ListQuery) (ListPage, error) {
	return ListPage{}, nil
}

func (f *observabilityFakeStore) PresignGet(context.Context, ObjectRef, time.Duration) (string, error) {
	return "", nil
}

func (f *observabilityFakeStore) PresignPut(context.Context, ObjectRef, time.Duration, PutOptions) (string, error) {
	return "", nil
}

func (f *observabilityFakeStore) StartMultipart(context.Context, ObjectRef, PutOptions) (UploadID, error) {
	return "", nil
}

func (f *observabilityFakeStore) PresignPart(context.Context, ObjectRef, UploadID, int32, time.Duration) (string, error) {
	return "", nil
}

func (f *observabilityFakeStore) CompleteMultipart(context.Context, ObjectRef, UploadID, []Part) (ObjectInfo, error) {
	return ObjectInfo{}, nil
}

func (f *observabilityFakeStore) AbortMultipart(context.Context, ObjectRef, UploadID) error {
	return nil
}

func (f *observabilityFakeStore) Capabilities() Capabilities {
	return Capabilities{}
}

var _ ObjectStore = (*observabilityFakeStore)(nil)

func TestArtifactObservabilityRecordsOperationDurationAndErrorsWithoutProjectOrKeyLabels(t *testing.T) {
	reader := setupTestMeterProvider(t)
	backend := t.Name() // unique per test: disambiguates this test's data points within the shared reader.
	ref, err := NewObjectRef("42", "reports", "q1.csv")
	if err != nil {
		t.Fatalf("NewObjectRef: %v", err)
	}

	store := Instrument(&observabilityFakeStore{
		putInfo: ObjectInfo{Size: 10},
		statErr: ErrNotFound,
	}, backend)

	if _, err := store.Put(context.Background(), ref, strings.NewReader("0123456789"), PutOptions{}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := store.Stat(context.Background(), ref); err == nil {
		t.Fatal("Stat: want error, got nil")
	}

	durationMetric, ok := collectMetric(t, reader, "artifact.storage.operation.duration")
	if !ok {
		t.Fatal("artifact.storage.operation.duration: no data collected")
	}
	hist, ok := durationMetric.Data.(metricdata.Histogram[float64])
	if !ok {
		t.Fatalf("artifact.storage.operation.duration: data is %T, want Histogram[float64]", durationMetric.Data)
	}
	seenOps := map[string]bool{}
	for _, dp := range hist.DataPoints {
		b, _ := dp.Attributes.Value("backend")
		if b.AsString() != backend {
			continue
		}
		if dp.Attributes.HasValue("project_id") || dp.Attributes.HasValue("key") {
			t.Fatalf("operation.duration data point %+v carries a project_id/key attribute, want backend+operation only", dp.Attributes.ToSlice())
		}
		op, _ := dp.Attributes.Value("operation")
		seenOps[op.AsString()] = true
	}
	if !seenOps["put"] || !seenOps["stat"] {
		t.Fatalf("operation.duration: got operations %v, want both put and stat recorded for backend %q", seenOps, backend)
	}

	errorMetric, ok := collectMetric(t, reader, "artifact.storage.operation.errors")
	if !ok {
		t.Fatal("artifact.storage.operation.errors: no data collected")
	}
	errSum, ok := errorMetric.Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("artifact.storage.operation.errors: data is %T, want Sum[int64]", errorMetric.Data)
	}
	var found bool
	for _, dp := range errSum.DataPoints {
		b, _ := dp.Attributes.Value("backend")
		if b.AsString() != backend {
			continue
		}
		if dp.Attributes.HasValue("project_id") || dp.Attributes.HasValue("key") {
			t.Fatalf("operation.errors data point %+v carries a project_id/key attribute, want backend+operation+error_type only", dp.Attributes.ToSlice())
		}
		op, _ := dp.Attributes.Value("operation")
		errType, _ := dp.Attributes.Value("error_type")
		if op.AsString() == "stat" {
			found = true
			if errType.AsString() != "not_found" {
				t.Fatalf("stat error_type = %q, want %q", errType.AsString(), "not_found")
			}
			if dp.Value != 1 {
				t.Fatalf("stat error count = %d, want 1", dp.Value)
			}
		}
		if op.AsString() == "put" {
			t.Fatal("operation.errors: put succeeded but recorded an error data point")
		}
	}
	if !found {
		t.Fatalf("operation.errors: no stat/not_found data point for backend %q", backend)
	}
}

func TestArtifactObservabilityRecordsBytesInAndBytesOut(t *testing.T) {
	reader := setupTestMeterProvider(t)
	backend := t.Name()
	ref, err := NewObjectRef("42", "reports", "q1.csv")
	if err != nil {
		t.Fatalf("NewObjectRef: %v", err)
	}

	store := Instrument(&observabilityFakeStore{
		putInfo: ObjectInfo{Size: 4096},
		getBody: io.NopCloser(strings.NewReader("")),
		getInfo: ObjectInfo{Size: 2048},
	}, backend)

	if _, err := store.Put(context.Background(), ref, strings.NewReader("x"), PutOptions{}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if body, _, err := store.Get(context.Background(), ref, nil); err != nil {
		t.Fatalf("Get: %v", err)
	} else {
		_ = body.Close()
	}

	bytesIn := sumForBackend(t, collectMustFind(t, reader, "artifact.storage.bytes_in"), backend)
	if bytesIn != 4096 {
		t.Fatalf("bytes_in for backend %q = %d, want 4096", backend, bytesIn)
	}
	bytesOut := sumForBackend(t, collectMustFind(t, reader, "artifact.storage.bytes_out"), backend)
	if bytesOut != 2048 {
		t.Fatalf("bytes_out for backend %q = %d, want 2048", backend, bytesOut)
	}
}

func collectMustFind(t *testing.T, reader *sdkmetric.ManualReader, name string) metricdata.Sum[int64] {
	t.Helper()
	m, ok := collectMetric(t, reader, name)
	if !ok {
		t.Fatalf("%s: no data collected", name)
	}
	sum, ok := m.Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("%s: data is %T, want Sum[int64]", name, m.Data)
	}
	return sum
}

func sumForBackend(t *testing.T, sum metricdata.Sum[int64], backend string) int64 {
	t.Helper()
	var total int64
	for _, dp := range sum.DataPoints {
		b, _ := dp.Attributes.Value("backend")
		if b.AsString() == backend {
			total += dp.Value
		}
	}
	return total
}

func TestArtifactObservabilityProjectByteUsageGaugeUpdatesOnlyOnExplicitCallNotOnUnrelatedOperations(t *testing.T) {
	reader := setupTestMeterProvider(t)
	const projectID int64 = 918_273_645 // unique to this test: a gauge holds one latest value per attribute set, so a shared project_id across tests would make assertions order-dependent.

	RecordProjectByteUsage(context.Background(), projectID, 12_345)
	if got := gaugeValueForProject(t, reader, projectID); got != 12_345 {
		t.Fatalf("project_bytes_used after first record = %d, want 12345", got)
	}

	// An unrelated storage operation, on a different backend/project entirely,
	// must not move this project's gauge value.
	unrelatedBackend := t.Name() + "-unrelated"
	unrelatedRef, err := NewObjectRef("7", "scratch", "noise.bin")
	if err != nil {
		t.Fatalf("NewObjectRef: %v", err)
	}
	store := Instrument(&observabilityFakeStore{putInfo: ObjectInfo{Size: 1}}, unrelatedBackend)
	if _, err := store.Put(context.Background(), unrelatedRef, strings.NewReader("x"), PutOptions{}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got := gaugeValueForProject(t, reader, projectID); got != 12_345 {
		t.Fatalf("project_bytes_used after unrelated Put = %d, want unchanged 12345", got)
	}

	// A second explicit call (standing in for the next sweeper tick) does move it.
	RecordProjectByteUsage(context.Background(), projectID, 99_999)
	if got := gaugeValueForProject(t, reader, projectID); got != 99_999 {
		t.Fatalf("project_bytes_used after second record = %d, want 99999", got)
	}
}

func gaugeValueForProject(t *testing.T, reader *sdkmetric.ManualReader, projectID int64) int64 {
	t.Helper()
	m, ok := collectMetric(t, reader, "artifact.storage.project_bytes_used")
	if !ok {
		t.Fatal("artifact.storage.project_bytes_used: no data collected")
	}
	gauge, ok := m.Data.(metricdata.Gauge[int64])
	if !ok {
		t.Fatalf("artifact.storage.project_bytes_used: data is %T, want Gauge[int64]", m.Data)
	}
	for _, dp := range gauge.DataPoints {
		pid, _ := dp.Attributes.Value("project_id")
		if pid.AsInt64() == projectID {
			return dp.Value
		}
	}
	t.Fatalf("artifact.storage.project_bytes_used: no data point for project_id %d", projectID)
	return 0
}

// capturingSlogHandler records every emitted slog.Record so LogAudit's
// output can be asserted on directly, without depending on stdout/stderr
// formatting.
type capturingSlogHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *capturingSlogHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *capturingSlogHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}

func (h *capturingSlogHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *capturingSlogHandler) WithGroup(string) slog.Handler      { return h }

func (h *capturingSlogHandler) attrMap(r slog.Record) map[string]string {
	attrs := make(map[string]string, r.NumAttrs())
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.String()
		return true
	})
	return attrs
}

func TestArtifactObservabilityLogsAuditForDeleteAndGrantIssuance(t *testing.T) {
	handler := &capturingSlogHandler{}
	previous := slog.Default()
	slog.SetDefault(slog.New(handler))
	defer slog.SetDefault(previous)

	authedCtx := auth.ContextWithAuthenticatedUser(context.Background(), auth.User{
		ID:    "user-1",
		Email: "alice@example.com",
	}, auth.AuthenticationSourceSession)
	LogAudit(authedCtx, "delete", "reports", "q1.csv", "42", "success")

	LogAudit(context.Background(), "grant_issued", "exports", "grant-abc123", "43", "success")

	if len(handler.records) != 2 {
		t.Fatalf("got %d audit log records, want 2", len(handler.records))
	}

	deleteAttrs := handler.attrMap(handler.records[0])
	wantDelete := map[string]string{
		"operation":  "delete",
		"bucket":     "reports",
		"key":        "q1.csv",
		"project_id": "42",
		"principal":  "alice@example.com",
		"outcome":    "success",
	}
	for k, want := range wantDelete {
		if got := deleteAttrs[k]; got != want {
			t.Errorf("delete audit record attr %q = %q, want %q", k, got, want)
		}
	}

	grantAttrs := handler.attrMap(handler.records[1])
	wantGrant := map[string]string{
		"operation":  "grant_issued",
		"bucket":     "exports",
		"key":        "grant-abc123",
		"project_id": "43",
		"principal":  "system",
		"outcome":    "success",
	}
	for k, want := range wantGrant {
		if got := grantAttrs[k]; got != want {
			t.Errorf("grant_issued audit record attr %q = %q, want %q", k, got, want)
		}
	}
}

// TestArtifactObservabilityInstrumentedStoreEmitsAuditThroughTheRealWrapper
// closes a real gap an adversarial review found: the test above calls
// LogAudit directly with hand-picked arguments, so it would still pass even
// if instrumentedStore.Delete/DeleteBatch's own LogAudit calls were deleted,
// swapped ref.Bucket()/ref.Key(), or hardcoded outcome to "success" — none
// of that wiring is exercised anywhere else. This test goes through
// Instrument(...) itself.
func TestArtifactObservabilityInstrumentedStoreEmitsAuditThroughTheRealWrapper(t *testing.T) {
	handler := &capturingSlogHandler{}
	previous := slog.Default()
	slog.SetDefault(slog.New(handler))
	defer slog.SetDefault(previous)

	ref, err := NewObjectRef("42", "reports", "q1.csv")
	if err != nil {
		t.Fatalf("NewObjectRef: %v", err)
	}
	store := Instrument(&observabilityFakeStore{deleteErr: ErrNotFound}, t.Name())

	if err := store.Delete(context.Background(), ref); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete err = %v, want ErrNotFound", err)
	}
	if len(handler.records) != 1 {
		t.Fatalf("got %d audit records after Delete, want 1", len(handler.records))
	}
	attrs := handler.attrMap(handler.records[0])
	want := map[string]string{
		"operation":  "delete",
		"bucket":     "reports",
		"key":        "q1.csv",
		"project_id": "42",
		"outcome":    "failure",
	}
	for k, wantV := range want {
		if got := attrs[k]; got != wantV {
			t.Errorf("Delete audit record attr %q = %q, want %q", k, got, wantV)
		}
	}
}

// TestArtifactObservabilityInstrumentedStoreDeleteBatchAuditsEveryRequestedKey
// proves instrumentedStore.DeleteBatch emits an audit line for every
// requested ref — including one the backend never reports in either
// BatchResult.Deleted or Failed, which an adversarial review confirmed is
// reachable (a chunked S3 DeleteBatch call can fail outright partway
// through, leaving some requested keys in neither list) — and that the
// bucket/project_id on each line come from the matching ref in refs itself,
// not a bare-key-keyed lookup that could misattribute a batch spanning
// multiple buckets with a colliding key.
func TestArtifactObservabilityInstrumentedStoreDeleteBatchAuditsEveryRequestedKey(t *testing.T) {
	handler := &capturingSlogHandler{}
	previous := slog.Default()
	slog.SetDefault(slog.New(handler))
	defer slog.SetDefault(previous)

	refDeleted, err := NewObjectRef("42", "reports", "deleted.csv")
	if err != nil {
		t.Fatalf("NewObjectRef: %v", err)
	}
	refOmitted, err := NewObjectRef("43", "archive", "omitted.csv")
	if err != nil {
		t.Fatalf("NewObjectRef: %v", err)
	}
	fake := &observabilityFakeStore{
		// refOmitted is deliberately absent from both Deleted and Failed —
		// simulating the whole batch call erroring outright before it
		// could report a per-key outcome for it.
		deleteBatchResult: BatchResult{Deleted: []string{"deleted.csv"}},
		deleteBatchErr:    errors.New("boom"),
	}
	store := Instrument(fake, t.Name())

	_, _ = store.DeleteBatch(context.Background(), []ObjectRef{refDeleted, refOmitted})

	if len(handler.records) != 2 {
		t.Fatalf("got %d audit records after DeleteBatch, want 2 (one per requested ref)", len(handler.records))
	}
	deletedAttrs := handler.attrMap(handler.records[0])
	if deletedAttrs["bucket"] != "reports" || deletedAttrs["key"] != "deleted.csv" || deletedAttrs["project_id"] != "42" || deletedAttrs["outcome"] != "success" {
		t.Errorf("deleted ref audit record = %+v, want bucket=reports key=deleted.csv project_id=42 outcome=success", deletedAttrs)
	}
	omittedAttrs := handler.attrMap(handler.records[1])
	if omittedAttrs["bucket"] != "archive" || omittedAttrs["key"] != "omitted.csv" || omittedAttrs["project_id"] != "43" || omittedAttrs["outcome"] != "failure" {
		t.Errorf("omitted-from-result ref audit record = %+v, want bucket=archive key=omitted.csv project_id=43 outcome=failure (never silently skipped)", omittedAttrs)
	}
}
