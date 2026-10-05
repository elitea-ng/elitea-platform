package artifacts_test

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/artifacts"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

func newObjectTestRouter(h *artifacts.Handler) chi.Router {
	r := chi.NewRouter()
	r.Get("/objects/{projectID}/{bucket}", h.ListObjects)
	r.Post("/objects/{projectID}/{bucket}", h.UploadObject)
	r.Post("/objects/{projectID}/{bucket}:batchDelete", h.BatchDeleteObjects)
	r.Get("/objects/{projectID}/{bucket}/*", h.DownloadObject)
	r.Head("/objects/{projectID}/{bucket}/*", h.StatObject)
	r.Delete("/objects/{projectID}/{bucket}/*", h.DeleteObject)
	return r
}

func newObjectTestHandler(t *testing.T) (*artifacts.Handler, *fakeRepo, *fakeStore) {
	t.Helper()
	repo := newFakeRepo()
	if _, err := repo.CreateBucket(t.Context(), repos.NewBucketInput{
		ProjectID: 1, Name: "reports", DisplayName: "reports", BucketType: "local",
	}); err != nil {
		t.Fatalf("seed CreateBucket: %v", err)
	}
	store := newFakeStore()
	return artifacts.NewHandler(repo, store), repo, store
}

func newUploadRequest(t *testing.T, path, filename string, content []byte) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := part.Write(content); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req
}

func TestListObjects_Empty(t *testing.T) {
	h, _, _ := newObjectTestHandler(t)
	r := newObjectTestRouter(h)

	req := httptest.NewRequest(http.MethodGet, "/objects/1/reports", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Objects        []map[string]any `json:"objects"`
		CommonPrefixes []string         `json:"common_prefixes"`
	}
	decodeJSON(t, rr.Body, &resp)
	if resp.Objects == nil || len(resp.Objects) != 0 {
		t.Errorf("expected empty objects array, got %v", resp.Objects)
	}
	if resp.CommonPrefixes == nil || len(resp.CommonPrefixes) != 0 {
		t.Errorf("expected empty common_prefixes array, got %v", resp.CommonPrefixes)
	}
}

func TestListObjects_BucketNotFound(t *testing.T) {
	h, _, _ := newObjectTestHandler(t)
	r := newObjectTestRouter(h)

	req := httptest.NewRequest(http.MethodGet, "/objects/1/missing", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestListObjects_ReturnsSeededObjects(t *testing.T) {
	h, _, store := newObjectTestHandler(t)
	store.seed("1", "reports", "a.png", 10)
	store.seed("1", "reports", "b.png", 20)
	r := newObjectTestRouter(h)

	req := httptest.NewRequest(http.MethodGet, "/objects/1/reports", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Objects []struct {
			Key       string `json:"key"`
			SizeBytes int64  `json:"size_bytes"`
		} `json:"objects"`
	}
	decodeJSON(t, rr.Body, &resp)
	if len(resp.Objects) != 2 {
		t.Fatalf("expected 2 objects, got %d", len(resp.Objects))
	}
}

func TestUploadObject_ReturnsFullShape(t *testing.T) {
	h, _, _ := newObjectTestHandler(t)
	r := newObjectTestRouter(h)

	req := newUploadRequest(t, "/objects/1/reports", "photo.png", []byte("hello world"))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var raw map[string]any
	decodeJSON(t, bytes.NewBuffer(rr.Body.Bytes()), &raw)
	for _, key := range []string{"key", "size_bytes", "media_type", "etag", "created_at"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("response missing key %q: %v", key, raw)
		}
	}
	if raw["key"] != "photo.png" {
		t.Errorf("expected key %q, got %v", "photo.png", raw["key"])
	}
	if size, ok := raw["size_bytes"].(float64); !ok || int64(size) != int64(len("hello world")) {
		t.Errorf("expected size_bytes %d, got %v", len("hello world"), raw["size_bytes"])
	}
}

func TestUploadObject_BucketNotFound(t *testing.T) {
	h, _, _ := newObjectTestHandler(t)
	r := newObjectTestRouter(h)

	req := newUploadRequest(t, "/objects/1/missing", "photo.png", []byte("x"))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestUploadObject_MissingFileFieldRejected(t *testing.T) {
	h, _, _ := newObjectTestHandler(t)
	r := newObjectTestRouter(h)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("not_file", "x"); err != nil {
		t.Fatalf("WriteField: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/objects/1/reports", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestUploadObject_DuplicateWithoutOverwriteRejected(t *testing.T) {
	h, _, _ := newObjectTestHandler(t)
	r := newObjectTestRouter(h)

	req := newUploadRequest(t, "/objects/1/reports", "photo.png", []byte("first"))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("first upload: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}

	req = newUploadRequest(t, "/objects/1/reports", "photo.png", []byte("second"))
	rr = httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("duplicate upload: expected 409, got %d: %s", rr.Code, rr.Body.String())
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

func TestUploadObject_OverwriteTrueReplaces(t *testing.T) {
	h, _, _ := newObjectTestHandler(t)
	r := newObjectTestRouter(h)

	req := newUploadRequest(t, "/objects/1/reports", "photo.png", []byte("first"))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("first upload: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}

	req = newUploadRequest(t, "/objects/1/reports?overwrite=true", "photo.png", []byte("second-longer"))
	rr = httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("overwrite upload: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestUploadObject_DoesNotSpillToTempDir(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("TMPDIR", tmpDir)

	h, _, _ := newObjectTestHandler(t)
	r := newObjectTestRouter(h)

	content := bytes.Repeat([]byte("A"), 40<<20) // 40 MiB
	req := newUploadRequest(t, "/objects/1/reports", "big.bin", content)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rr.Code, rr.Body.String())
	}

	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("ReadDir(TMPDIR): %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("expected TMPDIR to stay empty after a 40 MiB upload, found %d entries: %v", len(entries), names)
	}
}

// TestUploadObject_StreamsWithoutBuffering drives the request body from an
// io.Pipe and proves the handler starts reading from it (via the fake
// store's Put) before the client finishes writing — i.e. it streams the
// multipart part straight through rather than buffering the whole body
// first. See docs/plans/storage-migration-plan.md S9.
func TestUploadObject_StreamsWithoutBuffering(t *testing.T) {
	h, _, store := newObjectTestHandler(t)
	r := newObjectTestRouter(h)

	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	finalWriteAt := make(chan time.Time, 1)

	go func() {
		part, err := mw.CreateFormFile("file", "big.bin")
		if err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		chunk := bytes.Repeat([]byte("A"), 1<<20) // 1 MiB
		const chunks = 20
		for i := 0; i < chunks; i++ {
			if _, err := part.Write(chunk); err != nil {
				_ = pw.CloseWithError(err)
				return
			}
			if i == chunks-1 {
				finalWriteAt <- time.Now()
			}
			time.Sleep(time.Millisecond)
		}
		_ = mw.Close()
		_ = pw.Close()
	}()

	req := httptest.NewRequest(http.MethodPost, "/objects/1/reports", pr)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	writeTime := <-finalWriteAt

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	if store.firstPutReadAt.IsZero() {
		t.Fatal("expected the fake store's Put to have read from the body")
	}
	if !store.firstPutReadAt.Before(writeTime) {
		t.Errorf("expected the store's first read (%v) to precede the client's final write (%v) — "+
			"the body was buffered before streaming to the store", store.firstPutReadAt, writeTime)
	}
}

func TestBatchDeleteObjects_EmptyKeysRejected(t *testing.T) {
	h, _, _ := newObjectTestHandler(t)
	r := newObjectTestRouter(h)

	body := bytes.NewBufferString(`{"keys":[]}`)
	req := httptest.NewRequest(http.MethodPost, "/objects/1/reports:batchDelete", body)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	decodeJSON(t, rr.Body, &resp)
	if resp.Error.Code != "InvalidArgument" {
		t.Errorf("expected code InvalidArgument, got %q", resp.Error.Code)
	}
}

func TestBatchDeleteObjects_DeletesAndReportsInvalidKeys(t *testing.T) {
	h, _, store := newObjectTestHandler(t)
	store.seed("1", "reports", "a.png", 10)
	store.seed("1", "reports", "b.png", 20)
	r := newObjectTestRouter(h)

	body := bytes.NewBufferString(`{"keys":["a.png","b.png","../escape.png"]}`)
	req := httptest.NewRequest(http.MethodPost, "/objects/1/reports:batchDelete", body)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Deleted []string `json:"deleted"`
		Failed  []struct {
			Key  string `json:"key"`
			Code string `json:"code"`
		} `json:"failed"`
	}
	decodeJSON(t, rr.Body, &resp)
	if len(resp.Deleted) != 2 {
		t.Errorf("expected 2 deleted keys, got %v", resp.Deleted)
	}
	if len(resp.Failed) != 1 || resp.Failed[0].Code != "InvalidKey" {
		t.Errorf("expected 1 InvalidKey failure, got %v", resp.Failed)
	}
	if store.objectCount() != 0 {
		t.Errorf("expected both seeded objects deleted, %d remain", store.objectCount())
	}
}

func TestDownloadObject_FullAndRange(t *testing.T) {
	h, _, _ := newObjectTestHandler(t)
	r := newObjectTestRouter(h)

	uploadReq := newUploadRequest(t, "/objects/1/reports", "hello.txt", []byte("0123456789"))
	uploadRR := httptest.NewRecorder()
	r.ServeHTTP(uploadRR, uploadReq)
	if uploadRR.Code != http.StatusCreated {
		t.Fatalf("seed upload: expected 201, got %d: %s", uploadRR.Code, uploadRR.Body.String())
	}

	fullReq := httptest.NewRequest(http.MethodGet, "/objects/1/reports/hello.txt", nil)
	fullRR := httptest.NewRecorder()
	r.ServeHTTP(fullRR, fullReq)
	if fullRR.Code != http.StatusOK {
		t.Fatalf("full download: expected 200, got %d: %s", fullRR.Code, fullRR.Body.String())
	}
	if fullRR.Body.String() != "0123456789" {
		t.Errorf("expected full body %q, got %q", "0123456789", fullRR.Body.String())
	}

	if got := fullRR.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Errorf("full download Accept-Ranges = %q, want %q", got, "bytes")
	}

	// DEFECT: the 206 branch set Content-Length and nothing else. RFC 7233
	// makes Content-Range mandatory on a 206, and without it a browser media
	// element cannot map the bytes into the timeline and aborts playback;
	// `curl -C -` and every resumable downloader fail the same way. The old
	// fake store ignored the range argument and returned the whole object,
	// so this could not be seen: the body, the Content-Length and the status
	// all looked right.
	rangeReq := httptest.NewRequest(http.MethodGet, "/objects/1/reports/hello.txt", nil)
	rangeReq.Header.Set("Range", "bytes=2-4")
	rangeRR := httptest.NewRecorder()
	r.ServeHTTP(rangeRR, rangeReq)
	if rangeRR.Code != http.StatusPartialContent {
		t.Fatalf("range download: expected 206, got %d: %s", rangeRR.Code, rangeRR.Body.String())
	}
	if got := rangeRR.Header().Get("Content-Range"); got != "bytes 2-4/10" {
		t.Errorf("range download Content-Range = %q, want %q", got, "bytes 2-4/10")
	}
	if got := rangeRR.Body.String(); got != "234" {
		t.Errorf("range download body = %q, want %q", got, "234")
	}
	// The declared length must match the bytes actually written. On GCS the
	// backend reported the WHOLE object size for a ranged read, so net/http
	// saw a short write and killed the connection: every ranged download
	// ended in an unexpected EOF.
	if got := rangeRR.Header().Get("Content-Length"); got != "3" {
		t.Errorf("range download Content-Length = %q, want %q", got, "3")
	}

	// An open-ended range reaches the end of the object.
	openReq := httptest.NewRequest(http.MethodGet, "/objects/1/reports/hello.txt", nil)
	openReq.Header.Set("Range", "bytes=7-")
	openRR := httptest.NewRecorder()
	r.ServeHTTP(openRR, openReq)
	if openRR.Code != http.StatusPartialContent {
		t.Fatalf("open range download: expected 206, got %d: %s", openRR.Code, openRR.Body.String())
	}
	if got := openRR.Header().Get("Content-Range"); got != "bytes 7-9/10" {
		t.Errorf("open range Content-Range = %q, want %q", got, "bytes 7-9/10")
	}
	if got := openRR.Body.String(); got != "789" {
		t.Errorf("open range body = %q, want %q", got, "789")
	}

	// A range that starts past the end is unsatisfiable. RFC 7233 answers
	// 416 and states the current length.
	pastReq := httptest.NewRequest(http.MethodGet, "/objects/1/reports/hello.txt", nil)
	pastReq.Header.Set("Range", "bytes=99-")
	pastRR := httptest.NewRecorder()
	r.ServeHTTP(pastRR, pastReq)
	if pastRR.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("past-end range: expected 416, got %d: %s", pastRR.Code, pastRR.Body.String())
	}
	if got := pastRR.Header().Get("Content-Range"); got != "bytes */10" {
		t.Errorf("past-end range Content-Range = %q, want %q", got, "bytes */10")
	}
}

// TestDownloadObject_StatesItsOwnCacheControl: an object download is an
// authenticated, per-user answer, and the handler says so itself — on the
// full body and on a 206 alike. Before, streamObject set no Cache-Control at
// all and the artifact routes' NoStore middleware supplied it, while that
// mount's comment claimed downloads choose their own value. Stating it here
// keeps the contract on every mount of the handler (the S3-compatible plane
// streams through the same function) and makes the router comment true.
func TestDownloadObject_StatesItsOwnCacheControl(t *testing.T) {
	h, _, _ := newObjectTestHandler(t)
	r := newObjectTestRouter(h)

	uploadRR := httptest.NewRecorder()
	r.ServeHTTP(uploadRR, newUploadRequest(t, "/objects/1/reports", "cache.txt", []byte("0123456789")))
	if uploadRR.Code != http.StatusCreated {
		t.Fatalf("seed upload: expected 201, got %d: %s", uploadRR.Code, uploadRR.Body.String())
	}

	for _, rangeHeader := range []string{"", "bytes=2-5"} {
		req := httptest.NewRequest(http.MethodGet, "/objects/1/reports/cache.txt", nil)
		if rangeHeader != "" {
			req.Header.Set("Range", rangeHeader)
		}
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK && rr.Code != http.StatusPartialContent {
			t.Fatalf("download (Range %q): got %d: %s", rangeHeader, rr.Code, rr.Body.String())
		}
		if got := rr.Header().Get("Cache-Control"); got != "private, no-store" {
			t.Errorf("download (Range %q) Cache-Control = %q, want %q", rangeHeader, got, "private, no-store")
		}
	}
}

func TestDownloadObject_NotFound(t *testing.T) {
	h, _, _ := newObjectTestHandler(t)
	r := newObjectTestRouter(h)

	req := httptest.NewRequest(http.MethodGet, "/objects/1/reports/missing.txt", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestDownloadObject_DotDotKeyRejected(t *testing.T) {
	h, _, _ := newObjectTestHandler(t)
	r := newObjectTestRouter(h)

	req := httptest.NewRequest(http.MethodGet, "/objects/1/reports/../escape.txt", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	decodeJSON(t, rr.Body, &resp)
	if resp.Error.Code != "InvalidKey" {
		t.Errorf("expected code InvalidKey, got %q", resp.Error.Code)
	}
}

func TestStatObject_HeadersNoBody(t *testing.T) {
	h, _, _ := newObjectTestHandler(t)
	r := newObjectTestRouter(h)

	uploadReq := newUploadRequest(t, "/objects/1/reports", "hello.txt", []byte("0123456789"))
	uploadRR := httptest.NewRecorder()
	r.ServeHTTP(uploadRR, uploadReq)
	if uploadRR.Code != http.StatusCreated {
		t.Fatalf("seed upload: expected 201, got %d: %s", uploadRR.Code, uploadRR.Body.String())
	}

	req := httptest.NewRequest(http.MethodHead, "/objects/1/reports/hello.txt", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if rr.Body.Len() != 0 {
		t.Errorf("expected empty body for HEAD, got %q", rr.Body.String())
	}
	if rr.Header().Get("Content-Length") != "10" {
		t.Errorf("expected Content-Length 10, got %q", rr.Header().Get("Content-Length"))
	}
}

func TestStatObject_NotFound(t *testing.T) {
	h, _, _ := newObjectTestHandler(t)
	r := newObjectTestRouter(h)

	req := httptest.NewRequest(http.MethodHead, "/objects/1/reports/missing.txt", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rr.Code)
	}
	if rr.Body.Len() != 0 {
		t.Errorf("expected empty body for HEAD, got %q", rr.Body.String())
	}
}

func TestDeleteObject_NoContent(t *testing.T) {
	h, _, store := newObjectTestHandler(t)
	store.seed("1", "reports", "a.png", 10)
	r := newObjectTestRouter(h)

	req := httptest.NewRequest(http.MethodDelete, "/objects/1/reports/a.png", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rr.Code, rr.Body.String())
	}
	if rr.Body.Len() != 0 {
		t.Errorf("expected empty body, got %q", rr.Body.String())
	}
	if store.objectCount() != 0 {
		t.Errorf("expected object deleted, %d remain", store.objectCount())
	}
}

func TestDeleteObject_NotFound(t *testing.T) {
	h, _, _ := newObjectTestHandler(t)
	r := newObjectTestRouter(h)

	req := httptest.NewRequest(http.MethodDelete, "/objects/1/reports/missing.png", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestObjectKeyWithSlash_RoundTripsThroughRealRouter proves a multi-segment
// key resolves through the mounted chi wildcard route, not just against the
// fake store directly — the case the "/*" pattern (S7/S9) exists to cover.
// chi v5.1.0 has no {name...} syntax; a literal {key} would 404 the instant
// the key contains an internal "/".
func TestObjectKeyWithSlash_RoundTripsThroughRealRouter(t *testing.T) {
	h, _, _ := newObjectTestHandler(t)
	r := newObjectTestRouter(h)

	uploadReq := newUploadRequest(t, "/objects/1/reports", "a/b/c.png", []byte("nested"))
	uploadRR := httptest.NewRecorder()
	r.ServeHTTP(uploadRR, uploadReq)
	if uploadRR.Code != http.StatusCreated {
		t.Fatalf("upload: expected 201, got %d: %s", uploadRR.Code, uploadRR.Body.String())
	}
	var uploadResp struct {
		Key string `json:"key"`
	}
	decodeJSON(t, bytes.NewBuffer(uploadRR.Body.Bytes()), &uploadResp)
	if uploadResp.Key != "a/b/c.png" {
		t.Fatalf("expected uploaded key %q, got %q", "a/b/c.png", uploadResp.Key)
	}

	getReq := httptest.NewRequest(http.MethodGet, "/objects/1/reports/a/b/c.png", nil)
	getRR := httptest.NewRecorder()
	r.ServeHTTP(getRR, getReq)
	if getRR.Code != http.StatusOK {
		t.Fatalf("GET: expected 200, got %d: %s", getRR.Code, getRR.Body.String())
	}
	if getRR.Body.String() != "nested" {
		t.Errorf("expected body %q, got %q", "nested", getRR.Body.String())
	}

	headReq := httptest.NewRequest(http.MethodHead, "/objects/1/reports/a/b/c.png", nil)
	headRR := httptest.NewRecorder()
	r.ServeHTTP(headRR, headReq)
	if headRR.Code != http.StatusOK {
		t.Fatalf("HEAD: expected 200, got %d", headRR.Code)
	}

	deleteReq := httptest.NewRequest(http.MethodDelete, "/objects/1/reports/a/b/c.png", nil)
	deleteRR := httptest.NewRecorder()
	r.ServeHTTP(deleteRR, deleteReq)
	if deleteRR.Code != http.StatusNoContent {
		t.Fatalf("DELETE: expected 204, got %d: %s", deleteRR.Code, deleteRR.Body.String())
	}

	confirmReq := httptest.NewRequest(http.MethodHead, "/objects/1/reports/a/b/c.png", nil)
	confirmRR := httptest.NewRecorder()
	r.ServeHTTP(confirmRR, confirmReq)
	if confirmRR.Code != http.StatusNotFound {
		t.Fatalf("expected the deleted key to 404, got %d", confirmRR.Code)
	}
}
