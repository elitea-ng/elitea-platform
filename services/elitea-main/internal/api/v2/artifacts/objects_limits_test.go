package artifacts_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/artifacts"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

// deadlineRecordingResponseWriter satisfies http.ResponseWriter plus the
// optional SetReadDeadline/SetWriteDeadline methods http.ResponseController
// looks for, recording whatever deadline the handler requests. Unlike
// httptest.ResponseRecorder (which implements neither, so
// ResponseController silently no-ops with http.ErrNotSupported), this lets
// a test observe the actual deadline value a handler asked for without
// waiting for it — the plan's own S12 text acknowledges nothing in this
// package's test scope can drive a real multi-minute HTTP round trip, so
// this is the one way to check the requested duration is sane.
type deadlineRecordingResponseWriter struct {
	http.ResponseWriter
	readDeadline  time.Time
	writeDeadline time.Time
}

func (w *deadlineRecordingResponseWriter) SetReadDeadline(t time.Time) error {
	w.readDeadline = t
	return nil
}

func (w *deadlineRecordingResponseWriter) SetWriteDeadline(t time.Time) error {
	w.writeDeadline = t
	return nil
}

// TestArtifactLimitUploadOverMaxObjectBytesReturns413 proves S12's
// http.MaxBytesReader wrapping: a body larger than the project's
// max_object_bytes policy is rejected with a well-formed 413 JSON error —
// not a truncated stream or a connection reset, which is what a bare
// MaxBytesReader failure looks like to a client if the handler doesn't
// catch it.
func TestArtifactLimitUploadOverMaxObjectBytesReturns413(t *testing.T) {
	h, repo, _ := newObjectTestHandler(t)
	small := int64(10)
	repo.setPolicy(repos.ProjectStoragePolicy{ProjectID: 1, MaxObjectBytes: &small})
	r := newObjectTestRouter(h)

	req := newUploadRequest(t, "/objects/1/reports", "big.bin", bytes.Repeat([]byte("A"), 1024))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d; body=%s", rr.Code, http.StatusRequestEntityTooLarge, rr.Body.String())
	}
	var resp struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	decodeJSON(t, rr.Body, &resp)
	if resp.Error.Code != "TooLarge" {
		t.Errorf("expected code TooLarge, got %q (body not well-formed JSON would have failed decodeJSON above)", resp.Error.Code)
	}
}

// TestArtifactLimitUploadOfHundredMiBSucceeds is the acceptance criterion
// "a 100 MiB upload over a 30-second body succeeds" — the size half of it.
// 100 MiB is comfortably under the 150 MiB default (no policy override),
// and well within Go test resource budgets (a single in-memory buffer).
func TestArtifactLimitUploadOfHundredMiBSucceeds(t *testing.T) {
	h, _, _ := newObjectTestHandler(t)
	r := newObjectTestRouter(h)

	const size = 100 << 20 // 100 MiB
	req := newUploadRequest(t, "/objects/1/reports", "large.bin", bytes.Repeat([]byte("A"), size))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body=%s", rr.Code, http.StatusCreated, rr.Body.String())
	}
	var resp struct {
		SizeBytes int64 `json:"size_bytes"`
	}
	decodeJSON(t, rr.Body, &resp)
	if resp.SizeBytes != size {
		t.Errorf("size_bytes = %d, want %d", resp.SizeBytes, size)
	}
}

// TestArtifactLimitUploadSetsGenerousReadDeadline is the timing half of "a
// 100 MiB upload over a 30-second body succeeds": proves the per-request
// read deadline UploadObject sets is comfortably longer than 30 seconds,
// using deadlineRecordingResponseWriter instead of an actual 30-second
// wait.
func TestArtifactLimitUploadSetsGenerousReadDeadline(t *testing.T) {
	h, _, _ := newObjectTestHandler(t)
	r := newObjectTestRouter(h)

	req := newUploadRequest(t, "/objects/1/reports", "small.bin", []byte("hello"))
	rec := &deadlineRecordingResponseWriter{ResponseWriter: httptest.NewRecorder()}
	before := time.Now()
	r.ServeHTTP(rec, req)

	if rec.readDeadline.IsZero() {
		t.Fatal("expected UploadObject to set a read deadline")
	}
	if got := rec.readDeadline.Sub(before); got < 30*time.Second {
		t.Errorf("read deadline %v from request start, want >= 30s", got)
	}
}

// TestArtifactLimitDownloadSetsGenerousWriteDeadline is the handler-level
// half of "a 300-second download completes" (the server-level half is
// cmd/elitea-main/http_server_test.go's
// TestArtifactLimitHTTPServerHasNoBodyLevelTimeouts, which proves the old
// global 120s WriteTimeout is gone entirely). This proves DownloadObject
// itself asks for a deadline comfortably past 300 seconds.
func TestArtifactLimitDownloadSetsGenerousWriteDeadline(t *testing.T) {
	h, _, store := newObjectTestHandler(t)
	store.seed("1", "reports", "hello.txt", 10)
	r := newObjectTestRouter(h)

	req := httptest.NewRequest(http.MethodGet, "/objects/1/reports/hello.txt", nil)
	rec := &deadlineRecordingResponseWriter{ResponseWriter: httptest.NewRecorder()}
	before := time.Now()
	r.ServeHTTP(rec, req)

	if rec.writeDeadline.IsZero() {
		t.Fatal("expected DownloadObject to set a write deadline")
	}
	if got := rec.writeDeadline.Sub(before); got < 300*time.Second {
		t.Errorf("write deadline %v from request start, want >= 300s", got)
	}
}

// TestArtifactLimitUploadExceedingProjectQuotaReturns413AndRollsBack proves
// S12's project-quota enforcement: SumProjectBytes (not SumBucketBytes,
// which is single-bucket and cannot be summed across a project by looping —
// see S6/S12) is checked after each write, and a write that pushes the
// project's total past max_total_bytes is rejected with 413 even though the
// individual object is under max_object_bytes — a materially different
// code path from the per-object cap. The rejected upload is rolled back:
// neither its physical bytes nor its metadata row survive, and the prior,
// successful upload is untouched.
func TestArtifactLimitUploadExceedingProjectQuotaReturns413AndRollsBack(t *testing.T) {
	h, repo, store := newObjectTestHandler(t)
	limit := int64(150)
	repo.setPolicy(repos.ProjectStoragePolicy{ProjectID: 1, MaxTotalBytes: &limit})
	r := newObjectTestRouter(h)

	firstReq := newUploadRequest(t, "/objects/1/reports", "first.bin", bytes.Repeat([]byte("A"), 100))
	firstRR := httptest.NewRecorder()
	r.ServeHTTP(firstRR, firstReq)
	if firstRR.Code != http.StatusCreated {
		t.Fatalf("first upload: status = %d, want %d; body=%s", firstRR.Code, http.StatusCreated, firstRR.Body.String())
	}

	secondReq := newUploadRequest(t, "/objects/1/reports", "second.bin", bytes.Repeat([]byte("B"), 100))
	secondRR := httptest.NewRecorder()
	r.ServeHTTP(secondRR, secondReq)
	if secondRR.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("second upload: status = %d, want %d; body=%s", secondRR.Code, http.StatusRequestEntityTooLarge, secondRR.Body.String())
	}
	var resp struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	decodeJSON(t, secondRR.Body, &resp)
	if resp.Error.Code != "TooLarge" {
		t.Errorf("expected code TooLarge, got %q", resp.Error.Code)
	}

	// Rolled back: second.bin must not exist physically...
	if store.objectCount() != 1 {
		t.Errorf("expected only first.bin to remain in the store, got %d objects", store.objectCount())
	}
	// ...nor count toward the project's metadata total.
	total, err := repo.SumProjectBytes(t.Context(), 1)
	if err != nil {
		t.Fatalf("SumProjectBytes: %v", err)
	}
	if total != 100 {
		t.Errorf("SumProjectBytes = %d, want 100 (only first.bin, second.bin rolled back)", total)
	}

	// first.bin is untouched.
	headReq := httptest.NewRequest(http.MethodHead, "/objects/1/reports/first.bin", nil)
	headRR := httptest.NewRecorder()
	r.ServeHTTP(headRR, headReq)
	if headRR.Code != http.StatusOK {
		t.Errorf("first.bin HEAD: status = %d, want %d — the successful upload must survive the second one's rollback", headRR.Code, http.StatusOK)
	}
}

// TestArtifactLimitUploadWithinQuotaSucceeds is the negative-space check for
// the rollback test above: a write that does NOT push the total past
// max_total_bytes must succeed normally.
func TestArtifactLimitUploadWithinQuotaSucceeds(t *testing.T) {
	h, repo, _ := newObjectTestHandler(t)
	limit := int64(1000)
	repo.setPolicy(repos.ProjectStoragePolicy{ProjectID: 1, MaxTotalBytes: &limit})
	r := newObjectTestRouter(h)

	req := newUploadRequest(t, "/objects/1/reports", "small.bin", bytes.Repeat([]byte("A"), 100))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body=%s", rr.Code, http.StatusCreated, rr.Body.String())
	}
}

// TestArtifactLimitUploadRecordsObjectMetadata closes the gap this stage
// found: before S12, UploadObject never wrote to the elitea_storage.objects
// metadata table at all, which left SumBucketBytes/SumProjectBytes/
// CountBucketObjects permanently zero for every real upload — silently
// breaking both S8's bucket size_bytes/object_count fields and, without a
// fix, this very stage's own quota feature. This proves a successful
// upload is now reflected in the bucket's aggregates.
func TestArtifactLimitUploadRecordsObjectMetadata(t *testing.T) {
	h, repo, _ := newObjectTestHandler(t)
	r := newObjectTestRouter(h)

	req := newUploadRequest(t, "/objects/1/reports", "tracked.bin", bytes.Repeat([]byte("A"), 42))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body=%s", rr.Code, http.StatusCreated, rr.Body.String())
	}

	bucket, err := repo.GetBucket(t.Context(), 1, "reports")
	if err != nil {
		t.Fatalf("GetBucket: %v", err)
	}
	size, err := repo.SumBucketBytes(t.Context(), bucket.ID)
	if err != nil {
		t.Fatalf("SumBucketBytes: %v", err)
	}
	if size != 42 {
		t.Errorf("SumBucketBytes = %d, want 42", size)
	}
	count, err := repo.CountBucketObjects(t.Context(), bucket.ID)
	if err != nil {
		t.Fatalf("CountBucketObjects: %v", err)
	}
	if count != 1 {
		t.Errorf("CountBucketObjects = %d, want 1", count)
	}
}

// TestArtifactRetentionUploadStampsObjectExpiryFromRetentionDays proves S14
// item 1's object-side half: an object uploaded into a bucket with retention
// configured gets a non-nil expires_at.
//
// DEFECT: the upload path copied the BUCKET's expires_at onto every object.
// That value is one absolute instant, computed once when the bucket was
// created (computeExpiresAt = now + retention_days) and never re-derived.
// Every upload made after that instant was therefore stamped with a deadline
// already in the past. The API answered 201 Created, and the retention
// sweeper (cadence */15 * * * *, ListExpiredObjects WHERE expires_at < now)
// deleted the bytes and the metadata row within 15 minutes. No error reached
// the caller. The second case below is the one that was missing: it uses a
// bucket whose own deadline has already passed.
func TestArtifactRetentionUploadStampsObjectExpiryFromRetentionDays(t *testing.T) {
	cases := []struct {
		name            string
		bucketExpiresAt *time.Time
	}{
		{"bucket deadline in the future", timePtr(time.Now().Add(30 * 24 * time.Hour))},
		{"bucket deadline already passed", timePtr(time.Now().Add(-2 * 24 * time.Hour))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newFakeRepo()
			bucket, err := repo.CreateBucket(t.Context(), repos.NewBucketInput{
				ProjectID: 1, Name: "reports", DisplayName: "reports", BucketType: "local",
				RetentionDays: int32Ptr(30),
				ExpiresAt:     tc.bucketExpiresAt,
			})
			if err != nil {
				t.Fatalf("seed CreateBucket: %v", err)
			}
			store := newFakeStore()
			h := artifacts.NewHandler(repo, store)
			r := newObjectTestRouter(h)

			before := time.Now()
			req := newUploadRequest(t, "/objects/1/reports", "expiring.bin", []byte("x"))
			rr := httptest.NewRecorder()
			r.ServeHTTP(rr, req)
			if rr.Code != http.StatusCreated {
				t.Fatalf("status = %d, want %d; body=%s", rr.Code, http.StatusCreated, rr.Body.String())
			}

			record, ok := repo.objects[fakeObjectKey(bucket.ID, "expiring.bin")]
			if !ok {
				t.Fatal("expected UpsertObject to have recorded expiring.bin")
			}
			if record.expiresAt == nil {
				t.Fatal("uploaded object has a nil ExpiresAt, want now + retention_days")
			}
			// The sweeper deletes anything whose expires_at is in the past.
			// A fresh upload must never be born in that set.
			if !record.expiresAt.After(time.Now()) {
				t.Fatalf("object ExpiresAt = %v is not in the future; the sweeper deletes it on the next tick", *record.expiresAt)
			}
			wantLow := before.AddDate(0, 0, 30)
			wantHigh := time.Now().AddDate(0, 0, 30)
			if record.expiresAt.Before(wantLow) || record.expiresAt.After(wantHigh) {
				t.Errorf("object ExpiresAt = %v, want between %v and %v", *record.expiresAt, wantLow, wantHigh)
			}
		})
	}
}

func int32Ptr(v int32) *int32        { return &v }
func timePtr(t time.Time) *time.Time { return &t }

// TestArtifactLimitDeleteRemovesObjectMetadata proves the other half of the
// same gap-fix: deleting an object (single or batch) removes its metadata
// row too, so the aggregates shrink back down rather than only ever
// growing — a project that deletes everything it uploaded must not stay
// permanently near its quota.
func TestArtifactLimitDeleteRemovesObjectMetadata(t *testing.T) {
	h, repo, _ := newObjectTestHandler(t)
	r := newObjectTestRouter(h)

	for _, name := range []string{"a.bin", "b.bin"} {
		req := newUploadRequest(t, "/objects/1/reports", name, bytes.Repeat([]byte("A"), 10))
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusCreated {
			t.Fatalf("upload %s: status = %d, want %d; body=%s", name, rr.Code, http.StatusCreated, rr.Body.String())
		}
	}

	bucket, err := repo.GetBucket(t.Context(), 1, "reports")
	if err != nil {
		t.Fatalf("GetBucket: %v", err)
	}
	if count, _ := repo.CountBucketObjects(t.Context(), bucket.ID); count != 2 {
		t.Fatalf("precondition: CountBucketObjects = %d, want 2", count)
	}

	deleteReq := httptest.NewRequest(http.MethodDelete, "/objects/1/reports/a.bin", nil)
	deleteRR := httptest.NewRecorder()
	r.ServeHTTP(deleteRR, deleteReq)
	if deleteRR.Code != http.StatusNoContent {
		t.Fatalf("DELETE a.bin: status = %d, want %d", deleteRR.Code, http.StatusNoContent)
	}

	batchReq := httptest.NewRequest(http.MethodPost, "/objects/1/reports:batchDelete", bytes.NewBufferString(`{"keys":["b.bin"]}`))
	batchRR := httptest.NewRecorder()
	r.ServeHTTP(batchRR, batchReq)
	if batchRR.Code != http.StatusOK {
		t.Fatalf("batchDelete b.bin: status = %d, want %d; body=%s", batchRR.Code, http.StatusOK, batchRR.Body.String())
	}

	if count, err := repo.CountBucketObjects(t.Context(), bucket.ID); err != nil || count != 0 {
		t.Errorf("CountBucketObjects after deleting both = %d (err=%v), want 0", count, err)
	}
	if size, err := repo.SumBucketBytes(t.Context(), bucket.ID); err != nil || size != 0 {
		t.Errorf("SumBucketBytes after deleting both = %d (err=%v), want 0", size, err)
	}
}
