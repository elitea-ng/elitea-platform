package artifacts_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/artifacts"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
)

// These tests pin the S3-shaped WRITE verbs — PUT, DELETE, HEAD — the SDK's
// artifact toolkit speaks (elitea-sdk runtime/clients/client.py:
// upload_artifact_s3 :1123, delete_artifact_s3 :1186, head_artifact_s3 :1206).
//
// Same discipline as s3_test.go: JSON bodies are decoded into map[string]any,
// never a struct, because a struct silently accepts a renamed or re-cased
// field and leaves the zero value behind — the exact regression class that
// produced the original listing defect.
//
// Every write assertion is a ROUND TRIP where it can be: a PUT is proven by
// reading the bytes back, a DELETE by the object no longer being there. A
// status code alone would pass against a handler that answered 200 and stored
// nothing, which is the failure this whole surface exists to rule out.

// s3PutRequest builds the request shape upload_artifact_s3 actually produces:
// raw bytes as the body (requests.put(url, data=data)), the media type in the
// Content-Type header, and project_id in the query string. Not multipart —
// that is the native route's shape, and confusing the two is the one wire
// difference between the two upload representations.
func s3PutRequest(target, contentType string, body []byte) *http.Request {
	req := httptest.NewRequest(http.MethodPut, target, bytes.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return req
}

func serveS3(h *artifacts.Handler, req *http.Request) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	newS3TestRouter(h).ServeHTTP(rr, req)
	return rr
}

// decodeS3JSON decodes a response body as a raw map, failing if it is not
// JSON — used for both the PUT success envelope and every error envelope.
func decodeS3JSON(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v (body=%s)", err, rr.Body.String())
	}
	return body
}

// ---------------------------------------------------------------------------
// PUT
// ---------------------------------------------------------------------------

// TestUploadObjectS3_StoresRawBodyAndReadsBack is the central PUT claim, and
// deliberately a round trip: the bytes the SDK PUT must be the bytes a
// subsequent GET returns, byte for byte, with the Content-Type it declared.
// A handler that answered 200 without calling Put — or that mangled the body
// by running it through a form parser — passes a status assertion and fails
// this one.
func TestUploadObjectS3_StoresRawBodyAndReadsBack(t *testing.T) {
	h, _, _ := newObjectTestHandler(t)
	content := []byte("the quick brown fox\n")

	rr := serveS3(h, s3PutRequest("/artifacts/s3/reports/notes.txt?project_id=1&format=json", "text/plain; charset=utf-8", content))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}

	body := decodeS3JSON(t, rr)
	// The envelope is in the listing's camelCase vocabulary, not the native
	// route's snake_case one — the two representations must not share names.
	for _, key := range []string{"key", "bucket", "size", "lastModified"} {
		if _, ok := body[key]; !ok {
			t.Errorf("response has no %q key; keys=%v", key, keysOf(body))
		}
	}
	for _, forbidden := range []string{"size_bytes", "media_type", "created_at"} {
		if _, ok := body[forbidden]; ok {
			t.Errorf("response carries native-route key %q", forbidden)
		}
	}
	if got := body["key"]; got != "notes.txt" {
		t.Errorf(`body["key"] = %v, want "notes.txt"`, got)
	}
	if got := body["size"]; got != float64(len(content)) {
		t.Errorf(`body["size"] = %v, want %d`, got, len(content))
	}

	// The round trip. Same handler, the read route the SDK would use next.
	back := getS3Object(t, h, "/artifacts/s3/reports/notes.txt?project_id=1")
	if back.Code != http.StatusOK {
		t.Fatalf("download after upload: status = %d, want 200; body=%s", back.Code, back.Body.String())
	}
	if !bytes.Equal(back.Body.Bytes(), content) {
		t.Errorf("downloaded %q, want the uploaded bytes %q", back.Body.Bytes(), content)
	}
	if got := back.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type after round trip = %q, want the type the PUT declared", got)
	}
}

// TestUploadObjectS3_KeyWithSlashes is the route-shape guard for the write
// half. The SDK quotes the key with safe='/' (client.py:1152), so a nested key
// arrives as literal path segments; a route capturing a single {key} segment
// would 404 every write into a folder. It also checks the key is stored whole
// rather than truncated to its basename — the trap RFC 7578's FileName()
// creates on the native route (see UploadObject's comment).
func TestUploadObjectS3_KeyWithSlashes(t *testing.T) {
	h, _, _ := newObjectTestHandler(t)
	content := []byte("nested content")

	rr := serveS3(h, s3PutRequest("/artifacts/s3/reports/folder/sub/file.txt?project_id=1", "text/plain", content))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a key containing slashes; body=%s", rr.Code, rr.Body.String())
	}
	if got := decodeS3JSON(t, rr)["key"]; got != "folder/sub/file.txt" {
		t.Fatalf(`body["key"] = %v, want the whole path as the key`, got)
	}

	back := getS3Object(t, h, "/artifacts/s3/reports/folder/sub/file.txt?project_id=1")
	if back.Code != http.StatusOK || !bytes.Equal(back.Body.Bytes(), content) {
		t.Fatalf("round trip of a nested key: status = %d body = %q", back.Code, back.Body.Bytes())
	}
	// And it must be one key, not a basename that happens to answer.
	if basename := getS3Object(t, h, "/artifacts/s3/reports/file.txt?project_id=1"); basename.Code != http.StatusNotFound {
		t.Errorf("the key was truncated to its basename: /file.txt answered %d", basename.Code)
	}
}

// TestUploadObjectS3_ListingSeesTheUploadedKey closes the loop the SDK's own
// toolkit closes: a file written through the S3 PUT must appear in the S3
// listing under the same key. The two representations share no code on the
// write side, so nothing else proves they agree.
func TestUploadObjectS3_ListingSeesTheUploadedKey(t *testing.T) {
	h, _, _ := newObjectTestHandler(t)
	if rr := serveS3(h, s3PutRequest("/artifacts/s3/reports/docs/a.txt?project_id=1", "text/plain", []byte("x"))); rr.Code != http.StatusOK {
		t.Fatalf("upload: status = %d; body=%s", rr.Code, rr.Body.String())
	}

	code, body := getS3Listing(t, h, "/artifacts/s3/reports?project_id=1&list-type=2")
	if code != http.StatusOK {
		t.Fatalf("listing: status = %d; body=%v", code, body)
	}
	contents, _ := body["contents"].([]any)
	if len(contents) != 1 {
		t.Fatalf("contents = %v, want the uploaded key", body["contents"])
	}
	item := contents[0].(map[string]any)
	if item["key"] != "docs/a.txt" || item["size"] != float64(1) {
		t.Errorf("listed entry = %v, want key docs/a.txt of size 1", item)
	}
}

// TestUploadObjectS3_OverwritesExistingKey pins the deliberate divergence from
// the native POST: S3 PUT is an upsert, and the SDK sends no overwrite flag,
// so a second write to the same key must replace it rather than 409. A 409
// here would reach the SDK as "S3 error: AlreadyExists" with no way to opt
// out, making the second save of any file fail permanently.
func TestUploadObjectS3_OverwritesExistingKey(t *testing.T) {
	h, _, store := newObjectTestHandler(t)
	if rr := serveS3(h, s3PutRequest("/artifacts/s3/reports/a.txt?project_id=1", "text/plain", []byte("first"))); rr.Code != http.StatusOK {
		t.Fatalf("first upload: status = %d; body=%s", rr.Code, rr.Body.String())
	}

	rr := serveS3(h, s3PutRequest("/artifacts/s3/reports/a.txt?project_id=1", "text/plain", []byte("second")))
	if rr.Code != http.StatusOK {
		t.Fatalf("overwrite: status = %d, want 200 (S3 PUT is an upsert); body=%s", rr.Code, rr.Body.String())
	}

	back := getS3Object(t, h, "/artifacts/s3/reports/a.txt?project_id=1")
	if got := back.Body.String(); got != "second" {
		t.Errorf("body after overwrite = %q, want %q", got, "second")
	}
	if store.objectCount() != 1 {
		t.Errorf("objectCount = %d, want 1 — the overwrite must replace, not duplicate", store.objectCount())
	}
}

// TestUploadObjectS3_FallsBackToExtensionContentType covers a client that
// sends no Content-Type at all (the SDK always does, having detected one
// itself). Without the fallback the object would be stored with an empty
// media type and served back as one, which the SDK's own parser then has to
// guess at.
func TestUploadObjectS3_FallsBackToExtensionContentType(t *testing.T) {
	h, _, _ := newObjectTestHandler(t)

	rr := serveS3(h, s3PutRequest("/artifacts/s3/reports/diagram.png?project_id=1", "", []byte("\x89PNG")))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	back := getS3Object(t, h, "/artifacts/s3/reports/diagram.png?project_id=1")
	if got := back.Header().Get("Content-Type"); !strings.HasPrefix(got, "image/png") {
		t.Errorf("Content-Type = %q, want image/png derived from the extension", got)
	}
}

// TestUploadObjectS3_RecordsObjectMetadata proves the PUT goes through the
// SAME post-write accounting as the native upload rather than only touching
// the object store. Without the metadata row every S3-written object is
// invisible to SumBucketBytes/SumProjectBytes, which silently exempts this
// verb from the project quota and leaves bucket sizes wrong — the exact gap
// S12 closed for the native route.
func TestUploadObjectS3_RecordsObjectMetadata(t *testing.T) {
	h, repo, _ := newObjectTestHandler(t)

	if rr := serveS3(h, s3PutRequest("/artifacts/s3/reports/tracked.bin?project_id=1", "application/octet-stream", bytes.Repeat([]byte("A"), 42))); rr.Code != http.StatusOK {
		t.Fatalf("upload: status = %d; body=%s", rr.Code, rr.Body.String())
	}

	bucket, err := repo.GetBucket(t.Context(), 1, "reports")
	if err != nil {
		t.Fatalf("GetBucket: %v", err)
	}
	if size, _ := repo.SumBucketBytes(t.Context(), bucket.ID); size != 42 {
		t.Errorf("SumBucketBytes = %d, want 42", size)
	}
	if count, _ := repo.CountBucketObjects(t.Context(), bucket.ID); count != 1 {
		t.Errorf("CountBucketObjects = %d, want 1", count)
	}
	if total, _ := repo.SumProjectBytes(t.Context(), 1); total != 42 {
		t.Errorf("SumProjectBytes = %d, want 42", total)
	}
}

// TestUploadObjectS3_OverMaxObjectBytesReturns413 proves the S3 PUT is behind
// the same http.MaxBytesReader cap as the native upload. The code must be one
// the SDK can act on: EntityTooLarge degrades to "S3 error: EntityTooLarge",
// which is vague but true, where the InternalError fallback would blame the
// server for the caller's oversized object.
func TestUploadObjectS3_OverMaxObjectBytesReturns413(t *testing.T) {
	h, repo, store := newObjectTestHandler(t)
	small := int64(10)
	repo.setPolicy(repos.ProjectStoragePolicy{ProjectID: 1, MaxObjectBytes: &small})

	rr := serveS3(h, s3PutRequest("/artifacts/s3/reports/big.bin?project_id=1", "application/octet-stream", bytes.Repeat([]byte("A"), 1024)))
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body=%s", rr.Code, rr.Body.String())
	}
	if got := errorCodeOf(t, decodeS3JSON(t, rr)); got != "EntityTooLarge" {
		t.Errorf("error.code = %q, want EntityTooLarge", got)
	}
	if store.objectCount() != 0 {
		t.Errorf("objectCount = %d, want 0 — a rejected upload must leave nothing behind", store.objectCount())
	}
}

// TestUploadObjectS3_ExceedingProjectQuotaRollsBack is the S3 half of S12's
// quota enforcement, and the one that would be easiest to lose by writing a
// second, simpler upload path: the object is individually under
// max_object_bytes, so only the post-write SumProjectBytes check can catch it,
// and both the bytes and the metadata row must be rolled back while the
// earlier, legitimate upload survives untouched.
func TestUploadObjectS3_ExceedingProjectQuotaRollsBack(t *testing.T) {
	h, repo, store := newObjectTestHandler(t)
	limit := int64(150)
	repo.setPolicy(repos.ProjectStoragePolicy{ProjectID: 1, MaxTotalBytes: &limit})

	if rr := serveS3(h, s3PutRequest("/artifacts/s3/reports/first.bin?project_id=1", "application/octet-stream", bytes.Repeat([]byte("A"), 100))); rr.Code != http.StatusOK {
		t.Fatalf("first upload: status = %d; body=%s", rr.Code, rr.Body.String())
	}

	rr := serveS3(h, s3PutRequest("/artifacts/s3/reports/second.bin?project_id=1", "application/octet-stream", bytes.Repeat([]byte("B"), 100)))
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("second upload: status = %d, want 413; body=%s", rr.Code, rr.Body.String())
	}
	if got := errorCodeOf(t, decodeS3JSON(t, rr)); got != "EntityTooLarge" {
		t.Errorf("error.code = %q, want EntityTooLarge", got)
	}

	if store.objectCount() != 1 {
		t.Errorf("objectCount = %d, want 1 — second.bin must be rolled back", store.objectCount())
	}
	if total, _ := repo.SumProjectBytes(t.Context(), 1); total != 100 {
		t.Errorf("SumProjectBytes = %d, want 100 (only first.bin)", total)
	}
	if back := getS3Object(t, h, "/artifacts/s3/reports/first.bin?project_id=1"); back.Code != http.StatusOK {
		t.Errorf("first.bin: status = %d, want 200 — it must survive the rollback", back.Code)
	}
}

// TestUploadObjectS3_ScopesWriteToQueryProject is the handler-level half of
// the cross-tenant guarantee for a write. The bucket "reports" exists only in
// project 1, so a PUT naming project 2 must not create it there. (The RBAC
// half — a caller who cannot even name project 2 — is proven at the router
// level in internal/api.)
func TestUploadObjectS3_ScopesWriteToQueryProject(t *testing.T) {
	h, _, store := newObjectTestHandler(t)

	rr := serveS3(h, s3PutRequest("/artifacts/s3/reports/x.txt?project_id=2", "text/plain", []byte("x")))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for another project's bucket; body=%s", rr.Code, rr.Body.String())
	}
	if got := errorCodeOf(t, decodeS3JSON(t, rr)); got != "NoSuchBucket" {
		t.Errorf("error.code = %q, want NoSuchBucket", got)
	}
	if store.objectCount() != 0 {
		t.Errorf("objectCount = %d, want 0 — nothing may be written for an unresolvable bucket", store.objectCount())
	}
}

// TestUploadObjectS3_ErrorEnvelopeMatchesWhatTheSDKReads pins the error shape
// _handle_s3_error parses (client.py:1078-1089) for the write path.
func TestUploadObjectS3_ErrorEnvelopeMatchesWhatTheSDKReads(t *testing.T) {
	cases := []struct {
		name       string
		target     string
		wantStatus int
		wantCode   string
	}{
		{
			name:       "missing bucket",
			target:     "/artifacts/s3/nope/a.txt?project_id=1",
			wantStatus: http.StatusNotFound,
			wantCode:   "NoSuchBucket",
		},
		{
			// Written unescaped on purpose: nothing in the chain cleans the
			// path, so this is the form that actually reaches the handler.
			name:       "traversal key",
			target:     "/artifacts/s3/reports/../../etc/passwd?project_id=1",
			wantStatus: http.StatusBadRequest,
			wantCode:   "InvalidArgument",
		},
		{
			name:       "missing project_id",
			target:     "/artifacts/s3/reports/a.txt",
			wantStatus: http.StatusBadRequest,
			wantCode:   "InvalidArgument",
		},
		{
			name:       "non-numeric project_id",
			target:     "/artifacts/s3/reports/a.txt?project_id=notanumber",
			wantStatus: http.StatusBadRequest,
			wantCode:   "InvalidArgument",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _, _ := newObjectTestHandler(t)
			rr := serveS3(h, s3PutRequest(tc.target, "text/plain", []byte("payload")))
			if rr.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", rr.Code, tc.wantStatus, rr.Body.String())
			}
			if got := errorCodeOf(t, decodeS3JSON(t, rr)); got != tc.wantCode {
				t.Errorf("error.code = %q, want %q", got, tc.wantCode)
			}
		})
	}
}

// TestUploadObjectS3_DoesNotSanitizeTheKey records where sanitization lives.
// The SDK rewrites the key BEFORE it builds the URL (_sanitize_artifact_name,
// client.py:1140) and reports sanitized_name/was_sanitized from its own
// inputs; the server never sees the original. So a key the server DOES accept
// must be stored verbatim — a server-side rewrite would write to a key the
// caller cannot predict and a later download by the requested key would miss.
// Keys the server cannot store safely are rejected, not rewritten (see the
// traversal case above).
func TestUploadObjectS3_DoesNotSanitizeTheKey(t *testing.T) {
	h, _, _ := newObjectTestHandler(t)
	// The SDK's sanitizer would turn this into "Q4-Summary.txt"; the server
	// must not do that on its behalf. On the wire the space is percent-encoded
	// (quote(sanitized_key, safe='/'), client.py:1152) and arrives decoded.
	const key = "Q4 Summary.txt"
	const encoded = "Q4%20Summary.txt"

	rr := serveS3(h, s3PutRequest("/artifacts/s3/reports/"+encoded+"?project_id=1", "text/plain", []byte("x")))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if got := decodeS3JSON(t, rr)["key"]; got != key {
		t.Fatalf(`body["key"] = %v, want the key verbatim (%q)`, got, key)
	}
	if back := getS3Object(t, h, "/artifacts/s3/reports/"+encoded+"?project_id=1"); back.Code != http.StatusOK {
		t.Errorf("download by the requested key: status = %d, want 200", back.Code)
	}
}

// ---------------------------------------------------------------------------
// DELETE
// ---------------------------------------------------------------------------

func s3Delete(h *artifacts.Handler, target string) *httptest.ResponseRecorder {
	return serveS3(h, httptest.NewRequest(http.MethodDelete, target, nil))
}

// TestDeleteObjectS3_RemovesObjectAndMetadata is the central DELETE claim, and
// again a round trip: after a 204 the object must be gone from the store AND
// its metadata row from the quota accounting. Dropping only the bytes leaves a
// project permanently counting storage it no longer has.
func TestDeleteObjectS3_RemovesObjectAndMetadata(t *testing.T) {
	h, repo, store := newObjectTestHandler(t)
	if rr := serveS3(h, s3PutRequest("/artifacts/s3/reports/gone.txt?project_id=1", "text/plain", []byte("bytes"))); rr.Code != http.StatusOK {
		t.Fatalf("seed upload: status = %d; body=%s", rr.Code, rr.Body.String())
	}

	rr := s3Delete(h, "/artifacts/s3/reports/gone.txt?project_id=1")
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
	if rr.Body.Len() != 0 {
		t.Errorf("204 carried a body: %s", rr.Body.String())
	}

	if back := getS3Object(t, h, "/artifacts/s3/reports/gone.txt?project_id=1"); back.Code != http.StatusNotFound {
		t.Errorf("download after delete: status = %d, want 404", back.Code)
	}
	if store.objectCount() != 0 {
		t.Errorf("objectCount = %d, want 0", store.objectCount())
	}
	if total, _ := repo.SumProjectBytes(t.Context(), 1); total != 0 {
		t.Errorf("SumProjectBytes = %d, want 0 — the metadata row must go too", total)
	}
}

// TestDeleteObjectS3_KeyWithSlashes: the wildcard capture again, on the verb
// where getting it wrong is worst — a delete that matched only the basename
// would remove the wrong file.
func TestDeleteObjectS3_KeyWithSlashes(t *testing.T) {
	h, _, store := newObjectTestHandler(t)
	store.seedContent("1", "reports", "folder/sub/file.txt", []byte("nested"), "text/plain")
	store.seedContent("1", "reports", "file.txt", []byte("top level"), "text/plain")

	if rr := s3Delete(h, "/artifacts/s3/reports/folder/sub/file.txt?project_id=1"); rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
	if back := getS3Object(t, h, "/artifacts/s3/reports/folder/sub/file.txt?project_id=1"); back.Code != http.StatusNotFound {
		t.Errorf("nested key after delete: status = %d, want 404", back.Code)
	}
	// The same-named key at the root must be untouched.
	back := getS3Object(t, h, "/artifacts/s3/reports/file.txt?project_id=1")
	if back.Code != http.StatusOK || back.Body.String() != "top level" {
		t.Errorf("root file.txt: status = %d body = %q — the delete matched the wrong key", back.Code, back.Body.String())
	}
}

// TestDeleteObjectS3_ScopesDeleteToQueryProject is the cross-tenant guarantee
// at the handler level for a destructive verb: project 2 has a same-named
// bucket, so the request gets past the bucket check and only the object lookup
// being project-scoped keeps project 1's bytes safe.
//
// Since DELETE became idempotent this answers 204 either way, so the status
// carries NO information here and the survival check below is the entire
// assertion — a cross-tenant delete that succeeded in destroying the file
// would return exactly the same 204. Do not "simplify" this test to its status.
func TestDeleteObjectS3_ScopesDeleteToQueryProject(t *testing.T) {
	h, repo, store := newObjectTestHandler(t)
	store.seedContent("1", "reports", "secret.txt", []byte("classified"), "text/plain")
	if _, err := repo.CreateBucket(t.Context(), repos.NewBucketInput{
		ProjectID: 2, Name: "reports", DisplayName: "reports", BucketType: "local",
	}); err != nil {
		t.Fatalf("seed CreateBucket: %v", err)
	}

	if rr := s3Delete(h, "/artifacts/s3/reports/secret.txt?project_id=2"); rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (absent in project 2, idempotent); body=%s", rr.Code, rr.Body.String())
	}
	back := getS3Object(t, h, "/artifacts/s3/reports/secret.txt?project_id=1")
	if back.Code != http.StatusOK {
		t.Fatalf("project 1's object was destroyed by project 2's delete: status = %d", back.Code)
	}
	if got := back.Body.String(); got != "classified" {
		t.Errorf("project 1's object body = %q, want it intact", got)
	}
}

// TestDeleteObjectS3_IsIdempotent pins the S3 contract: deleting a key that is
// not there is success, and deleting twice is the same as deleting once. The
// second DELETE is the one that would have failed before — the first leaves the
// object genuinely absent, so a non-idempotent handler 404s on the repeat.
func TestDeleteObjectS3_IsIdempotent(t *testing.T) {
	h, _, store := newObjectTestHandler(t)
	store.seedContent("1", "reports", "twice.txt", []byte("payload"), "text/plain")

	for _, attempt := range []string{"first", "second"} {
		rr := s3Delete(h, "/artifacts/s3/reports/twice.txt?project_id=1")
		if rr.Code != http.StatusNoContent {
			t.Fatalf("%s delete: status = %d, want 204; body=%s", attempt, rr.Code, rr.Body.String())
		}
		if body := rr.Body.String(); body != "" {
			t.Errorf("%s delete: body = %q, want empty", attempt, body)
		}
	}

	// A key that never existed at all, in an existing bucket.
	if rr := s3Delete(h, "/artifacts/s3/reports/never-existed.txt?project_id=1"); rr.Code != http.StatusNoContent {
		t.Fatalf("absent key: status = %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
}

// TestDeleteObjectS3_ErrorEnvelopeMatchesWhatTheSDKReads pins the delete's
// error vocabulary for the failures that remain once an absent key is success
// (TestDeleteObjectS3_IsIdempotent). A missing BUCKET is still an error: the
// bucket is addressed by the caller's own configuration rather than by a key it
// is iterating, so silence there would hide a misconfigured toolkit.
func TestDeleteObjectS3_ErrorEnvelopeMatchesWhatTheSDKReads(t *testing.T) {
	cases := []struct {
		name       string
		target     string
		wantStatus int
		wantCode   string
	}{
		{
			name:       "missing bucket",
			target:     "/artifacts/s3/nope/a.txt?project_id=1",
			wantStatus: http.StatusNotFound,
			wantCode:   "NoSuchBucket",
		},
		{
			name:       "traversal key",
			target:     "/artifacts/s3/reports/../../etc/passwd?project_id=1",
			wantStatus: http.StatusBadRequest,
			wantCode:   "InvalidArgument",
		},
		{
			name:       "missing project_id",
			target:     "/artifacts/s3/reports/a.txt",
			wantStatus: http.StatusBadRequest,
			wantCode:   "InvalidArgument",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _, _ := newObjectTestHandler(t)
			rr := s3Delete(h, tc.target)
			if rr.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", rr.Code, tc.wantStatus, rr.Body.String())
			}
			if got := errorCodeOf(t, decodeS3JSON(t, rr)); got != tc.wantCode {
				t.Errorf("error.code = %q, want %q", got, tc.wantCode)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// HEAD
// ---------------------------------------------------------------------------

func s3Head(h *artifacts.Handler, target string) *httptest.ResponseRecorder {
	return serveS3(h, httptest.NewRequest(http.MethodHead, target, nil))
}

// TestStatObjectS3_ReportsEveryHeaderTheSDKReads pins the success contract:
// head_artifact_s3 builds its whole answer from four headers
// (client.py:1227-1232), so each is asserted by name and value. A missing
// Content-Length silently becomes size 0, which is indistinguishable from an
// empty file to the caller.
func TestStatObjectS3_ReportsEveryHeaderTheSDKReads(t *testing.T) {
	h, _, store := newObjectTestHandler(t)
	content := []byte("some bytes here")
	store.seedContent("1", "reports", "notes.txt", content, "text/plain; charset=utf-8")

	rr := s3Head(h, "/artifacts/s3/reports/notes.txt?project_id=1")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Content-Length"); got != strconv.Itoa(len(content)) {
		t.Errorf("Content-Length = %q, want %d", got, len(content))
	}
	if got := rr.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q, want the stored type", got)
	}
	if got := rr.Header().Get("Last-Modified"); got == "" {
		t.Error("Last-Modified is empty; the SDK reports it as lastModified")
	}
	if got := rr.Header().Get("ETag"); got == "" {
		t.Error("ETag is empty; the SDK reports it (stripped of quotes)")
	}
	// A HEAD response must carry no body at all — anything here would
	// contradict the Content-Length just reported.
	if rr.Body.Len() != 0 {
		t.Errorf("HEAD carried a body: %s", rr.Body.String())
	}
}

// TestStatObjectS3_AbsentKeyIs404WithNoBody is the SDK's most common HEAD
// answer, and not an error to it: 404 becomes {"exists": False}
// (client.py:1220-1221). The no-body assertion matters because any other
// status goes through _handle_s3_error, which on a body it cannot parse falls
// back to "HTTP_<status>" — so a body here would be read, and misread.
func TestStatObjectS3_AbsentKeyIs404WithNoBody(t *testing.T) {
	h, _, _ := newObjectTestHandler(t)

	for name, target := range map[string]string{
		"absent key":     "/artifacts/s3/reports/absent.txt?project_id=1",
		"absent bucket":  "/artifacts/s3/nope/a.txt?project_id=1",
		"another tenant": "/artifacts/s3/reports/a.txt?project_id=2",
	} {
		t.Run(name, func(t *testing.T) {
			rr := s3Head(h, target)
			if rr.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404; body=%s", rr.Code, rr.Body.String())
			}
			if rr.Body.Len() != 0 {
				t.Errorf("HEAD carried a body: %s", rr.Body.String())
			}
		})
	}
}

// TestStatObjectS3_MalformedRequestIs400WithNoBody covers the two rejections
// that happen before any lookup. They must not write the JSON envelope the
// other verbs use, for the same reason as above.
func TestStatObjectS3_MalformedRequestIs400WithNoBody(t *testing.T) {
	h, _, store := newObjectTestHandler(t)
	store.seedContent("1", "reports", "a.txt", []byte("x"), "text/plain")

	for name, target := range map[string]string{
		"missing project_id":     "/artifacts/s3/reports/a.txt",
		"non-numeric project_id": "/artifacts/s3/reports/a.txt?project_id=notanumber",
		"traversal key":          "/artifacts/s3/reports/../../etc/passwd?project_id=1",
	} {
		t.Run(name, func(t *testing.T) {
			rr := s3Head(h, target)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
			}
			if rr.Body.Len() != 0 {
				t.Errorf("HEAD carried a body: %s", rr.Body.String())
			}
		})
	}
}

// TestStatObjectS3_KeyWithSlashes: the wildcard capture on the existence
// check. Without it every HEAD of a file in a folder reports {"exists":
// False}, which reads as "safe to write" to a caller checking before a write.
func TestStatObjectS3_KeyWithSlashes(t *testing.T) {
	h, _, store := newObjectTestHandler(t)
	store.seedContent("1", "reports", "folder/sub/file.txt", []byte("nested"), "text/plain")

	rr := s3Head(h, "/artifacts/s3/reports/folder/sub/file.txt?project_id=1")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a key containing slashes", rr.Code)
	}
	if got := rr.Header().Get("Content-Length"); got != "6" {
		t.Errorf("Content-Length = %q, want 6", got)
	}
}

// TestS3WriteVerbsRoundTripTogether is the whole surface in one sequence, in
// the order an agent's toolkit uses it: HEAD says absent, PUT writes, HEAD
// says present with the right size, GET returns the bytes, DELETE removes it,
// HEAD says absent again. Each verb is checked against the others' effects
// rather than against its own status code, so a verb that reports success
// without acting is caught here even if its own test somehow passes.
func TestS3WriteVerbsRoundTripTogether(t *testing.T) {
	h, _, _ := newObjectTestHandler(t)
	const target = "/artifacts/s3/reports/folder/report.txt?project_id=1"
	content := []byte("round trip payload")

	if rr := s3Head(h, target); rr.Code != http.StatusNotFound {
		t.Fatalf("HEAD before upload: status = %d, want 404", rr.Code)
	}
	if rr := serveS3(h, s3PutRequest(target, "text/plain", content)); rr.Code != http.StatusOK {
		t.Fatalf("PUT: status = %d; body=%s", rr.Code, rr.Body.String())
	}
	head := s3Head(h, target)
	if head.Code != http.StatusOK {
		t.Fatalf("HEAD after upload: status = %d, want 200", head.Code)
	}
	if got := head.Header().Get("Content-Length"); got != strconv.Itoa(len(content)) {
		t.Errorf("HEAD Content-Length = %q, want %d", got, len(content))
	}
	if back := getS3Object(t, h, target); !bytes.Equal(back.Body.Bytes(), content) {
		t.Errorf("GET body = %q, want %q", back.Body.Bytes(), content)
	}
	if rr := s3Delete(h, target); rr.Code != http.StatusNoContent {
		t.Fatalf("DELETE: status = %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
	if rr := s3Head(h, target); rr.Code != http.StatusNotFound {
		t.Fatalf("HEAD after delete: status = %d, want 404", rr.Code)
	}
}

// timelessPutStore is a fakeStore whose Put reports no LastModified — the
// behaviour of the real S3 backend, whose Put returns only the key, size and
// ETag. fakeStore.Put stamps time.Now(), so without this double nothing in
// this package could observe the zero-time case at all; it was found against
// the running standalone stack, where the PUT response carried
// "lastModified":"0001-01-01T00:00:00Z".
type timelessPutStore struct{ *fakeStore }

func (s timelessPutStore) Put(ctx context.Context, ref storage.ObjectRef, body io.Reader, opts storage.PutOptions) (storage.ObjectInfo, error) {
	info, err := s.fakeStore.Put(ctx, ref, body, opts)
	info.LastModified = time.Time{}
	return info, err
}

// TestUploadObjectS3_FallsBackToRowUpdatedAt proves the PUT envelope never
// carries the year-1 zero time: when the backend reports no time, lastModified
// is the persisted row's updated_at.
func TestUploadObjectS3_FallsBackToRowUpdatedAt(t *testing.T) {
	repo := newFakeRepo()
	if _, err := repo.CreateBucket(t.Context(), repos.NewBucketInput{
		ProjectID: 1, Name: "reports", DisplayName: "reports", BucketType: "local",
	}); err != nil {
		t.Fatalf("seed CreateBucket: %v", err)
	}
	h := artifacts.NewHandler(repo, timelessPutStore{newFakeStore()})

	rr := serveS3(h, s3PutRequest("/artifacts/s3/reports/a.txt?project_id=1", "text/plain", []byte("x")))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	body := decodeS3JSON(t, rr)
	got, ok := body["lastModified"].(string)
	if !ok {
		t.Fatalf("lastModified missing; body=%v", body)
	}
	lm, err := time.Parse(time.RFC3339Nano, got)
	if err != nil || !lm.Equal(repo.lastUpserted.UpdatedAt) {
		t.Errorf("lastModified = %q (%v), want the row's updated_at %v", got, err, repo.lastUpserted.UpdatedAt)
	}
	if body["key"] != "a.txt" || body["size"] != float64(1) {
		t.Errorf("envelope = %v, want key a.txt of size 1", body)
	}
}
