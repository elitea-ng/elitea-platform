package artifacts_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/artifacts"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
)

func newMultipartTestRouter(h *artifacts.Handler) chi.Router {
	r := chi.NewRouter()
	r.Post("/grants/{projectID}/{bucket}", h.CreateTransferGrant)
	r.Post("/grants/{projectID}/{grantID}:commit", h.CommitTransferGrant)
	r.Post("/grants/{projectID}/{grantID}/parts/{partNumber}", h.PresignUploadPart)
	r.Post("/grants/{projectID}/{grantID}:completeMultipart", h.CompleteMultipartUpload)
	r.Post("/grants/{projectID}/{grantID}:abortMultipart", h.AbortMultipartUpload)
	return r
}

// startMultipartGrant issues a native-multipart PUT grant (max_bytes above
// the 64 MiB threshold, on a store with multipart=true) and returns its
// grant_id and upload_id.
func startMultipartGrant(t *testing.T, r chi.Router, projectID int64, contentType string, extra string) (grantID, uploadID string) {
	t.Helper()
	body := fmt.Sprintf(`{"method":"PUT","content_type":%q,"max_bytes":104857601%s}`, contentType, extra)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, newGrantRequest(t, fmt.Sprintf("/grants/%d/reports", projectID), body))
	if rr.Code != http.StatusOK {
		t.Fatalf("start multipart grant: status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	resp := decodeGrantResponse(t, rr.Body)
	if resp.UploadID == nil {
		t.Fatalf("expected upload_id to be set, got response %+v", resp)
	}
	if resp.URL != "" {
		t.Fatalf("expected url to be empty for a multipart grant, got %q", resp.URL)
	}
	return resp.GrantID, *resp.UploadID
}

func presignPart(t *testing.T, r chi.Router, projectID int64, grantID string, partNumber int) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	path := fmt.Sprintf("/grants/%d/%s/parts/%d", projectID, grantID, partNumber)
	r.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, path, nil))
	return rr
}

func completeMultipart(t *testing.T, r chi.Router, projectID int64, grantID string, parts []map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]any{"parts": parts})
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	path := fmt.Sprintf("/grants/%d/%s:completeMultipart", projectID, grantID)
	r.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(body))))
	return rr
}

func abortMultipart(t *testing.T, r chi.Router, projectID int64, grantID string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	path := fmt.Sprintf("/grants/%d/%s:abortMultipart", projectID, grantID)
	r.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, path, nil))
	return rr
}

// errorCode decodes the typed error envelope's code field.
func errorCode(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode error envelope: %v (body=%s)", err, rr.Body.String())
	}
	return envelope.Error.Code
}

// TestArtifactMultipartTwoPartUploadCompletesWithMatchingDigest is the
// plan's first required scenario: "a 2-part 10 MiB upload ... completes
// with a matching digest." S3 and Azure share identical ObjectStore-level
// multipart semantics (both report NativeMultipart: true; the difference
// between them is entirely inside the backend implementations already
// conformance-tested against real emulators in S3/S4) — this test exercises
// the one handler-level code path both providers go through.
func TestArtifactMultipartTwoPartUploadCompletesWithMatchingDigest(t *testing.T) {
	h, repo, store := newObjectTestHandler(t)
	store.multipart = true
	r := newMultipartTestRouter(h)

	part1 := strings.Repeat("A", 5<<20) // 5 MiB
	part2 := strings.Repeat("B", 5<<20) // 5 MiB
	full := part1 + part2
	digest := sha256.Sum256([]byte(full))

	grantID, uploadID := startMultipartGrant(t, r, 1, "application/octet-stream",
		fmt.Sprintf(`,"digest_alg":"sha256","digest":%q`, hex.EncodeToString(digest[:])))

	etag1 := store.simulateMultipartPartUpload(storage.UploadID(uploadID), 1, []byte(part1))
	etag2 := store.simulateMultipartPartUpload(storage.UploadID(uploadID), 2, []byte(part2))

	// Parts deliberately reported out of numeric order. Note this proves
	// end-to-end assembly is correct through the handler, not specifically
	// that the HANDLER (as opposed to the store layer) reorders — both
	// fakeStore.CompleteMultipart and the real S3 backend already re-sort
	// by Number internally (matching S3's own contract), so the ordering
	// guarantee here ultimately comes from the store, not this handler.
	// What this test does verify at the handler level: PartNumber/ETag
	// pairs survive the request -> []storage.Part conversion unmangled and
	// undeduplicated, regardless of what order they arrived in.
	rr := completeMultipart(t, r, 1, grantID, []map[string]any{
		{"part_number": 2, "etag": etag2},
		{"part_number": 1, "etag": etag1},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("complete: status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Key       string `json:"key"`
		SizeBytes int64  `json:"size_bytes"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Key != grantID || resp.SizeBytes != int64(len(full)) {
		t.Fatalf("resp = %+v, want key=%q size_bytes=%d", resp, grantID, len(full))
	}

	bucket, err := repo.GetBucket(t.Context(), 1, "reports")
	if err != nil {
		t.Fatal(err)
	}
	if record, ok := repo.objects[fakeObjectKey(bucket.ID, grantID)]; !ok || record.byteLength != int64(len(full)) {
		t.Fatalf("expected object metadata for %q to be recorded, got ok=%v record=%+v", grantID, ok, record)
	}

	// Single-use: a second complete on the same grant must fail.
	rr2 := completeMultipart(t, r, 1, grantID, []map[string]any{{"part_number": 1, "etag": etag1}})
	if rr2.Code != http.StatusConflict {
		t.Fatalf("second complete: status = %d, want 409; body=%s", rr2.Code, rr2.Body.String())
	}
}

// TestArtifactMultipartFallsBackToFacadeWhenNativeMultipartUnsupported is
// the plan's second required scenario: "the same request against GCS
// transparently uses the facade." GCS's Capabilities().NativeMultipart is
// always false (S3/backend.go) — the fake models that directly rather than
// standing up a GCS-specific double, since the handler code path only ever
// branches on Capabilities(), never on which backend it is.
func TestArtifactMultipartFallsBackToFacadeWhenNativeMultipartUnsupported(t *testing.T) {
	h, _, store := newObjectTestHandler(t)
	store.multipart = false // GCS
	r := newMultipartTestRouter(h)

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, newGrantRequest(t, "/grants/1/reports", `{"method":"PUT","content_type":"application/octet-stream","max_bytes":104857601}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	resp := decodeGrantResponse(t, rr.Body)
	if resp.UploadID != nil {
		t.Fatalf("expected no upload_id when NativeMultipart is false, got %q", *resp.UploadID)
	}
	if resp.URL != "/api/v2/artifacts/objects/1/reports" {
		t.Fatalf("url = %q, want the facade upload endpoint (S15's existing fallback, unchanged)", resp.URL)
	}
}

// TestArtifactMultipartBelowThresholdStaysSingleShot proves the "objects
// above 64 MiB" gate: a PUT request at or under the threshold never starts
// a multipart upload, even on a backend that supports it — S16 is not a
// benefit for small objects and every part/complete round trip has a real
// database cost this package would rather not pay for a small upload.
func TestArtifactMultipartBelowThresholdStaysSingleShot(t *testing.T) {
	h, _, store := newObjectTestHandler(t)
	store.multipart = true
	r := newMultipartTestRouter(h)

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, newGrantRequest(t, "/grants/1/reports", `{"method":"PUT","content_type":"application/octet-stream","max_bytes":1024}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	resp := decodeGrantResponse(t, rr.Body)
	if resp.UploadID != nil {
		t.Fatalf("expected no upload_id for a 1024-byte request even with NativeMultipart true, got %q", *resp.UploadID)
	}
}

// TestArtifactMultipartExactlyAtThresholdStaysSingleShot proves the gate is
// strictly-greater-than, matching the plan's "above 64 MiB" wording: a
// request for exactly 64 MiB (67108864 bytes) must stay single-shot, not
// start a multipart upload — the boundary value itself, not just numbers
// safely on either side of it, which is what would actually regress if `>`
// were ever changed to `>=` (or vice versa).
func TestArtifactMultipartExactlyAtThresholdStaysSingleShot(t *testing.T) {
	h, _, store := newObjectTestHandler(t)
	store.multipart = true
	r := newMultipartTestRouter(h)

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, newGrantRequest(t, "/grants/1/reports", `{"method":"PUT","content_type":"application/octet-stream","max_bytes":67108864}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	resp := decodeGrantResponse(t, rr.Body)
	if resp.UploadID != nil {
		t.Fatalf("expected no upload_id at exactly the 64 MiB threshold, got %q", *resp.UploadID)
	}
}

// TestArtifactMultipartPartCallWithAnotherProjectsGrantReturns403 is the
// plan's third required scenario, verbatim: "a part call with another
// project's grant returns 403." This is deliberately 403 (AccessDenied),
// not the 404 CommitTransferGrant's own project-scoped lookup returns for
// the same situation — see requireOwnedMultipartGrant's doc comment for
// why S16 needs to distinguish "wrong project" from "doesn't exist" here.
func TestArtifactMultipartPartCallWithAnotherProjectsGrantReturns403(t *testing.T) {
	h, repo, store := newObjectTestHandler(t)
	store.multipart = true
	r := newMultipartTestRouter(h)
	if _, err := repo.CreateBucket(t.Context(), repos.NewBucketInput{
		ProjectID: 2, Name: "reports", DisplayName: "reports", BucketType: "local",
	}); err != nil {
		t.Fatalf("seed project 2 bucket: %v", err)
	}

	grantID, _ := startMultipartGrant(t, r, 1, "application/octet-stream", "")

	rr := presignPart(t, r, 2, grantID, 1)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
	if code := errorCode(t, rr); code != "AccessDenied" {
		t.Fatalf("error.code = %q, want AccessDenied", code)
	}
}

// TestArtifactMultipartCompleteCallWithAnotherProjectsGrantReturns403
// proves the plan's "on every part and completion call" language applies
// literally — not just to PresignUploadPart.
func TestArtifactMultipartCompleteCallWithAnotherProjectsGrantReturns403(t *testing.T) {
	h, repo, store := newObjectTestHandler(t)
	store.multipart = true
	r := newMultipartTestRouter(h)
	if _, err := repo.CreateBucket(t.Context(), repos.NewBucketInput{
		ProjectID: 2, Name: "reports", DisplayName: "reports", BucketType: "local",
	}); err != nil {
		t.Fatalf("seed project 2 bucket: %v", err)
	}

	grantID, _ := startMultipartGrant(t, r, 1, "application/octet-stream", "")

	rr := completeMultipart(t, r, 2, grantID, []map[string]any{{"part_number": 1, "etag": "whatever"}})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
	if code := errorCode(t, rr); code != "AccessDenied" {
		t.Fatalf("error.code = %q, want AccessDenied", code)
	}
}

// TestArtifactMultipartAbortCallWithAnotherProjectsGrantReturns403 rounds
// out ownership coverage across all three continuation endpoints, not just
// the two the plan names explicitly — abort is exactly as security-relevant
// (an attacker could otherwise cancel a legitimate in-progress upload).
func TestArtifactMultipartAbortCallWithAnotherProjectsGrantReturns403(t *testing.T) {
	h, repo, store := newObjectTestHandler(t)
	store.multipart = true
	r := newMultipartTestRouter(h)
	if _, err := repo.CreateBucket(t.Context(), repos.NewBucketInput{
		ProjectID: 2, Name: "reports", DisplayName: "reports", BucketType: "local",
	}); err != nil {
		t.Fatalf("seed project 2 bucket: %v", err)
	}

	grantID, _ := startMultipartGrant(t, r, 1, "application/octet-stream", "")

	rr := abortMultipart(t, r, 2, grantID)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
	if code := errorCode(t, rr); code != "AccessDenied" {
		t.Fatalf("error.code = %q, want AccessDenied", code)
	}
}

func TestArtifactMultipartAbortDiscardsUploadAndGrantBecomesUnusable(t *testing.T) {
	h, _, store := newObjectTestHandler(t)
	store.multipart = true
	r := newMultipartTestRouter(h)

	grantID, uploadID := startMultipartGrant(t, r, 1, "application/octet-stream", "")
	store.simulateMultipartPartUpload(storage.UploadID(uploadID), 1, []byte("partial upload"))

	rr := abortMultipart(t, r, 1, grantID)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("abort: status = %d, want 204; body=%s", rr.Code, rr.Body.String())
	}

	// The grant is now terminal: neither a further part call...
	partRR := presignPart(t, r, 1, grantID, 2)
	if partRR.Code != http.StatusConflict {
		t.Fatalf("part after abort: status = %d, want 409; body=%s", partRR.Code, partRR.Body.String())
	}
	// ...nor completion should succeed.
	completeRR := completeMultipart(t, r, 1, grantID, []map[string]any{{"part_number": 1, "etag": "x"}})
	if completeRR.Code != http.StatusConflict {
		t.Fatalf("complete after abort: status = %d, want 409; body=%s", completeRR.Code, completeRR.Body.String())
	}
	// A second sequential abort 409s, same single-use semantics as a second
	// commit (grants_test.go) — requireOwnedMultipartGrant's own
	// already-consumed check rejects it before the store is ever touched
	// again. (AbortMultipartUpload's ErrAlreadyExists handling in
	// MarkTransferGrantConsumed exists for a genuine concurrent race
	// between two abort calls that both pass that check before either
	// writes — not reachable from two sequential calls like this.)
	secondAbortRR := abortMultipart(t, r, 1, grantID)
	if secondAbortRR.Code != http.StatusConflict {
		t.Fatalf("second abort: status = %d, want 409; body=%s", secondAbortRR.Code, secondAbortRR.Body.String())
	}
}

// TestArtifactMultipartConcurrentCompleteAndAbortAreMutuallyExclusive is a
// regression test for a blocking bug an adversarial-review pass found
// before this stage was committed: the first-draft CompleteMultipartUpload
// and AbortMultipartUpload each claimed the grant (MarkTransferGrantConsumed)
// only AFTER calling their own store method, not before — so two concurrent
// same-project requests (a client retry racing a cancel, for example) could
// both observe consumed_at == nil and then race on which backend call
// landed first. If abort won that race, it could delete the very parts
// complete was in the middle of assembling, silently and permanently
// discarding an otherwise fully, legitimately uploaded transfer.
//
// A real goroutine-based race is inherently timing-dependent and not
// reliably reproducible in a unit test. This instead uses fakeStore's
// beforeCompleteMultipart/beforeAbortMultipart hooks to synchronously make
// a *second* HTTP request land exactly inside the window a genuine race
// would occupy — after the first request's own ownership checks have
// already passed, but from inside its own store call. Under the fix (claim
// before store call), the first request has already claimed the grant by
// the time the hook fires, so the nested "concurrent" request is cleanly
// rejected with 409 and never reaches the store itself — which is exactly
// what this test asserts. Run this same test against the pre-fix ordering
// (claim after store call) and it fails: the nested request passes its own
// ownership check, reaches the store, and corrupts the outer call's
// in-flight state — precisely the bug this regression test exists to catch.
func TestArtifactMultipartConcurrentCompleteAndAbortAreMutuallyExclusive(t *testing.T) {
	t.Run("complete wins, concurrent abort is rejected", func(t *testing.T) {
		h, _, store := newObjectTestHandler(t)
		store.multipart = true
		r := newMultipartTestRouter(h)

		grantID, uploadID := startMultipartGrant(t, r, 1, "application/octet-stream", "")
		etag := store.simulateMultipartPartUpload(storage.UploadID(uploadID), 1, []byte("payload"))

		var nestedAbortRR *httptest.ResponseRecorder
		store.beforeCompleteMultipart = func() {
			nestedAbortRR = abortMultipart(t, r, 1, grantID)
		}

		rr := completeMultipart(t, r, 1, grantID, []map[string]any{{"part_number": 1, "etag": etag}})
		if rr.Code != http.StatusOK {
			t.Fatalf("complete: status = %d, want 200; body=%s", rr.Code, rr.Body.String())
		}
		if nestedAbortRR == nil {
			t.Fatal("beforeCompleteMultipart hook never ran")
		}
		if nestedAbortRR.Code != http.StatusConflict {
			t.Fatalf("concurrent abort: status = %d, want 409 (must lose the race, not silently delete parts complete is assembling); body=%s",
				nestedAbortRR.Code, nestedAbortRR.Body.String())
		}
		// The completion itself must have genuinely succeeded, not just
		// returned 200 over corrupted state.
		if store.objectCount() != 1 {
			t.Fatalf("expected exactly 1 completed object to survive, got %d", store.objectCount())
		}
	})

	t.Run("abort wins, concurrent complete is rejected", func(t *testing.T) {
		h, _, store := newObjectTestHandler(t)
		store.multipart = true
		r := newMultipartTestRouter(h)

		grantID, uploadID := startMultipartGrant(t, r, 1, "application/octet-stream", "")
		etag := store.simulateMultipartPartUpload(storage.UploadID(uploadID), 1, []byte("payload"))

		var nestedCompleteRR *httptest.ResponseRecorder
		store.beforeAbortMultipart = func() {
			nestedCompleteRR = completeMultipart(t, r, 1, grantID, []map[string]any{{"part_number": 1, "etag": etag}})
		}

		rr := abortMultipart(t, r, 1, grantID)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("abort: status = %d, want 204; body=%s", rr.Code, rr.Body.String())
		}
		if nestedCompleteRR == nil {
			t.Fatal("beforeAbortMultipart hook never ran")
		}
		if nestedCompleteRR.Code != http.StatusConflict {
			t.Fatalf("concurrent complete: status = %d, want 409 (must lose the race, not report a fake success over an aborted upload); body=%s",
				nestedCompleteRR.Code, nestedCompleteRR.Body.String())
		}
		// The abort itself must have genuinely won — no object exists.
		if store.objectCount() != 0 {
			t.Fatalf("expected no completed object after abort won the race, got %d", store.objectCount())
		}
	})
}

// TestArtifactMultipartPartAfterGrantExpiryReturns412AndAbortStillSucceeds
// proves the deliberate asymmetry: continuing an upload (part presign,
// complete) is blocked once the grant's own TTL has passed, but cleaning
// one up (abort) is not — see AbortMultipartUpload's own doc comment.
func TestArtifactMultipartPartAfterGrantExpiryReturns412AndAbortStillSucceeds(t *testing.T) {
	h, repo, store := newObjectTestHandler(t)
	store.multipart = true
	r := newMultipartTestRouter(h)

	grantID, _ := startMultipartGrant(t, r, 1, "application/octet-stream", "")

	grant := repo.grants[grantID]
	grant.ExpiresAt = grant.ExpiresAt.Add(-time.Hour)
	repo.grants[grantID] = grant

	partRR := presignPart(t, r, 1, grantID, 1)
	if partRR.Code != http.StatusPreconditionFailed {
		t.Fatalf("part after expiry: status = %d, want 412; body=%s", partRR.Code, partRR.Body.String())
	}

	abortRR := abortMultipart(t, r, 1, grantID)
	if abortRR.Code != http.StatusNoContent {
		t.Fatalf("abort after expiry: status = %d, want 204; body=%s", abortRR.Code, abortRR.Body.String())
	}
}

func TestArtifactMultipartCompleteWithMismatchedDigestReturns409AndDeletesObject(t *testing.T) {
	h, _, store := newObjectTestHandler(t)
	store.multipart = true
	r := newMultipartTestRouter(h)

	declaredDigest := sha256.Sum256([]byte("expected content"))
	grantID, uploadID := startMultipartGrant(t, r, 1, "application/octet-stream",
		fmt.Sprintf(`,"digest_alg":"sha256","digest":%q`, hex.EncodeToString(declaredDigest[:])))
	etag := store.simulateMultipartPartUpload(storage.UploadID(uploadID), 1, []byte("different content actually uploaded"))

	rr := completeMultipart(t, r, 1, grantID, []map[string]any{{"part_number": 1, "etag": etag}})
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rr.Code, rr.Body.String())
	}
	if code := errorCode(t, rr); code != "DigestMismatch" {
		t.Fatalf("error.code = %q, want DigestMismatch", code)
	}
	if store.objectCount() != 0 {
		t.Fatalf("expected the mismatched object to be deleted, got %d objects", store.objectCount())
	}
}

// TestArtifactMultipartCompleteExceedingProjectQuotaReturns413AndRollsBack
// proves finalizeGrantCommit's S12 quota enforcement (already exhaustively
// tested for the single-shot path in grants_test.go) also threads through
// the multipart completion call site added by this refactor.
func TestArtifactMultipartCompleteExceedingProjectQuotaReturns413AndRollsBack(t *testing.T) {
	h, repo, store := newObjectTestHandler(t)
	store.multipart = true
	limit := int64(100)
	repo.setPolicy(repos.ProjectStoragePolicy{ProjectID: 1, MaxTotalBytes: &limit})
	r := newMultipartTestRouter(h)

	grantID, uploadID := startMultipartGrant(t, r, 1, "application/octet-stream", "")
	etag := store.simulateMultipartPartUpload(storage.UploadID(uploadID), 1, []byte(strings.Repeat("A", 200)))

	rr := completeMultipart(t, r, 1, grantID, []map[string]any{{"part_number": 1, "etag": etag}})
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body=%s", rr.Code, rr.Body.String())
	}
	if code := errorCode(t, rr); code != "TooLarge" {
		t.Fatalf("error.code = %q, want TooLarge", code)
	}
	if store.objectCount() != 0 {
		t.Fatalf("expected the rolled-back object to be deleted, got %d objects", store.objectCount())
	}
}

func TestArtifactMultipartPartRejectsOutOfRangePartNumber(t *testing.T) {
	h, _, store := newObjectTestHandler(t)
	store.multipart = true
	r := newMultipartTestRouter(h)

	grantID, _ := startMultipartGrant(t, r, 1, "application/octet-stream", "")

	for _, n := range []int{0, -1, 10001} {
		rr := presignPart(t, r, 1, grantID, n)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("part number %d: status = %d, want 400; body=%s", n, rr.Code, rr.Body.String())
		}
	}
}

func TestArtifactMultipartCompleteRejectsEmptyParts(t *testing.T) {
	h, _, store := newObjectTestHandler(t)
	store.multipart = true
	r := newMultipartTestRouter(h)

	grantID, _ := startMultipartGrant(t, r, 1, "application/octet-stream", "")

	rr := completeMultipart(t, r, 1, grantID, []map[string]any{})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

// TestArtifactMultipartCompleteWithWrongETagIsRejectedByStore proves an
// attacker-controlled ETag in the completion request body — the first
// place in this codebase an externally-supplied ETag string reaches
// storage.ObjectStore.CompleteMultipart, rather than one this package
// generated itself — cannot be used to make the store assemble the wrong
// bytes for a part. This is enforced by the backend (real S3/Azure reject
// a mismatched ETag with their own InvalidPart-style error), modeled here
// by fakeStore.CompleteMultipart's own ETag check; CompleteMultipartUpload
// itself does no ETag validation of its own and relies entirely on the
// store to catch this, matching the plan's general design that
// storage.ObjectStore implementations own backend-specific correctness.
func TestArtifactMultipartCompleteWithWrongETagIsRejectedByStore(t *testing.T) {
	h, _, store := newObjectTestHandler(t)
	store.multipart = true
	r := newMultipartTestRouter(h)

	grantID, uploadID := startMultipartGrant(t, r, 1, "application/octet-stream", "")
	store.simulateMultipartPartUpload(storage.UploadID(uploadID), 1, []byte("payload"))

	rr := completeMultipart(t, r, 1, grantID, []map[string]any{{"part_number": 1, "etag": "not-the-real-etag"}})
	if rr.Code != http.StatusPreconditionFailed {
		t.Fatalf("status = %d, want 412; body=%s", rr.Code, rr.Body.String())
	}
	if store.objectCount() != 0 {
		t.Fatalf("expected no object to be assembled from a wrong-etag completion, got %d", store.objectCount())
	}
}

// TestArtifactMultipartPartAgainstNonMultipartGrantReturns400 proves a
// grant that never went through the multipart-start branch of
// CreateTransferGrant (a plain single-shot grant, or one issued while
// NativeMultipart was false) cannot be repurposed for the parts endpoint —
// requireOwnedMultipartGrant checks UploadID != nil, not just that the
// grant exists.
func TestArtifactMultipartPartAgainstNonMultipartGrantReturns400(t *testing.T) {
	h, _, _ := newObjectTestHandler(t)
	r := newMultipartTestRouter(h)

	grantID := createTestGrant(t, r, `{"method":"PUT","content_type":"image/png","max_bytes":1024}`)

	rr := presignPart(t, r, 1, grantID, 1)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	if code := errorCode(t, rr); code != "InvalidArgument" {
		t.Fatalf("error.code = %q, want InvalidArgument", code)
	}
}

func TestArtifactMultipartPartUnknownGrantReturns404(t *testing.T) {
	h, _, store := newObjectTestHandler(t)
	store.multipart = true
	r := newMultipartTestRouter(h)
	rr := presignPart(t, r, 1, "00000000-0000-4000-8000-000000000000", 1)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
}

func TestArtifactMultipartPartMalformedGrantIDReturns404(t *testing.T) {
	h, _, _ := newObjectTestHandler(t)
	r := newMultipartTestRouter(h)
	rr := presignPart(t, r, 1, "not-a-uuid", 1)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
}
