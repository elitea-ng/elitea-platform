package artifacts_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/artifacts"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

func newTestRouter(h *artifacts.Handler) chi.Router {
	r := chi.NewRouter()
	r.Get("/buckets/{projectID}", h.ListBuckets)
	r.Post("/buckets/{projectID}", h.CreateBucket)
	r.Get("/buckets/{projectID}/{bucket}", h.GetBucket)
	r.Patch("/buckets/{projectID}/{bucket}", h.UpdateBucket)
	r.Delete("/buckets/{projectID}/{bucket}", h.DeleteBucket)
	return r
}

func decodeJSON(t *testing.T, body *bytes.Buffer, v any) {
	t.Helper()
	if err := json.NewDecoder(body).Decode(v); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, body.String())
	}
}

func TestListBuckets_Empty(t *testing.T) {
	h := artifacts.NewHandler(newFakeRepo(), newFakeStore())
	r := newTestRouter(h)

	req := httptest.NewRequest(http.MethodGet, "/buckets/1", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Buckets []artifacts.Bucket `json:"buckets"`
	}
	decodeJSON(t, rr.Body, &resp)
	if resp.Buckets == nil {
		t.Error("expected buckets to be an empty array, got null")
	}
	if len(resp.Buckets) != 0 {
		t.Errorf("expected 0 buckets, got %d", len(resp.Buckets))
	}
}

func TestListBuckets_ReturnsAggregatesForProjectOnly(t *testing.T) {
	repo := newFakeRepo()
	bucket, err := repo.CreateBucket(t.Context(), repos.NewBucketInput{
		ProjectID: 1, Name: "reports", DisplayName: "reports", BucketType: "local",
	})
	if err != nil {
		t.Fatalf("seed CreateBucket: %v", err)
	}
	repo.setAggregate(bucket.ID, 10485760, 42)
	if _, err := repo.CreateBucket(t.Context(), repos.NewBucketInput{
		ProjectID: 2, Name: "other-project", DisplayName: "other-project", BucketType: "local",
	}); err != nil {
		t.Fatalf("seed CreateBucket: %v", err)
	}

	h := artifacts.NewHandler(repo, newFakeStore())
	r := newTestRouter(h)

	req := httptest.NewRequest(http.MethodGet, "/buckets/1", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Buckets []artifacts.Bucket `json:"buckets"`
	}
	decodeJSON(t, rr.Body, &resp)
	if len(resp.Buckets) != 1 {
		t.Fatalf("expected 1 bucket scoped to project 1, got %d", len(resp.Buckets))
	}
	got := resp.Buckets[0]
	if got.Name != "reports" || got.SizeBytes != 10485760 || got.ObjectCount != 42 {
		t.Errorf("unexpected bucket: %+v", got)
	}
}

func TestCreateBucket_ReturnsFullBucketShape(t *testing.T) {
	h := artifacts.NewHandler(newFakeRepo(), newFakeStore())
	r := newTestRouter(h)

	body := bytes.NewBufferString(`{"name":"test-bucket","retention_days":30}`)
	req := httptest.NewRequest(http.MethodPost, "/buckets/1", body)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var raw map[string]any
	decodeJSON(t, bytes.NewBuffer(rr.Body.Bytes()), &raw)
	for _, key := range []string{
		"name", "type", "is_pinned", "tags", "retention_days", "expires_at",
		"size_bytes", "object_count", "created_at", "updated_at",
	} {
		if _, ok := raw[key]; !ok {
			t.Errorf("response missing key %q: %v", key, raw)
		}
	}
	if _, ok := raw["size_bytes"].(float64); !ok {
		t.Errorf("size_bytes is not a JSON number: %T (%v)", raw["size_bytes"], raw["size_bytes"])
	}
	if _, ok := raw["object_count"].(float64); !ok {
		t.Errorf("object_count is not a JSON number: %T (%v)", raw["object_count"], raw["object_count"])
	}

	var b artifacts.Bucket
	decodeJSON(t, bytes.NewBuffer(rr.Body.Bytes()), &b)
	if b.Name != "test-bucket" {
		t.Errorf("expected name %q, got %q", "test-bucket", b.Name)
	}
	if b.RetentionDays == nil || *b.RetentionDays != 30 {
		t.Errorf("expected retention_days 30, got %v", b.RetentionDays)
	}
	if b.ExpiresAt == nil {
		t.Error("expected expires_at to be set given a non-nil retention_days")
	}
	if b.SizeBytes != 0 || b.ObjectCount != 0 {
		t.Errorf("expected a fresh bucket to have 0/0, got %d/%d", b.SizeBytes, b.ObjectCount)
	}
}

func TestCreateBucket_RejectsNameNeedingNormalisation(t *testing.T) {
	h := artifacts.NewHandler(newFakeRepo(), newFakeStore())
	r := newTestRouter(h)

	cases := []string{"Test_Bucket", "UPPERCASE", "has_underscore", "a", "-leadingdash"}
	for _, name := range cases {
		body := bytes.NewBufferString(`{"name":"` + name + `"}`)
		req := httptest.NewRequest(http.MethodPost, "/buckets/1", body)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)

		if rr.Code != http.StatusBadRequest {
			t.Errorf("name %q: expected 400, got %d: %s", name, rr.Code, rr.Body.String())
			continue
		}
		var resp struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		decodeJSON(t, rr.Body, &resp)
		if resp.Error.Code != "InvalidArgument" {
			t.Errorf("name %q: expected code InvalidArgument, got %q", name, resp.Error.Code)
		}
	}
}

func TestCreateBucket_DuplicateReturnsAlreadyExists(t *testing.T) {
	h := artifacts.NewHandler(newFakeRepo(), newFakeStore())
	r := newTestRouter(h)

	body := bytes.NewBufferString(`{"name":"reports"}`)
	req := httptest.NewRequest(http.MethodPost, "/buckets/1", body)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("first create: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	body = bytes.NewBufferString(`{"name":"reports"}`)
	req = httptest.NewRequest(http.MethodPost, "/buckets/1", body)
	rr = httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("duplicate create: expected 409, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	decodeJSON(t, rr.Body, &resp)
	if resp.Error.Code != "AlreadyExists" {
		t.Errorf("expected code AlreadyExists, got %q", resp.Error.Code)
	}
}

func TestCreateBucket_RetentionAboveLimitRejected(t *testing.T) {
	repo := newFakeRepo()
	maxDays := int32(30)
	repo.setPolicy(repos.ProjectStoragePolicy{ProjectID: 1, RetentionMaxDays: &maxDays})
	h := artifacts.NewHandler(repo, newFakeStore())
	r := newTestRouter(h)

	body := bytes.NewBufferString(`{"name":"reports","retention_days":365}`)
	req := httptest.NewRequest(http.MethodPost, "/buckets/1", body)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	decodeJSON(t, rr.Body, &resp)
	if resp.Error.Code != "QuotaExceeded" {
		t.Errorf("expected code QuotaExceeded, got %q", resp.Error.Code)
	}
}

func TestGetBucket_NotFound(t *testing.T) {
	h := artifacts.NewHandler(newFakeRepo(), newFakeStore())
	r := newTestRouter(h)

	req := httptest.NewRequest(http.MethodGet, "/buckets/1/missing", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	decodeJSON(t, rr.Body, &resp)
	if resp.Error.Code != "NotFound" {
		t.Errorf("expected code NotFound, got %q", resp.Error.Code)
	}
}

func TestUpdateBucket_PinTagsAndRetentionTogether(t *testing.T) {
	repo := newFakeRepo()
	if _, err := repo.CreateBucket(t.Context(), repos.NewBucketInput{
		ProjectID: 1, Name: "reports", DisplayName: "reports", BucketType: "local",
	}); err != nil {
		t.Fatalf("seed CreateBucket: %v", err)
	}
	h := artifacts.NewHandler(repo, newFakeStore())
	r := newTestRouter(h)

	body := bytes.NewBufferString(`{"is_pinned":true,"tags":{"team":"platform"},"retention_days":90}`)
	req := httptest.NewRequest(http.MethodPatch, "/buckets/1/reports", body)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var b artifacts.Bucket
	decodeJSON(t, rr.Body, &b)
	if !b.IsPinned {
		t.Error("expected is_pinned true")
	}
	if b.RetentionDays == nil || *b.RetentionDays != 90 {
		t.Errorf("expected retention_days 90, got %v", b.RetentionDays)
	}
	if b.ExpiresAt == nil {
		t.Error("expected expires_at to be set")
	}
	var tags map[string]string
	if err := json.Unmarshal(b.Tags, &tags); err != nil {
		t.Fatalf("decode tags: %v", err)
	}
	if tags["team"] != "platform" {
		t.Errorf("expected tags.team=platform, got %v", tags)
	}
}

func TestUpdateBucket_NullRetentionClearsExpiry(t *testing.T) {
	repo := newFakeRepo()
	created, err := repo.CreateBucket(t.Context(), repos.NewBucketInput{
		ProjectID: 1, Name: "reports", DisplayName: "reports", BucketType: "local",
	})
	if err != nil {
		t.Fatalf("seed CreateBucket: %v", err)
	}
	thirty := int32(30)
	if _, err := repo.UpdateBucketRetention(t.Context(), created.ID, &thirty, nil); err != nil {
		t.Fatalf("seed UpdateBucketRetention: %v", err)
	}

	h := artifacts.NewHandler(repo, newFakeStore())
	r := newTestRouter(h)

	body := bytes.NewBufferString(`{"retention_days":null}`)
	req := httptest.NewRequest(http.MethodPatch, "/buckets/1/reports", body)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var b artifacts.Bucket
	decodeJSON(t, rr.Body, &b)
	if b.RetentionDays != nil {
		t.Errorf("expected retention_days to be cleared, got %v", *b.RetentionDays)
	}
	if b.ExpiresAt != nil {
		t.Errorf("expected expires_at to be cleared, got %v", *b.ExpiresAt)
	}
}

func TestUpdateBucket_RetentionAboveLimitRejected(t *testing.T) {
	repo := newFakeRepo()
	if _, err := repo.CreateBucket(t.Context(), repos.NewBucketInput{
		ProjectID: 1, Name: "reports", DisplayName: "reports", BucketType: "local",
	}); err != nil {
		t.Fatalf("seed CreateBucket: %v", err)
	}
	maxDays := int32(30)
	repo.setPolicy(repos.ProjectStoragePolicy{ProjectID: 1, RetentionMaxDays: &maxDays})

	h := artifacts.NewHandler(repo, newFakeStore())
	r := newTestRouter(h)

	body := bytes.NewBufferString(`{"retention_days":365}`)
	req := httptest.NewRequest(http.MethodPatch, "/buckets/1/reports", body)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	decodeJSON(t, rr.Body, &resp)
	if resp.Error.Code != "QuotaExceeded" {
		t.Errorf("expected code QuotaExceeded, got %q", resp.Error.Code)
	}
}

func TestUpdateBucket_NotFound(t *testing.T) {
	h := artifacts.NewHandler(newFakeRepo(), newFakeStore())
	r := newTestRouter(h)

	body := bytes.NewBufferString(`{"is_pinned":true}`)
	req := httptest.NewRequest(http.MethodPatch, "/buckets/1/missing", body)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestDeleteBucket_DeletesObjectsAndSoftDeletesRow(t *testing.T) {
	repo := newFakeRepo()
	if _, err := repo.CreateBucket(t.Context(), repos.NewBucketInput{
		ProjectID: 1, Name: "reports", DisplayName: "reports", BucketType: "local",
	}); err != nil {
		t.Fatalf("seed CreateBucket: %v", err)
	}
	store := newFakeStore()
	store.seed("1", "reports", "a.png", 10)
	store.seed("1", "reports", "sub/b.png", 20)
	store.seed("1", "other-bucket", "c.png", 30)

	h := artifacts.NewHandler(repo, store)
	r := newTestRouter(h)

	req := httptest.NewRequest(http.MethodDelete, "/buckets/1/reports", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rr.Code, rr.Body.String())
	}
	if rr.Body.Len() != 0 {
		t.Errorf("expected empty body, got %q", rr.Body.String())
	}

	if store.objectCount() != 1 {
		t.Errorf("expected only the other bucket's object to remain, got %d objects", store.objectCount())
	}

	if _, err := repo.GetBucket(t.Context(), 1, "reports"); err == nil {
		t.Error("expected bucket to be soft-deleted (GetBucket should no longer find it)")
	}
}

func TestDeleteBucket_NotFound(t *testing.T) {
	h := artifacts.NewHandler(newFakeRepo(), newFakeStore())
	r := newTestRouter(h)

	req := httptest.NewRequest(http.MethodDelete, "/buckets/1/missing", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestCreateBucket_AppliesProjectRetentionDefaultWhenOmitted covers the
// defect that CreateBucket read only RetentionMaxDays from the project
// storage policy. It ignored RetentionDefaultDays, the field
// migrations/shared/0057_artifact_storage.sql:42-44 defines as "what a
// bucket gets when the caller omits a value". A POST without
// retention_days therefore stored retention_days = NULL and
// expires_at = NULL. Objects written into that bucket also got a NULL
// expires_at. ListExpiredArtifactObjects only selects a row whose
// expires_at is set and past. The operator's mandated retention
// therefore swept nothing.
func TestCreateBucket_AppliesProjectRetentionDefaultWhenOmitted(t *testing.T) {
	repo := newFakeRepo()
	defaultDays := int32(30)
	maxDays := int32(90)
	repo.setPolicy(repos.ProjectStoragePolicy{
		ProjectID: 1, RetentionDefaultDays: &defaultDays, RetentionMaxDays: &maxDays,
	})
	h := artifacts.NewHandler(repo, newFakeStore())
	r := newTestRouter(h)

	body := bytes.NewBufferString(`{"name":"scratch"}`)
	req := httptest.NewRequest(http.MethodPost, "/buckets/1", body)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var b artifacts.Bucket
	decodeJSON(t, rr.Body, &b)
	if b.RetentionDays == nil || *b.RetentionDays != 30 {
		t.Errorf("expected the policy default of 30 days, got %v", b.RetentionDays)
	}
	if b.ExpiresAt == nil {
		t.Error("expected expires_at to be set, because a bucket with a retention must expire")
	}
}

// TestCreateBucket_ClampsRetentionDefaultToPolicyCeiling covers the second
// half of the same defect. An operator default above the operator ceiling
// is a configuration error, not a caller error. The handler must clamp the
// default to RetentionMaxDays. It must not write a bucket retention above
// the project's own ceiling, and it must not answer 403 to a caller who
// asked for nothing.
func TestCreateBucket_ClampsRetentionDefaultToPolicyCeiling(t *testing.T) {
	repo := newFakeRepo()
	defaultDays := int32(365)
	maxDays := int32(90)
	repo.setPolicy(repos.ProjectStoragePolicy{
		ProjectID: 1, RetentionDefaultDays: &defaultDays, RetentionMaxDays: &maxDays,
	})
	h := artifacts.NewHandler(repo, newFakeStore())
	r := newTestRouter(h)

	body := bytes.NewBufferString(`{"name":"scratch"}`)
	req := httptest.NewRequest(http.MethodPost, "/buckets/1", body)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var b artifacts.Bucket
	decodeJSON(t, rr.Body, &b)
	if b.RetentionDays == nil || *b.RetentionDays != 90 {
		t.Errorf("expected the ceiling of 90 days, got %v", b.RetentionDays)
	}
}

// TestCreateBucket_NoPolicyLeavesRetentionUnset locks the default install.
// A project with no storage policy row has no default, so the bucket keeps
// a nil retention and a nil expiry.
func TestCreateBucket_NoPolicyLeavesRetentionUnset(t *testing.T) {
	h := artifacts.NewHandler(newFakeRepo(), newFakeStore())
	r := newTestRouter(h)

	body := bytes.NewBufferString(`{"name":"scratch"}`)
	req := httptest.NewRequest(http.MethodPost, "/buckets/1", body)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var b artifacts.Bucket
	decodeJSON(t, rr.Body, &b)
	if b.RetentionDays != nil {
		t.Errorf("expected no retention without a policy row, got %v", *b.RetentionDays)
	}
	if b.ExpiresAt != nil {
		t.Errorf("expected no expiry without a policy row, got %v", *b.ExpiresAt)
	}
}

// seedBucketWithObjects creates a bucket, its physical objects, and one
// metadata row per object. DeleteBucket's tests need all three, because
// the defect they cover is the drift between the physical bytes and the
// metadata rows.
func seedBucketWithObjects(t *testing.T, repo *fakeRepo, store *fakeStore, name string, keys ...string) repos.BucketRow {
	t.Helper()
	row, err := repo.CreateBucket(t.Context(), repos.NewBucketInput{
		ProjectID: 1, Name: name, DisplayName: name, BucketType: "local",
	})
	if err != nil {
		t.Fatalf("seed CreateBucket: %v", err)
	}
	for _, key := range keys {
		store.seed("1", name, key, 10)
		if _, err := repo.UpsertObject(t.Context(), repos.NewObjectInput{
			BucketID: row.ID, Key: key, ByteLength: 10, MediaType: "image/png",
		}); err != nil {
			t.Fatalf("seed UpsertObject(%s): %v", key, err)
		}
	}
	return row
}

// TestDeleteBucket_PartialDeleteFailureCleansMetadataOfDestroyedObjects
// covers the defect in deleteAllObjects. On a partial DeleteBatch failure
// the function returned at once and discarded result.Deleted. It
// therefore never removed the metadata rows of the objects whose bytes
// it had already destroyed. DeleteBucket then answered 500 and skipped
// SoftDeleteBucket, which left the bucket active. Every aggregate joins buckets on
// deleted_at IS NULL, so the still-active bucket kept reporting the
// destroyed bytes, and the project quota stayed inflated forever. A retry
// could not repair it, because ObjectStore.List no longer returns a
// destroyed key.
func TestDeleteBucket_PartialDeleteFailureCleansMetadataOfDestroyedObjects(t *testing.T) {
	repo := newFakeRepo()
	store := newFakeStore()
	row := seedBucketWithObjects(t, repo, store, "reports", "ok.png", "stuck.png")
	store.failKeys = map[string]bool{"stuck.png": true}

	h := artifacts.NewHandler(repo, store)
	r := newTestRouter(h)

	req := httptest.NewRequest(http.MethodDelete, "/buckets/1/reports", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 from the refused delete, got %d: %s", rr.Code, rr.Body.String())
	}

	if store.objectCount() != 1 {
		t.Errorf("expected only stuck.png to remain physically, got %d objects", store.objectCount())
	}
	keys := repo.objectKeys(row.ID)
	if len(keys) != 1 || keys[0] != "stuck.png" {
		t.Fatalf("expected only stuck.png to keep a metadata row, got %v", keys)
	}

	// The bucket stays active, so an operator can retry. Its reported size
	// must count the surviving object only.
	if _, err := repo.GetBucket(t.Context(), 1, "reports"); err != nil {
		t.Fatalf("expected the bucket to stay active after a partial failure, got %v", err)
	}
	size, err := repo.SumBucketBytes(t.Context(), row.ID)
	if err != nil {
		t.Fatalf("SumBucketBytes: %v", err)
	}
	if size != 10 {
		t.Errorf("expected the bucket to report the surviving 10 bytes, got %d", size)
	}
}

// TestDeleteBucket_RemovesObjectMetadataOnSuccess is the success half of
// the same defect: a complete purge must leave no metadata row behind.
func TestDeleteBucket_RemovesObjectMetadataOnSuccess(t *testing.T) {
	repo := newFakeRepo()
	store := newFakeStore()
	row := seedBucketWithObjects(t, repo, store, "reports", "a.png", "sub/b.png")

	h := artifacts.NewHandler(repo, store)
	r := newTestRouter(h)

	req := httptest.NewRequest(http.MethodDelete, "/buckets/1/reports", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rr.Code, rr.Body.String())
	}
	if keys := repo.objectKeys(row.ID); len(keys) != 0 {
		t.Errorf("expected no metadata rows after a full purge, got %v", keys)
	}
}
