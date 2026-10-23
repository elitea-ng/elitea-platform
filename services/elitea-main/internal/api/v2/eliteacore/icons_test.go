package eliteacore_test

// S20b: TestArtifactIcon* — UploadIcon/DeleteIcon/DownloadIcon moved off
// ICONS_DATA_DIR onto the object store. Handler-level, backed by an
// in-memory fake storage.ObjectStore (no real infra) — no shared testutil
// exists in this codebase, matching S18/S19/S20a's own per-file fakes.

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
)

// fakeIconObjectStore implements storage.ObjectStore in memory — only Put/
// Get/Delete are exercised by the icon byte path.
type fakeIconObjectStore struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func newFakeIconObjectStore() *fakeIconObjectStore {
	return &fakeIconObjectStore{objects: map[string][]byte{}}
}

func iconStoreKey(ref storage.ObjectRef) string {
	return ref.ProjectID() + "/" + ref.Bucket() + "/" + ref.Key()
}

func (f *fakeIconObjectStore) Put(_ context.Context, ref storage.ObjectRef, body io.Reader, _ storage.PutOptions) (storage.ObjectInfo, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	f.mu.Lock()
	f.objects[iconStoreKey(ref)] = data
	f.mu.Unlock()
	return storage.ObjectInfo{Key: ref.Key(), Size: int64(len(data))}, nil
}

func (f *fakeIconObjectStore) Get(_ context.Context, ref storage.ObjectRef, _ *storage.ByteRange) (io.ReadCloser, storage.ObjectInfo, error) {
	f.mu.Lock()
	data, ok := f.objects[iconStoreKey(ref)]
	f.mu.Unlock()
	if !ok {
		return nil, storage.ObjectInfo{}, storage.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), storage.ObjectInfo{Key: ref.Key(), Size: int64(len(data))}, nil
}

func (f *fakeIconObjectStore) Delete(_ context.Context, ref storage.ObjectRef) error {
	f.mu.Lock()
	delete(f.objects, iconStoreKey(ref))
	f.mu.Unlock()
	return nil
}

func (f *fakeIconObjectStore) DeleteBatch(context.Context, []storage.ObjectRef) (storage.BatchResult, error) {
	return storage.BatchResult{}, storage.ErrNotSupported
}
func (f *fakeIconObjectStore) Stat(context.Context, storage.ObjectRef) (storage.ObjectInfo, error) {
	return storage.ObjectInfo{}, storage.ErrNotSupported
}
func (f *fakeIconObjectStore) List(context.Context, storage.ListQuery) (storage.ListPage, error) {
	return storage.ListPage{}, storage.ErrNotSupported
}
func (f *fakeIconObjectStore) PresignGet(context.Context, storage.ObjectRef, time.Duration) (string, error) {
	return "", storage.ErrNotSupported
}
func (f *fakeIconObjectStore) PresignPut(context.Context, storage.ObjectRef, time.Duration, storage.PutOptions) (string, error) {
	return "", storage.ErrNotSupported
}
func (f *fakeIconObjectStore) StartMultipart(context.Context, storage.ObjectRef, storage.PutOptions) (storage.UploadID, error) {
	return "", storage.ErrNotSupported
}
func (f *fakeIconObjectStore) PresignPart(context.Context, storage.ObjectRef, storage.UploadID, int32, time.Duration) (string, error) {
	return "", storage.ErrNotSupported
}
func (f *fakeIconObjectStore) CompleteMultipart(context.Context, storage.ObjectRef, storage.UploadID, []storage.Part) (storage.ObjectInfo, error) {
	return storage.ObjectInfo{}, storage.ErrNotSupported
}
func (f *fakeIconObjectStore) AbortMultipart(context.Context, storage.ObjectRef, storage.UploadID) error {
	return storage.ErrNotSupported
}
func (f *fakeIconObjectStore) Capabilities() storage.Capabilities { return storage.Capabilities{} }

var _ storage.ObjectStore = (*fakeIconObjectStore)(nil)

func iconUploadRequest(t *testing.T, projectID, filename string, content []byte) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := part.Write(content); err != nil {
		t.Fatalf("write file content: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	req := newRequest(http.MethodPost, "/", map[string]string{"projectID": projectID}, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req
}

func TestArtifactIconUploadStoresBytesAndDownloadServesThem(t *testing.T) {
	store := newFakeIconObjectStore()
	h := eliteacore.NewHandler(nil, eliteacore.WithObjectStore(store))

	content := []byte("<svg>icon</svg>")
	req := iconUploadRequest(t, "1", "icon.svg", content)
	w := httptest.NewRecorder()
	h.UploadIcon(w, req)

	assertStatus(t, w, http.StatusOK)
	body := decodeObj(t, w)
	iconURL, _ := body["url"].(string)
	const prefix = "/icons/1/"
	if !strings.HasPrefix(iconURL, prefix) || !strings.HasSuffix(iconURL, ".svg") {
		t.Fatalf("unexpected icon URL %q", iconURL)
	}
	filename := strings.TrimPrefix(iconURL, prefix)

	downloadReq := newRequest(http.MethodGet, "/", map[string]string{"projectID": "1", "filename": filename}, nil)
	downloadRec := httptest.NewRecorder()
	eliteacore.DownloadIcon(store).ServeHTTP(downloadRec, downloadReq)

	if downloadRec.Code != http.StatusOK {
		t.Fatalf("download status = %d, want 200; body=%s", downloadRec.Code, downloadRec.Body.String())
	}
	if got := downloadRec.Body.String(); got != string(content) {
		t.Fatalf("downloaded body = %q, want %q", got, content)
	}
}

func TestArtifactIconUploadWithoutStoreConfiguredReturns500(t *testing.T) {
	h := eliteacore.NewHandler(nil) // no WithObjectStore

	req := iconUploadRequest(t, "1", "icon.png", []byte("data"))
	w := httptest.NewRecorder()
	h.UploadIcon(w, req)

	assertStatus(t, w, http.StatusInternalServerError)
}

func TestArtifactIconUploadWithNoFileStillSucceedsWithoutStore(t *testing.T) {
	// Matches the pre-S20b contract: an upload request with no file part is
	// a no-op, not an error — even when storage isn't configured, since the
	// early return never touches it.
	h := eliteacore.NewHandler(nil)

	req := newRequest(http.MethodPost, "/", map[string]string{"projectID": "1"}, nil)
	w := httptest.NewRecorder()
	h.UploadIcon(w, req)

	assertStatus(t, w, http.StatusOK)
	body := decodeObj(t, w)
	if ok, _ := body["ok"].(bool); !ok {
		t.Error("ok should be true")
	}
	if url, _ := body["url"].(string); url != "" {
		t.Errorf("url = %q, want empty", url)
	}
}

func TestArtifactIconUploadRejectsInvalidProjectID(t *testing.T) {
	store := newFakeIconObjectStore()
	h := eliteacore.NewHandler(nil, eliteacore.WithObjectStore(store))

	// "abc" passes validIconPathSegment's own (looser, pre-S20b) check —
	// non-empty, no slash/backslash/null byte — but storage.NewObjectRef
	// requires a project ID matching ^[1-9][0-9]{0,17}$, so this exercises
	// the new, stricter numeric-format enforcement S20b adds, not the
	// pre-existing structural check.
	req := iconUploadRequest(t, "abc", "icon.png", []byte("data"))
	w := httptest.NewRecorder()
	h.UploadIcon(w, req)

	assertStatus(t, w, http.StatusBadRequest)
}

func TestArtifactIconDeleteRemovesFromStoreAndIsIdempotent(t *testing.T) {
	store := newFakeIconObjectStore()
	h := eliteacore.NewHandler(nil, eliteacore.WithObjectStore(store))

	uploadReq := iconUploadRequest(t, "1", "icon.png", []byte("data"))
	uploadRec := httptest.NewRecorder()
	h.UploadIcon(uploadRec, uploadReq)
	body := decodeObj(t, uploadRec)
	iconURL, _ := body["url"].(string)
	filename := strings.TrimPrefix(iconURL, "/icons/1/")

	deleteReq := newRequest(http.MethodDelete, "/", map[string]string{"projectID": "1", "name": filename}, nil)
	deleteRec := httptest.NewRecorder()
	h.DeleteIcon(deleteRec, deleteReq)
	assertStatus(t, deleteRec, http.StatusNoContent)

	downloadReq := newRequest(http.MethodGet, "/", map[string]string{"projectID": "1", "filename": filename}, nil)
	downloadRec := httptest.NewRecorder()
	eliteacore.DownloadIcon(store).ServeHTTP(downloadRec, downloadReq)
	if downloadRec.Code != http.StatusNotFound {
		t.Fatalf("download after delete status = %d, want 404", downloadRec.Code)
	}

	// Deleting again (or a never-existed icon) is idempotent, matching this
	// handler's pre-S20b behavior and storage.ObjectStore's own documented
	// Delete semantics.
	deleteAgainRec := httptest.NewRecorder()
	h.DeleteIcon(deleteAgainRec, deleteReq)
	assertStatus(t, deleteAgainRec, http.StatusNoContent)
}

func TestArtifactIconDownloadMissingIconReturns404(t *testing.T) {
	store := newFakeIconObjectStore()
	req := newRequest(http.MethodGet, "/", map[string]string{"projectID": "1", "filename": "never-uploaded.png"}, nil)
	rec := httptest.NewRecorder()
	eliteacore.DownloadIcon(store).ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestArtifactIconDownloadRejectsInvalidProjectID(t *testing.T) {
	store := newFakeIconObjectStore()
	req := newRequest(http.MethodGet, "/", map[string]string{"projectID": "not-numeric", "filename": "icon.png"}, nil)
	rec := httptest.NewRecorder()
	eliteacore.DownloadIcon(store).ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestArtifactIconDownloadWithNilStoreReturns500(t *testing.T) {
	req := newRequest(http.MethodGet, "/", map[string]string{"projectID": "1", "filename": "icon.png"}, nil)
	rec := httptest.NewRecorder()
	eliteacore.DownloadIcon(nil).ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (not a panic)", rec.Code)
	}
}

// TestArtifactIconUploadRejectsNonImageExtensionAndDownloadSetsNosniff
// closes a stored-XSS path an adversarial review found: DownloadIcon is a
// public, unauthenticated route (a browser <img src> carries no auth
// header), so anything it could be made to serve back with an
// attacker-chosen Content-Type is script-executable in the app's own
// origin. Uploading a file named "evil.html" must not let the response be
// served as text/html.
func TestArtifactIconUploadRejectsNonImageExtensionAndDownloadSetsNosniff(t *testing.T) {
	store := newFakeIconObjectStore()
	h := eliteacore.NewHandler(nil, eliteacore.WithObjectStore(store))

	payload := []byte(`<script>alert(document.cookie)</script>`)
	uploadReq := iconUploadRequest(t, "1", "evil.html", payload)
	uploadRec := httptest.NewRecorder()
	h.UploadIcon(uploadRec, uploadReq)
	assertStatus(t, uploadRec, http.StatusOK)

	body := decodeObj(t, uploadRec)
	iconURL, _ := body["url"].(string)
	if strings.HasSuffix(iconURL, ".html") {
		t.Fatalf("icon URL = %q, want a non-.html extension (safeIconExtension should have forced the default)", iconURL)
	}
	filename := strings.TrimPrefix(iconURL, "/icons/1/")

	downloadReq := newRequest(http.MethodGet, "/", map[string]string{"projectID": "1", "filename": filename}, nil)
	downloadRec := httptest.NewRecorder()
	eliteacore.DownloadIcon(store).ServeHTTP(downloadRec, downloadReq)

	assertStatus(t, downloadRec, http.StatusOK)
	if got := downloadRec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if ct := downloadRec.Header().Get("Content-Type"); strings.Contains(ct, "html") {
		t.Fatalf("Content-Type = %q, want a non-HTML type", ct)
	}
}

// An SVG is an ALLOWLISTED icon extension and neither uploader inspects file
// content, so this upload succeeds by design and the stored bytes really do
// carry a <script>. The extension allowlist is therefore NOT what stops this
// one — image/svg+xml is a genuine image type that a browser executes script
// in when the response is rendered as a document, and nosniff only suppresses
// sniffing. What stops it is the response being unrenderable as a document:
// `sandbox` (no tokens) disables scripting in an opaque origin, and
// `Content-Disposition: attachment` makes a direct navigation download it.
//
// Pinned because it is a security control with no other guard: the route is
// public and unauthenticated by design, and a header silently dropped in a
// later edit produces no test failure and no visible symptom.
func TestArtifactIconDownloadNeutralisesAnUploadedSvgCarryingScript(t *testing.T) {
	store := newFakeIconObjectStore()
	h := eliteacore.NewHandler(nil, eliteacore.WithObjectStore(store))

	payload := []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(document.cookie)</script></svg>`)
	uploadRec := httptest.NewRecorder()
	h.UploadIcon(uploadRec, iconUploadRequest(t, "1", "evil.svg", payload))
	assertStatus(t, uploadRec, http.StatusOK)

	iconURL, _ := decodeObj(t, uploadRec)["url"].(string)
	if !strings.HasSuffix(iconURL, ".svg") {
		t.Fatalf("icon URL = %q, want the .svg to be stored as-is — this test is meaningless otherwise", iconURL)
	}
	filename := strings.TrimPrefix(iconURL, "/icons/1/")

	downloadRec := httptest.NewRecorder()
	eliteacore.DownloadIcon(store).ServeHTTP(downloadRec,
		newRequest(http.MethodGet, "/", map[string]string{"projectID": "1", "filename": filename}, nil))
	assertStatus(t, downloadRec, http.StatusOK)

	// The served bytes are the attacker's, and the type is the executable one.
	// Both are stated so that a future reader can see the headers below are
	// carrying the whole defence.
	if !bytes.Contains(downloadRec.Body.Bytes(), []byte("<script>")) {
		t.Fatalf("stored body no longer carries the script; this test no longer covers what it claims")
	}
	if ct := downloadRec.Header().Get("Content-Type"); !strings.Contains(ct, "svg") {
		t.Fatalf("Content-Type = %q, want the image/svg+xml this defence assumes", ct)
	}

	csp := downloadRec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "sandbox") {
		t.Fatalf("Content-Security-Policy = %q, want a sandbox directive — without it a navigated SVG runs script in this origin", csp)
	}
	if !strings.Contains(csp, "default-src 'none'") {
		t.Fatalf("Content-Security-Policy = %q, want default-src 'none'", csp)
	}
	if got := downloadRec.Header().Get("Content-Disposition"); got != "attachment" {
		t.Fatalf("Content-Disposition = %q, want attachment", got)
	}
	if got := downloadRec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
	}
}
