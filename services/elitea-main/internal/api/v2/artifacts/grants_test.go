package artifacts_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/artifacts"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
)

func newGrantTestRouter(h *artifacts.Handler) chi.Router {
	r := chi.NewRouter()
	r.Post("/grants/{projectID}/{bucket}", h.CreateTransferGrant)
	r.Post("/grants/{projectID}/{grantID}:commit", h.CommitTransferGrant)
	return r
}

func newGrantRequest(t *testing.T, path, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func decodeGrantResponse(t *testing.T, body *bytes.Buffer) struct {
	GrantID     string    `json:"grant_id"`
	URL         string    `json:"url"`
	Method      string    `json:"method"`
	ExpiresAt   time.Time `json:"expires_at"`
	ContentType string    `json:"content_type"`
	MaxBytes    int64     `json:"max_bytes"`
	UploadID    *string   `json:"upload_id"`
} {
	t.Helper()
	var resp struct {
		GrantID     string    `json:"grant_id"`
		URL         string    `json:"url"`
		Method      string    `json:"method"`
		ExpiresAt   time.Time `json:"expires_at"`
		ContentType string    `json:"content_type"`
		MaxBytes    int64     `json:"max_bytes"`
		UploadID    *string   `json:"upload_id"`
	}
	if err := json.Unmarshal(body.Bytes(), &resp); err != nil {
		t.Fatalf("decode grant response: %v (body=%s)", err, body.String())
	}
	return resp
}

func TestArtifactGrantCreatePresignedPutReturnsRealPresignedURL(t *testing.T) {
	h, _, store := newObjectTestHandler(t)
	store.presign = true
	r := newGrantTestRouter(h)

	before := time.Now()
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, newGrantRequest(t, "/grants/1/reports", `{"method":"PUT","content_type":"image/png","max_bytes":1024}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	resp := decodeGrantResponse(t, rr.Body)
	if resp.GrantID == "" {
		t.Fatal("expected a non-empty grant_id")
	}
	if !strings.HasPrefix(resp.URL, "https://presigned.example.test/put/") {
		t.Fatalf("url = %q, want a real presigned PUT URL", resp.URL)
	}
	// The URL addresses the grant's STAGING key, never grant_id itself: the
	// committed object lives at grant_id, out of the URL's reach.
	if !strings.HasSuffix(resp.URL, "/"+resp.GrantID+".upload") {
		t.Fatalf("url = %q, want it to end with the staging key %q", resp.URL, resp.GrantID+".upload")
	}
	if resp.Method != "PUT" || resp.ContentType != "image/png" || resp.MaxBytes != 1024 {
		t.Fatalf("resp = %+v", resp)
	}
	if resp.ExpiresAt.Before(before.Add(14*time.Minute)) || resp.ExpiresAt.After(before.Add(16*time.Minute)) {
		t.Fatalf("expires_at = %v, want ~15 minutes from %v", resp.ExpiresAt, before)
	}
}

// TestArtifactGrantCreateFallsBackToFacadeURLWhenPresignUnsupported proves
// S15's explicit fallback requirement: "return the facade URL, not an
// error" when Capabilities().Presign is false — the default for fakeStore,
// matching an Azure-workload-identity or signing-material-less GCS client.
func TestArtifactGrantCreateFallsBackToFacadeURLWhenPresignUnsupported(t *testing.T) {
	h, _, _ := newObjectTestHandler(t)
	r := newGrantTestRouter(h)

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, newGrantRequest(t, "/grants/1/reports", `{"method":"PUT","content_type":"image/png","max_bytes":1024}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	resp := decodeGrantResponse(t, rr.Body)
	if resp.URL != "/api/v2/artifacts/objects/1/reports" {
		t.Fatalf("url = %q, want the facade upload endpoint", resp.URL)
	}
}

// A download grant could never work, so it is refused.
//
// DEFECT this guards: `method: "GET"` was accepted, and the handler then
// presigned the grant's OWN key — a value it had just generated at random.
// No request field names an existing object, so that key holds nothing and
// the returned URL answered 404 on every backend. The previous version of
// this test asserted the dead URL and so certified the defect. An
// authenticated download has its own endpoint, GET
// /api/v2/artifacts/objects/{projectID}/{bucket}/{key}.
func TestArtifactGrantCreateRefusesADownloadGrant(t *testing.T) {
	h, _, _ := newObjectTestHandler(t)
	r := newGrantTestRouter(h)

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, newGrantRequest(t, "/grants/1/reports", `{"method":"GET","content_type":"image/png","max_bytes":1024}`))

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "method must be PUT") {
		t.Fatalf("body = %s, want the reason for the refusal", rr.Body.String())
	}
}

func TestArtifactGrantCreateRejectsInvalidMethod(t *testing.T) {
	h, _, _ := newObjectTestHandler(t)
	r := newGrantTestRouter(h)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, newGrantRequest(t, "/grants/1/reports", `{"method":"DELETE","content_type":"image/png","max_bytes":1024}`))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

func TestArtifactGrantCreateRejectsMissingContentType(t *testing.T) {
	h, _, _ := newObjectTestHandler(t)
	r := newGrantTestRouter(h)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, newGrantRequest(t, "/grants/1/reports", `{"method":"PUT","max_bytes":1024}`))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

func TestArtifactGrantCreateRejectsNonPositiveMaxBytes(t *testing.T) {
	h, _, _ := newObjectTestHandler(t)
	r := newGrantTestRouter(h)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, newGrantRequest(t, "/grants/1/reports", `{"method":"PUT","content_type":"image/png","max_bytes":0}`))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

// TestArtifactGrantCreateRejectsPartialDigestDeclaration proves the plan's
// "reject a grant request with no digest when the caller declares one is
// required" reasoning applied the other direction too: declaring only one
// of digest_alg/digest is a caller error, not something to silently accept
// and leave unverifiable at commit.
func TestArtifactGrantCreateRejectsPartialDigestDeclaration(t *testing.T) {
	h, _, _ := newObjectTestHandler(t)
	r := newGrantTestRouter(h)

	for _, body := range []string{
		`{"method":"PUT","content_type":"image/png","max_bytes":1024,"digest_alg":"sha256"}`,
		`{"method":"PUT","content_type":"image/png","max_bytes":1024,"digest":"` + strings.Repeat("00", 32) + `"}`,
	} {
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, newGrantRequest(t, "/grants/1/reports", body))
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
		}
	}
}

func TestArtifactGrantCreateRejectsUnsupportedDigestAlgorithm(t *testing.T) {
	h, _, _ := newObjectTestHandler(t)
	r := newGrantTestRouter(h)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, newGrantRequest(t, "/grants/1/reports",
		`{"method":"PUT","content_type":"image/png","max_bytes":1024,"digest_alg":"md5","digest":"`+strings.Repeat("00", 16)+`"}`))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

// TestArtifactGrantCreateRejectsMalformedDigest exercises
// parseGrantDigest's two remaining failure branches beyond the algorithm
// check: non-hex characters, and hex that decodes but isn't 32 bytes.
func TestArtifactGrantCreateRejectsMalformedDigest(t *testing.T) {
	h, _, _ := newObjectTestHandler(t)
	r := newGrantTestRouter(h)

	for name, digest := range map[string]string{
		"not hex":        "not-valid-hex-at-all-zzzzzzzzzzzzzzzzzzzzzzzzzzzzz",
		"wrong length":   strings.Repeat("00", 16),
		"empty":          "",
		"odd hex length": "0",
	} {
		t.Run(name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			r.ServeHTTP(rr, newGrantRequest(t, "/grants/1/reports",
				`{"method":"PUT","content_type":"image/png","max_bytes":1024,"digest_alg":"sha256","digest":"`+digest+`"}`))
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
			}
		})
	}
}

// createTestGrant issues a grant through the real handler and returns its
// grant_id and content-type, so commit tests exercise the exact code path
// CommitTransferGrant reads back — not a hand-built repository row.
func createTestGrant(t *testing.T, r chi.Router, body string) string {
	t.Helper()
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, newGrantRequest(t, "/grants/1/reports", body))
	if rr.Code != http.StatusOK {
		t.Fatalf("create grant: status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	return decodeGrantResponse(t, rr.Body).GrantID
}

// simulateGrantUpload calls Put directly against the fake store, standing
// in for the out-of-band presigned PUT a real client would perform — commit
// only ever observes the object through Get, so it cannot tell the
// difference.
func simulateGrantUpload(t *testing.T, store *fakeStore, grantID, contentType string, content []byte) {
	t.Helper()
	ref, err := storage.NewObjectRef("1", "reports", grantID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(t.Context(), ref, bytes.NewReader(content), storage.PutOptions{ContentType: contentType}); err != nil {
		t.Fatal(err)
	}
}

func TestArtifactGrantCommitProducesListableObjectAndConsumesGrant(t *testing.T) {
	h, repo, store := newObjectTestHandler(t)
	r := newGrantTestRouter(h)

	content := []byte("hello, grant")
	digest := sha256.Sum256(content)
	grantID := createTestGrant(t, r, `{"method":"PUT","content_type":"text/plain","max_bytes":1024,"digest_alg":"sha256","digest":"`+hex.EncodeToString(digest[:])+`"}`)
	simulateGrantUpload(t, store, grantID, "text/plain", content)

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/grants/1/"+grantID+":commit", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Key       string `json:"key"`
		SizeBytes int64  `json:"size_bytes"`
		MediaType string `json:"media_type"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode commit response: %v (body=%s)", err, rr.Body.String())
	}
	if resp.Key != grantID || resp.SizeBytes != int64(len(content)) || resp.MediaType != "text/plain" {
		t.Fatalf("resp = %+v", resp)
	}

	bucket, err := repo.GetBucket(t.Context(), 1, "reports")
	if err != nil {
		t.Fatal(err)
	}
	if record, ok := repo.objects[fakeObjectKey(bucket.ID, grantID)]; !ok || record.byteLength != int64(len(content)) {
		t.Fatalf("expected object metadata for %q to be recorded, got ok=%v record=%+v", grantID, ok, record)
	}

	// Single-use: a second commit on the same grant must fail.
	rr2 := httptest.NewRecorder()
	r.ServeHTTP(rr2, httptest.NewRequest(http.MethodPost, "/grants/1/"+grantID+":commit", nil))
	if rr2.Code != http.StatusConflict {
		t.Fatalf("second commit: status = %d, want 409; body=%s", rr2.Code, rr2.Body.String())
	}
}

// presignedGrantKey is the key a presigned grant URL addresses (fakeStore's
// PresignPut puts it after "/put/").
func presignedGrantKey(t *testing.T, url string) string {
	t.Helper()
	_, key, ok := strings.Cut(url, "https://presigned.example.test/put/")
	if !ok || key == "" {
		t.Fatalf("url = %q is not a fake presigned PUT URL", url)
	}
	return key
}

func downloadGrantObject(t *testing.T, h *artifacts.Handler, key string) []byte {
	t.Helper()
	r := chi.NewRouter()
	r.Get("/objects/{projectID}/{bucket}/*", h.DownloadObject)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/objects/1/reports/"+key, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("download %q: status = %d; body=%s", key, rr.Code, rr.Body.String())
	}
	return rr.Body.Bytes()
}

// A presigned PUT URL cannot be revoked and outlives the commit. It used to
// address the committed object itself, so a PUT after the commit replaced
// verified bytes with unverified ones, past the digest, size and quota checks
// (follow-up to #1290). The URL now addresses a staging key: the commit
// copies it to grant_id and verifies the copy, and a later PUT changes
// nothing the artifact serves.
func TestArtifactGrantPresignedPutAfterCommitDoesNotChangeTheServedObject(t *testing.T) {
	h, repo, store := newObjectTestHandler(t)
	store.presign = true
	r := newGrantTestRouter(h)

	content := []byte("verified bytes")
	digest := sha256.Sum256(content)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, newGrantRequest(t, "/grants/1/reports", `{"method":"PUT","content_type":"text/plain","max_bytes":64,"digest_alg":"sha256","digest":"`+hex.EncodeToString(digest[:])+`"}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("create grant: status = %d; body=%s", rr.Code, rr.Body.String())
	}
	grant := decodeGrantResponse(t, rr.Body)
	urlKey := presignedGrantKey(t, grant.URL)
	if urlKey == grant.GrantID {
		t.Fatalf("the grant URL addresses the committed key %q", urlKey)
	}
	simulateGrantUpload(t, store, urlKey, "text/plain", content)

	rr = httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/grants/1/"+grant.GrantID+":commit", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("commit: status = %d; body=%s", rr.Code, rr.Body.String())
	}
	var committed struct {
		Key       string `json:"key"`
		SizeBytes int64  `json:"size_bytes"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &committed); err != nil {
		t.Fatal(err)
	}
	if committed.Key != grant.GrantID || committed.SizeBytes != int64(len(content)) {
		t.Fatalf("commit response = %+v, want key %q", committed, grant.GrantID)
	}
	if store.objectCount() != 1 {
		t.Fatalf("objects after commit = %d, want 1: the staging object was not deleted", store.objectCount())
	}

	// The out-of-band rewrite through the still-valid URL.
	simulateGrantUpload(t, store, urlKey, "text/plain", []byte("unverified, much longer replacement bytes"))

	if got := downloadGrantObject(t, h, grant.GrantID); !bytes.Equal(got, content) {
		t.Fatalf("served %q after a post-commit PUT, want %q", got, content)
	}
	bucket, err := repo.GetBucket(t.Context(), 1, "reports")
	if err != nil {
		t.Fatal(err)
	}
	if record := repo.objects[fakeObjectKey(bucket.ID, grant.GrantID)]; record.byteLength != int64(len(content)) {
		t.Fatalf("byte_length = %d, want %d", record.byteLength, len(content))
	}
}

// copyingStore is a fakeStore with a server-side copy (storage.ObjectCopier),
// as S3, GCS and Azure have.
type copyingStore struct {
	*fakeStore
	copies []string
}

func (s *copyingStore) Copy(_ context.Context, src, dst storage.ObjectRef) (storage.ObjectInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	info, ok := s.objects[src.StorageKey("")]
	if !ok {
		return storage.ObjectInfo{}, storage.ErrNotFound
	}
	info.Key = dst.Key()
	s.objects[dst.StorageKey("")] = info
	s.data[dst.StorageKey("")] = append([]byte(nil), s.data[src.StorageKey("")]...)
	s.copies = append(s.copies, src.Key()+"->"+dst.Key())
	return info, nil
}

// A backend with a server-side copy commits through it: the bytes do not
// pass through Main a second time.
func TestArtifactGrantStagedCommitUsesTheServerSideCopy(t *testing.T) {
	_, repo, fake := newObjectTestHandler(t)
	fake.presign = true
	store := &copyingStore{fakeStore: fake}
	h := artifacts.NewHandler(repo, store)
	r := newGrantTestRouter(h)

	grant := createTestGrant(t, r, `{"method":"PUT","content_type":"text/plain","max_bytes":64}`)
	simulateGrantUpload(t, fake, grant+".upload", "text/plain", []byte("copied"))

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/grants/1/"+grant+":commit", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("commit: status = %d; body=%s", rr.Code, rr.Body.String())
	}
	if len(store.copies) != 1 || store.copies[0] != grant+".upload->"+grant {
		t.Fatalf("copies = %v", store.copies)
	}
	if got := downloadGrantObject(t, h, grant); string(got) != "copied" {
		t.Fatalf("served %q", got)
	}
}

// The copy is what commit verifies, so a staged upload that fails a check
// leaves neither the copy nor a metadata row behind, and the grant stays
// uncommitted.
func TestArtifactGrantStagedCommitWithMismatchedDigestDeletesTheCopy(t *testing.T) {
	h, repo, store := newObjectTestHandler(t)
	store.presign = true
	r := newGrantTestRouter(h)

	declared := sha256.Sum256([]byte("expected"))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, newGrantRequest(t, "/grants/1/reports", `{"method":"PUT","content_type":"text/plain","max_bytes":64,"digest_alg":"sha256","digest":"`+hex.EncodeToString(declared[:])+`"}`))
	grant := decodeGrantResponse(t, rr.Body)
	urlKey := presignedGrantKey(t, grant.URL)
	simulateGrantUpload(t, store, urlKey, "text/plain", []byte("something else"))

	rr = httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/grants/1/"+grant.GrantID+":commit", nil))
	if rr.Code != http.StatusConflict {
		t.Fatalf("commit: status = %d, want 409; body=%s", rr.Code, rr.Body.String())
	}
	ref, err := storage.NewObjectRef("1", "reports", grant.GrantID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Stat(t.Context(), ref); err == nil {
		t.Fatal("the rejected copy is still at the final key")
	}
	bucket, err := repo.GetBucket(t.Context(), 1, "reports")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := repo.objects[fakeObjectKey(bucket.ID, grant.GrantID)]; ok {
		t.Fatal("a rejected staged commit recorded metadata")
	}
}

func TestArtifactGrantCommitWithMismatchedDigestReturns409AndDeletesObject(t *testing.T) {
	h, _, store := newObjectTestHandler(t)
	r := newGrantTestRouter(h)

	declaredDigest := sha256.Sum256([]byte("expected content"))
	grantID := createTestGrant(t, r, `{"method":"PUT","content_type":"text/plain","max_bytes":1024,"digest_alg":"sha256","digest":"`+hex.EncodeToString(declaredDigest[:])+`"}`)
	simulateGrantUpload(t, store, grantID, "text/plain", []byte("different content actually uploaded"))

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/grants/1/"+grantID+":commit", nil))
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rr.Code, rr.Body.String())
	}
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error.Code != "DigestMismatch" {
		t.Fatalf("error.code = %q, want DigestMismatch", envelope.Error.Code)
	}
	if store.objectCount() != 0 {
		t.Fatalf("expected the mismatched object to be deleted, got %d objects", store.objectCount())
	}
}

func TestArtifactGrantCommitWithMismatchedMediaTypeReturns409AndDeletesObject(t *testing.T) {
	h, _, store := newObjectTestHandler(t)
	r := newGrantTestRouter(h)

	grantID := createTestGrant(t, r, `{"method":"PUT","content_type":"image/png","max_bytes":1024}`)
	simulateGrantUpload(t, store, grantID, "application/pdf", []byte("not actually a png"))

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/grants/1/"+grantID+":commit", nil))
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rr.Code, rr.Body.String())
	}
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error.Code != "MediaTypeMismatch" {
		t.Fatalf("error.code = %q, want MediaTypeMismatch", envelope.Error.Code)
	}
	if store.objectCount() != 0 {
		t.Fatalf("expected the mismatched object to be deleted, got %d objects", store.objectCount())
	}
}

func TestArtifactGrantCommitExceedingMaxBytesReturns413AndDeletesObject(t *testing.T) {
	h, _, store := newObjectTestHandler(t)
	r := newGrantTestRouter(h)

	grantID := createTestGrant(t, r, `{"method":"PUT","content_type":"text/plain","max_bytes":4}`)
	simulateGrantUpload(t, store, grantID, "text/plain", []byte("this is way more than 4 bytes"))

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/grants/1/"+grantID+":commit", nil))
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body=%s", rr.Code, rr.Body.String())
	}
	if store.objectCount() != 0 {
		t.Fatalf("expected the oversized object to be deleted, got %d objects", store.objectCount())
	}
}

func TestArtifactGrantCommitUnknownGrantReturns404(t *testing.T) {
	h, _, _ := newObjectTestHandler(t)
	r := newGrantTestRouter(h)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/grants/1/00000000-0000-4000-8000-000000000000:commit", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
}

// TestArtifactGrantCommitMalformedGrantIDReturns404 proves looksLikeGrantID's
// defensive check: a grantID that cannot possibly be a UUID must not reach
// the repository at all (which would otherwise surface a raw Postgres
// "invalid input syntax for type uuid" as a 500 in production).
func TestArtifactGrantCommitMalformedGrantIDReturns404(t *testing.T) {
	h, _, _ := newObjectTestHandler(t)
	r := newGrantTestRouter(h)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/grants/1/not-a-uuid:commit", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
}

// TestArtifactGrantCommitMalformedGrantIDVariantsReturn404 exercises
// looksLikeGrantID's per-character checks individually — right length but a
// dash in the wrong place, and right shape but a non-hex character — not
// just the "obviously not a UUID at all" case above.
func TestArtifactGrantCommitMalformedGrantIDVariantsReturn404(t *testing.T) {
	h, _, _ := newObjectTestHandler(t)
	r := newGrantTestRouter(h)

	for name, id := range map[string]string{
		"dash in wrong place": "111111112-111-4111-8111-111111111111",
		"non-hex character":   "zzzzzzzz-1111-4111-8111-111111111111",
	} {
		t.Run(name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			r.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/grants/1/"+id+":commit", nil))
			if rr.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404; body=%s", rr.Code, rr.Body.String())
			}
		})
	}
}

// TestArtifactGrantCommitExpiredGrantReturns412 proves commit's own
// defense-in-depth expiry check (beyond "an expired grant URL returns 403
// from the provider" — the provider-side behavior a presigned URL itself
// enforces, which this test cannot exercise against a fake store). Backdates
// the grant's expires_at directly on the fake repository, since waiting out
// a real 15-minute TTL is not a reasonable thing to do in a test.
func TestArtifactGrantCommitExpiredGrantReturns412(t *testing.T) {
	h, repo, _ := newObjectTestHandler(t)
	r := newGrantTestRouter(h)
	grantID := createTestGrant(t, r, `{"method":"PUT","content_type":"image/png","max_bytes":1024}`)

	grant := repo.grants[grantID]
	grant.ExpiresAt = time.Now().Add(-time.Minute)
	repo.grants[grantID] = grant

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/grants/1/"+grantID+":commit", nil))
	if rr.Code != http.StatusPreconditionFailed {
		t.Fatalf("status = %d, want 412; body=%s", rr.Code, rr.Body.String())
	}
}

// TestArtifactGrantCommitWithEmptyBackendContentTypeReturns409 proves the
// media-type check is mandatory, not defensive (S15's explicit requirement):
// a backend that reports no ContentType at all (info.ContentType == "" —
// e.g. a provider that doesn't echo it back, or an out-of-band client that
// omitted the header on its presigned PUT) must still be rejected against a
// grant that declared one, not silently waved through.
func TestArtifactGrantCommitWithEmptyBackendContentTypeReturns409(t *testing.T) {
	h, _, store := newObjectTestHandler(t)
	r := newGrantTestRouter(h)

	grantID := createTestGrant(t, r, `{"method":"PUT","content_type":"image/png","max_bytes":1024}`)
	simulateGrantUpload(t, store, grantID, "", []byte("no content-type reported by the backend"))

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/grants/1/"+grantID+":commit", nil))
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rr.Code, rr.Body.String())
	}
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error.Code != "MediaTypeMismatch" {
		t.Fatalf("error.code = %q, want MediaTypeMismatch", envelope.Error.Code)
	}
	if store.objectCount() != 0 {
		t.Fatalf("expected the mismatched object to be deleted, got %d objects", store.objectCount())
	}
}

// TestArtifactGrantCommitExceedingProjectQuotaReturns413AndRollsBack proves
// commit enforces S12's project storage quota exactly like UploadObject does
// (objects_limits_test.go's TestArtifactLimitUploadExceedingProjectQuotaReturns413AndRollsBack) —
// a grant-based commit that pushes the project's total past max_total_bytes
// must not be allowed to bypass the same control a direct upload is subject
// to.
func TestArtifactGrantCommitExceedingProjectQuotaReturns413AndRollsBack(t *testing.T) {
	h, repo, store := newObjectTestHandler(t)
	limit := int64(150)
	repo.setPolicy(repos.ProjectStoragePolicy{ProjectID: 1, MaxTotalBytes: &limit})
	r := newGrantTestRouter(h)

	firstGrantID := createTestGrant(t, r, `{"method":"PUT","content_type":"text/plain","max_bytes":1024}`)
	simulateGrantUpload(t, store, firstGrantID, "text/plain", bytes.Repeat([]byte("A"), 100))
	firstRR := httptest.NewRecorder()
	r.ServeHTTP(firstRR, httptest.NewRequest(http.MethodPost, "/grants/1/"+firstGrantID+":commit", nil))
	if firstRR.Code != http.StatusOK {
		t.Fatalf("first commit: status = %d, want 200; body=%s", firstRR.Code, firstRR.Body.String())
	}

	secondGrantID := createTestGrant(t, r, `{"method":"PUT","content_type":"text/plain","max_bytes":1024}`)
	simulateGrantUpload(t, store, secondGrantID, "text/plain", bytes.Repeat([]byte("B"), 100))
	secondRR := httptest.NewRecorder()
	r.ServeHTTP(secondRR, httptest.NewRequest(http.MethodPost, "/grants/1/"+secondGrantID+":commit", nil))
	if secondRR.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("second commit: status = %d, want 413; body=%s", secondRR.Code, secondRR.Body.String())
	}
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(secondRR.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error.Code != "TooLarge" {
		t.Fatalf("error.code = %q, want TooLarge", envelope.Error.Code)
	}

	// Rolled back: the second object must not exist physically...
	if store.objectCount() != 1 {
		t.Fatalf("expected only the first commit's object to remain in the store, got %d objects", store.objectCount())
	}
	// ...nor count toward the project's metadata total.
	total, err := repo.SumProjectBytes(t.Context(), 1)
	if err != nil {
		t.Fatalf("SumProjectBytes: %v", err)
	}
	if total != 100 {
		t.Fatalf("SumProjectBytes = %d, want 100 (only the first commit, second rolled back)", total)
	}

	// The grant itself is not marked consumed on rollback — the plan's
	// existing rejectCommit path deletes the object but does not, and must
	// not, call MarkTransferGrantConsumed on a rejected commit.
	if _, err := repo.GetTransferGrant(t.Context(), secondGrantID, 1); err != nil {
		t.Fatalf("GetTransferGrant after rollback: %v", err)
	}
}

// Commit still refuses a non-PUT grant.
//
// The row is written straight through the repository, because the API no
// longer creates one: a GET grant is refused at creation now. A deployment
// can still hold rows written before that, so the commit guard must stay.
func TestArtifactGrantCommitNonPutGrantReturns400(t *testing.T) {
	h, repo, _ := newObjectTestHandler(t)
	r := newGrantTestRouter(h)

	const grantID = "11111111-2222-4333-8444-555555555555"
	if _, err := repo.CreateTransferGrant(t.Context(), repos.NewTransferGrantInput{
		ID:          grantID,
		ProjectID:   1,
		BucketID:    1,
		Key:         grantID,
		Method:      "GET",
		ContentType: "image/png",
		MaxBytes:    1024,
		ExpiresAt:   time.Now().Add(15 * time.Minute),
	}); err != nil {
		t.Fatalf("seed the legacy grant: %v", err)
	}

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/grants/1/"+grantID+":commit", nil))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

// grantAuditCapturingHandler is a minimal slog.Handler capturing every
// emitted record's attributes — no shared testutil exists in this codebase,
// so this mirrors internal/infra/storage/observability_test.go's own
// capturingSlogHandler locally rather than exporting one across packages.
type grantAuditCapturingHandler struct {
	records []map[string]string
}

func (h *grantAuditCapturingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *grantAuditCapturingHandler) Handle(_ context.Context, r slog.Record) error {
	attrs := make(map[string]string, r.NumAttrs())
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.String()
		return true
	})
	h.records = append(h.records, attrs)
	return nil
}

func (h *grantAuditCapturingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *grantAuditCapturingHandler) WithGroup(string) slog.Handler      { return h }

// TestArtifactGrantCreateEmitsAuditLogOnSuccess closes a real gap an
// adversarial review found: nothing in this package's test suite asserted
// that a successful CreateTransferGrant call actually invokes
// storage.LogAudit — grants.go:252's call could be deleted entirely and
// every other test here would stay green. This asserts on the real emitted
// slog record, not by calling LogAudit directly.
func TestArtifactGrantCreateEmitsAuditLogOnSuccess(t *testing.T) {
	handler := &grantAuditCapturingHandler{}
	previous := slog.Default()
	slog.SetDefault(slog.New(handler))
	defer slog.SetDefault(previous)

	h, _, _ := newObjectTestHandler(t)
	r := newGrantTestRouter(h)

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, newGrantRequest(t, "/grants/1/reports", `{"method":"PUT","content_type":"image/png","max_bytes":1024}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	resp := decodeGrantResponse(t, rr.Body)

	var auditRecord map[string]string
	for _, rec := range handler.records {
		if rec["operation"] == "grant_issued" {
			auditRecord = rec
			break
		}
	}
	if auditRecord == nil {
		t.Fatalf("no grant_issued audit record emitted; got records: %+v", handler.records)
	}
	want := map[string]string{
		"operation":  "grant_issued",
		"bucket":     "reports",
		"key":        resp.GrantID,
		"project_id": "1",
		"outcome":    "success",
	}
	for k, wantV := range want {
		if got := auditRecord[k]; got != wantV {
			t.Errorf("grant_issued audit record attr %q = %q, want %q", k, got, wantV)
		}
	}
}
