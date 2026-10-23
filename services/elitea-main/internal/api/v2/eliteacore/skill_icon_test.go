package eliteacore_test

// Handler-level coverage for the skill icon route family.
//
// NO CASE HERE ASSERTS A STATUS CODE ALONE. The defect this family already
// shipped once — in the project-icon handler beside it — was a 200 that
// persisted nothing and answered with a fabricated URL, and a status assertion
// passes against exactly that. Every write below is read back: through the
// LISTING the picker calls, and through DownloadIcon, the public route the
// browser's <img src> actually fetches.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
)

// listableIconStore is fakeIconObjectStore plus a real List. The existing fake
// answers ErrNotSupported for List, which is a legitimate backend shape the
// listing handler tolerates — and therefore exactly the fake that CANNOT tell a
// working gallery from an empty one.
type listableIconStore struct {
	mu      sync.Mutex
	objects map[string][]byte
	// listErr, when set, is returned by List instead of a page.
	listErr error
}

func newListableIconStore() *listableIconStore {
	return &listableIconStore{objects: map[string][]byte{}}
}

func (f *listableIconStore) key(ref storage.ObjectRef) string {
	return ref.ProjectID() + "/" + ref.Bucket() + "/" + ref.Key()
}

func (f *listableIconStore) Put(_ context.Context, ref storage.ObjectRef, body io.Reader, _ storage.PutOptions) (storage.ObjectInfo, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	f.mu.Lock()
	f.objects[f.key(ref)] = data
	f.mu.Unlock()
	return storage.ObjectInfo{Key: ref.Key(), Size: int64(len(data))}, nil
}

func (f *listableIconStore) Get(_ context.Context, ref storage.ObjectRef, _ *storage.ByteRange) (io.ReadCloser, storage.ObjectInfo, error) {
	f.mu.Lock()
	data, ok := f.objects[f.key(ref)]
	f.mu.Unlock()
	if !ok {
		return nil, storage.ObjectInfo{}, storage.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), storage.ObjectInfo{Key: ref.Key(), Size: int64(len(data))}, nil
}

func (f *listableIconStore) Delete(_ context.Context, ref storage.ObjectRef) error {
	f.mu.Lock()
	delete(f.objects, f.key(ref))
	f.mu.Unlock()
	return nil
}

func (f *listableIconStore) List(_ context.Context, q storage.ListQuery) (storage.ListPage, error) {
	if f.listErr != nil {
		return storage.ListPage{}, f.listErr
	}
	prefix := q.Bucket.ProjectID() + "/" + q.Bucket.Bucket() + "/"
	page := storage.ListPage{}
	f.mu.Lock()
	keys := make([]string, 0, len(f.objects))
	for key := range f.objects {
		keys = append(keys, key)
	}
	f.mu.Unlock()
	sort.Strings(keys)
	for _, key := range keys {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		name := strings.TrimPrefix(key, prefix)
		if !strings.HasPrefix(name, q.KeyPrefix) {
			continue
		}
		page.Objects = append(page.Objects, storage.ObjectInfo{Key: name})
	}
	return page, nil
}

func (f *listableIconStore) DeleteBatch(context.Context, []storage.ObjectRef) (storage.BatchResult, error) {
	return storage.BatchResult{}, storage.ErrNotSupported
}
func (f *listableIconStore) Stat(context.Context, storage.ObjectRef) (storage.ObjectInfo, error) {
	return storage.ObjectInfo{}, storage.ErrNotSupported
}
func (f *listableIconStore) PresignGet(context.Context, storage.ObjectRef, time.Duration) (string, error) {
	return "", storage.ErrNotSupported
}
func (f *listableIconStore) PresignPut(context.Context, storage.ObjectRef, time.Duration, storage.PutOptions) (string, error) {
	return "", storage.ErrNotSupported
}
func (f *listableIconStore) StartMultipart(context.Context, storage.ObjectRef, storage.PutOptions) (storage.UploadID, error) {
	return "", storage.ErrNotSupported
}
func (f *listableIconStore) PresignPart(context.Context, storage.ObjectRef, storage.UploadID, int32, time.Duration) (string, error) {
	return "", storage.ErrNotSupported
}
func (f *listableIconStore) CompleteMultipart(context.Context, storage.ObjectRef, storage.UploadID, []storage.Part) (storage.ObjectInfo, error) {
	return storage.ObjectInfo{}, storage.ErrNotSupported
}
func (f *listableIconStore) AbortMultipart(context.Context, storage.ObjectRef, storage.UploadID) error {
	return storage.ErrNotSupported
}
func (f *listableIconStore) Capabilities() storage.Capabilities { return storage.Capabilities{} }

var _ storage.ObjectStore = (*listableIconStore)(nil)

func skillIconUploadRequest(t *testing.T, projectID, filename string, content []byte, fields map[string]string) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := part.Write(content); err != nil {
		t.Fatalf("write file content: %v", err)
	}
	for name, value := range fields {
		if err := writer.WriteField(name, value); err != nil {
			t.Fatalf("write field %s: %v", name, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	req := newRequest(http.MethodPost, "/", map[string]string{"projectID": projectID}, &buf)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	return req
}

// setURLParam adds one more chi URL param to a request newRequest already
// built — the upload route carries an OPTIONAL trailing version id, so its
// request is the same one with one more segment bound.
func setURLParam(req *http.Request, name, value string) {
	if rctx := chi.RouteContext(req.Context()); rctx != nil {
		rctx.URLParams.Add(name, value)
	}
}

// uploadOneSkillIcon uploads an icon and returns its icon_meta.
func uploadOneSkillIcon(t *testing.T, h *eliteacore.Handler, projectID, filename string, content []byte) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	h.UploadSkillIcon(rec, skillIconUploadRequest(t, projectID, filename, content, nil))
	assertStatus(t, rec, http.StatusOK)
	return decodeObj(t, rec)
}

func TestSkillIconUploadIsReadableThroughTheListingAndTheDownloadRoute(t *testing.T) {
	store := newListableIconStore()
	h := eliteacore.NewHandler(nil, eliteacore.WithObjectStore(store))

	content := []byte("PNG-ish bytes")
	meta := uploadOneSkillIcon(t, h, "7", "my-icon.png", content)

	name, _ := meta["name"].(string)
	iconURL, _ := meta["url"].(string)
	if !strings.HasPrefix(name, "skill_") || !strings.HasSuffix(name, ".png") {
		t.Fatalf("stored name %q is not a prefixed skill icon", name)
	}
	if iconURL != "/icons/7/"+name {
		t.Fatalf("url = %q, want /icons/7/%s", iconURL, name)
	}

	// (1) The gallery route the picker calls must SEE it. `rows`/`total` is
	// the only shape the client reads; `items` here would be a green 200 in
	// front of an empty dialog.
	listRec := httptest.NewRecorder()
	h.ListSkillIcons(listRec, newRequest(http.MethodGet, "/", map[string]string{"projectID": "7"}, nil))
	assertStatus(t, listRec, http.StatusOK)
	listed := decodeObj(t, listRec)
	if _, hasItems := listed["items"]; hasItems {
		t.Fatalf("listing answered `items`; the client reads `rows`: %v", listed)
	}
	if total, _ := listed["total"].(float64); total != 1 {
		t.Fatalf("total = %v, want 1", listed["total"])
	}
	rows, ok := listed["rows"].([]any)
	if !ok || len(rows) != 1 {
		t.Fatalf("rows = %v, want exactly the uploaded icon", listed["rows"])
	}
	row, _ := rows[0].(map[string]any)
	if row["name"] != name || row["url"] != iconURL {
		t.Fatalf("row = %v, want name=%q url=%q", row, name, iconURL)
	}

	// (2) The bytes must come back through the PUBLIC route the browser uses.
	downloadRec := httptest.NewRecorder()
	eliteacore.DownloadIcon(store).ServeHTTP(downloadRec,
		newRequest(http.MethodGet, "/", map[string]string{"projectID": "7", "filename": name}, nil))
	if downloadRec.Code != http.StatusOK {
		t.Fatalf("download status = %d, want 200", downloadRec.Code)
	}
	if got := downloadRec.Body.String(); got != string(content) {
		t.Fatalf("download body = %q, want %q", got, content)
	}
}

func TestSkillIconListingIsScopedToSkillIconsAndToTheProject(t *testing.T) {
	store := newListableIconStore()
	h := eliteacore.NewHandler(nil, eliteacore.WithObjectStore(store))

	skillIcon := uploadOneSkillIcon(t, h, "7", "skill.png", []byte("a"))
	// An AGENT icon in the same project lands in the same bucket without the
	// prefix. It must not appear in the skill gallery — that separation is the
	// entire reason the prefix exists.
	agentRec := httptest.NewRecorder()
	h.UploadIcon(agentRec, iconUploadRequest(t, "7", "agent.png", []byte("b")))
	assertStatus(t, agentRec, http.StatusOK)
	// A skill icon in a DIFFERENT project must not appear either.
	uploadOneSkillIcon(t, h, "8", "other.png", []byte("c"))

	listRec := httptest.NewRecorder()
	h.ListSkillIcons(listRec, newRequest(http.MethodGet, "/", map[string]string{"projectID": "7"}, nil))
	assertStatus(t, listRec, http.StatusOK)
	listed := decodeObj(t, listRec)

	rows, _ := listed["rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("rows = %v, want only project 7's skill icon", listed["rows"])
	}
	if row, _ := rows[0].(map[string]any); row["name"] != skillIcon["name"] {
		t.Fatalf("row = %v, want %v", row, skillIcon["name"])
	}
}

func TestSkillIconListingPaginatesLikeTheLegacySlice(t *testing.T) {
	store := newListableIconStore()
	h := eliteacore.NewHandler(nil, eliteacore.WithObjectStore(store))
	for range 3 {
		uploadOneSkillIcon(t, h, "7", "icon.png", []byte("x"))
	}

	for _, testCase := range []struct {
		query     string
		wantRows  int
		wantTotal float64
	}{
		{"", 3, 3},
		{"?limit=2", 2, 3},
		{"?skip=2&limit=2", 1, 3},
		{"?skip=99", 0, 3},
		{"?limit=notanumber", 3, 3}, // _safe_int: a junk value is the default, not a 500
	} {
		rec := httptest.NewRecorder()
		h.ListSkillIcons(rec, newRequest(http.MethodGet, "/"+testCase.query, map[string]string{"projectID": "7"}, nil))
		assertStatus(t, rec, http.StatusOK)
		listed := decodeObj(t, rec)
		rows, _ := listed["rows"].([]any)
		if len(rows) != testCase.wantRows {
			t.Errorf("%q: rows = %d, want %d", testCase.query, len(rows), testCase.wantRows)
		}
		if total, _ := listed["total"].(float64); total != testCase.wantTotal {
			t.Errorf("%q: total = %v, want %v", testCase.query, listed["total"], testCase.wantTotal)
		}
	}
}

func TestSkillIconUploadRefusesARequestWithNoFile(t *testing.T) {
	store := newListableIconStore()
	h := eliteacore.NewHandler(nil, eliteacore.WithObjectStore(store))

	rec := httptest.NewRecorder()
	req := newRequest(http.MethodPost, "/", map[string]string{"projectID": "7"}, strings.NewReader(""))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=zzz")
	h.UploadSkillIcon(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 — a 200 for an upload that stored nothing is the defect this family already shipped", rec.Code)
	}
	listRec := httptest.NewRecorder()
	h.ListSkillIcons(listRec, newRequest(http.MethodGet, "/", map[string]string{"projectID": "7"}, nil))
	if total, _ := decodeObj(t, listRec)["total"].(float64); total != 0 {
		t.Fatalf("total = %v, want 0", total)
	}
}

func TestSkillIconUploadRefusesAnOversizeFileAndStoresNothing(t *testing.T) {
	store := newListableIconStore()
	h := eliteacore.NewHandler(nil, eliteacore.WithObjectStore(store))

	oversize := bytes.Repeat([]byte("x"), 512*1024+64)
	rec := httptest.NewRecorder()
	h.UploadSkillIcon(rec, skillIconUploadRequest(t, "7", "big.png", oversize, nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}

	listRec := httptest.NewRecorder()
	h.ListSkillIcons(listRec, newRequest(http.MethodGet, "/", map[string]string{"projectID": "7"}, nil))
	assertStatus(t, listRec, http.StatusOK)
	if total, _ := decodeObj(t, listRec)["total"].(float64); total != 0 {
		t.Fatalf("an over-cap upload left an object behind: total = %v", total)
	}
}

func TestSkillIconUploadStoresOnlyAllowlistedExtensions(t *testing.T) {
	store := newListableIconStore()
	h := eliteacore.NewHandler(nil, eliteacore.WithObjectStore(store))

	// The stored-XSS boundary the agent family already established: an .html
	// part is stored as .png, so the public download route can never serve it
	// as script in the app's own origin.
	meta := uploadOneSkillIcon(t, h, "7", "payload.html", []byte("<script>alert(1)</script>"))
	if name, _ := meta["name"].(string); !strings.HasSuffix(name, ".png") {
		t.Fatalf("stored name = %v, want the .png fallback", meta["name"])
	}
}

func TestSkillIconUploadDeclaresTheClampedBox(t *testing.T) {
	store := newListableIconStore()
	h := eliteacore.NewHandler(nil, eliteacore.WithObjectStore(store))

	rec := httptest.NewRecorder()
	h.UploadSkillIcon(rec, skillIconUploadRequest(t, "7", "icon.png", []byte("x"),
		map[string]string{"width": "512", "height": "0"}))
	assertStatus(t, rec, http.StatusOK)
	if size := decodeObj(t, rec)["size"]; size != "64x1" {
		t.Fatalf("size = %v, want the clamped 64x1", size)
	}
}

func TestSkillIconUpdateRefusesAPayloadTheReadPathCannotRender(t *testing.T) {
	h := eliteacore.NewHandler(nil)

	for _, body := range []string{`{}`, `{"name":"a"}`, `{"url":"/icons/7/a"}`, `not json`} {
		rec := httptest.NewRecorder()
		h.UpdateSkillIcon(rec, newRequest(http.MethodPut, "/",
			map[string]string{"projectID": "7", "versionId": "1"}, strings.NewReader(body)))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %s: status = %d, want 400", body, rec.Code)
		}
	}
}

func TestSkillIconDeleteRefusesATraversingName(t *testing.T) {
	store := newListableIconStore()
	h := eliteacore.NewHandler(nil, eliteacore.WithObjectStore(store))

	rec := httptest.NewRecorder()
	h.DeleteSkillIcon(rec, newRequest(http.MethodDelete, "/",
		map[string]string{"projectID": "7", "name": "../secrets"}, nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestSkillIconDeleteRemovesTheObjectAndTheListingEntry(t *testing.T) {
	store := newListableIconStore()
	h := eliteacore.NewHandler(nil, eliteacore.WithObjectStore(store))

	meta := uploadOneSkillIcon(t, h, "7", "icon.png", []byte("bytes"))
	name, _ := meta["name"].(string)

	rec := httptest.NewRecorder()
	h.DeleteSkillIcon(rec, newRequest(http.MethodDelete, "/",
		map[string]string{"projectID": "7", "name": name}, nil))
	assertStatus(t, rec, http.StatusOK)

	listRec := httptest.NewRecorder()
	h.ListSkillIcons(listRec, newRequest(http.MethodGet, "/", map[string]string{"projectID": "7"}, nil))
	if total, _ := decodeObj(t, listRec)["total"].(float64); total != 0 {
		t.Fatalf("deleted icon is still listed: %v", total)
	}

	downloadRec := httptest.NewRecorder()
	eliteacore.DownloadIcon(store).ServeHTTP(downloadRec,
		newRequest(http.MethodGet, "/", map[string]string{"projectID": "7", "filename": name}, nil))
	if downloadRec.Code != http.StatusNotFound {
		t.Fatalf("download after delete = %d, want 404", downloadRec.Code)
	}
}

// TestSkillIconListingSurvivesABackendWithNoListing pins the ONE error branch
// that is deliberately not a 500: a store that cannot list has no uploaded
// icons to show, which is true rather than broken.
func TestSkillIconListingSurvivesABackendWithNoListing(t *testing.T) {
	store := newListableIconStore()
	store.listErr = storage.ErrNotSupported
	h := eliteacore.NewHandler(nil, eliteacore.WithObjectStore(store))

	rec := httptest.NewRecorder()
	h.ListSkillIcons(rec, newRequest(http.MethodGet, "/", map[string]string{"projectID": "7"}, nil))
	assertStatus(t, rec, http.StatusOK)
	listed := decodeObj(t, rec)
	if total, _ := listed["total"].(float64); total != 0 {
		t.Fatalf("total = %v, want 0", total)
	}
	// Never `null`: the client maps over `rows` directly.
	raw, _ := json.Marshal(listed["rows"])
	if string(raw) != "[]" {
		t.Fatalf("rows = %s, want []", raw)
	}
}

func TestSkillIconListingReportsARealStoreFailure(t *testing.T) {
	store := newListableIconStore()
	store.listErr = storage.ErrInvalidKey
	h := eliteacore.NewHandler(nil, eliteacore.WithObjectStore(store))

	rec := httptest.NewRecorder()
	h.ListSkillIcons(rec, newRequest(http.MethodGet, "/", map[string]string{"projectID": "7"}, nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 — a store failure reported as an empty gallery is unreadable", rec.Code)
	}
}
