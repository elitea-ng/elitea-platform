package admin_test

// The asset upload route, driven without a database: the handler's own
// refusals (kind, part, size, content) and the path it hands out, over an
// in-memory object store.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/admin"
	v2branding "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/branding"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
)

type memoryObjectStore struct {
	mu       sync.Mutex
	objects  map[string][]byte
	modified map[string]time.Time
}

// age backdates every stored object whose key contains fragment, so a test
// can put an object past the collection grace period (#1057).
func (m *memoryObjectStore) age(fragment string, by time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key := range m.objects {
		if strings.Contains(key, fragment) {
			m.modified[key] = m.modified[key].Add(-by)
		}
	}
}

func (m *memoryObjectStore) Put(_ context.Context, ref storage.ObjectRef, body io.Reader, _ storage.PutOptions) (storage.ObjectInfo, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.objects == nil {
		m.objects = map[string][]byte{}
		m.modified = map[string]time.Time{}
	}
	m.objects[ref.StorageKey("")] = data
	m.modified[ref.StorageKey("")] = time.Now()
	return storage.ObjectInfo{Key: ref.Key(), Size: int64(len(data))}, nil
}
func (m *memoryObjectStore) Get(_ context.Context, ref storage.ObjectRef, _ *storage.ByteRange) (io.ReadCloser, storage.ObjectInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.objects[ref.StorageKey("")]
	if !ok {
		return nil, storage.ObjectInfo{}, storage.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), storage.ObjectInfo{Key: ref.Key(), Size: int64(len(data))}, nil
}
func (m *memoryObjectStore) Delete(_ context.Context, ref storage.ObjectRef) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, ref.StorageKey(""))
	delete(m.modified, ref.StorageKey(""))
	return nil
}
func (m *memoryObjectStore) List(_ context.Context, q storage.ListQuery) (storage.ListPage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var page storage.ListPage
	for key := range m.objects {
		if rest, ok := strings.CutPrefix(key, q.Bucket.BucketPrefix("")); ok {
			page.Objects = append(page.Objects, storage.ObjectInfo{Key: rest, LastModified: m.modified[key]})
		}
	}
	return page, nil
}
func (m *memoryObjectStore) DeleteBatch(context.Context, []storage.ObjectRef) (storage.BatchResult, error) {
	return storage.BatchResult{}, storage.ErrNotSupported
}
func (m *memoryObjectStore) Stat(context.Context, storage.ObjectRef) (storage.ObjectInfo, error) {
	return storage.ObjectInfo{}, storage.ErrNotSupported
}
func (m *memoryObjectStore) PresignGet(context.Context, storage.ObjectRef, time.Duration) (string, error) {
	return "", storage.ErrNotSupported
}
func (m *memoryObjectStore) PresignPut(context.Context, storage.ObjectRef, time.Duration, storage.PutOptions) (string, error) {
	return "", storage.ErrNotSupported
}
func (m *memoryObjectStore) StartMultipart(context.Context, storage.ObjectRef, storage.PutOptions) (storage.UploadID, error) {
	return "", storage.ErrNotSupported
}
func (m *memoryObjectStore) PresignPart(context.Context, storage.ObjectRef, storage.UploadID, int32, time.Duration) (string, error) {
	return "", storage.ErrNotSupported
}
func (m *memoryObjectStore) CompleteMultipart(context.Context, storage.ObjectRef, storage.UploadID, []storage.Part) (storage.ObjectInfo, error) {
	return storage.ObjectInfo{}, storage.ErrNotSupported
}
func (m *memoryObjectStore) AbortMultipart(context.Context, storage.ObjectRef, storage.UploadID) error {
	return storage.ErrNotSupported
}
func (m *memoryObjectStore) Capabilities() storage.Capabilities { return storage.Capabilities{} }

func multipartUpload(t *testing.T, target, filename string, data []byte) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if filename != "" {
		part, err := writer.CreateFormFile("file", filename)
		if err != nil {
			t.Fatalf("CreateFormFile: %v", err)
		}
		_, _ = part.Write(data)
	}
	_ = writer.Close()
	req := httptest.NewRequest(http.MethodPost, target, &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	return req
}

var pngFixture = append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{7}, 24)...)

func TestBrandingAssetUpload(t *testing.T) {
	store := &memoryObjectStore{}
	handler := admin.NewHandler(nil, admin.WithBrandingAssets(v2branding.NewAssetStore(store)))
	router := chi.NewRouter()
	router.Post("/admin/branding/assets/{kind}", handler.BrandingAssetUpload)
	serve := func(req *http.Request) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	t.Run("a png logo mark is stored and its path returned", func(t *testing.T) {
		rec := serve(multipartUpload(t, "/admin/branding/assets/logo-mark", "mark.png", pngFixture))
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		var asset v2branding.Asset
		if err := json.Unmarshal(rec.Body.Bytes(), &asset); err != nil {
			t.Fatalf("decode: %v", err)
		}
		kind, _, ext, ok := v2branding.ParseAssetPath(asset.Path)
		if !ok || kind != v2branding.KindLogoMark || ext != "png" || asset.Size != int64(len(pngFixture)) {
			t.Fatalf("asset = %+v", asset)
		}
		if _, present := store.objects["p/platform/b/branding/o/logo-mark/"+asset.Digest+".png"]; !present {
			t.Fatalf("object not stored under the platform scope: %v", keysOf(store))
		}
	})

	t.Run("refusals", func(t *testing.T) {
		for name, tc := range map[string]struct {
			target, filename string
			data             []byte
			want             int
		}{
			"unknown kind":      {"/admin/branding/assets/banner", "x.png", pngFixture, http.StatusNotFound},
			"no file part":      {"/admin/branding/assets/logo-full", "", nil, http.StatusBadRequest},
			"wrong extension":   {"/admin/branding/assets/font", "Inter.ttf", []byte("wOF2"), http.StatusUnsupportedMediaType},
			"fake png":          {"/admin/branding/assets/favicon", "f.png", []byte("not a png"), http.StatusUnsupportedMediaType},
			"scripted svg":      {"/admin/branding/assets/logo-full", "l.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>1</script></svg>`), http.StatusUnsupportedMediaType},
			"oversized favicon": {"/admin/branding/assets/favicon", "f.png", append(pngFixture, bytes.Repeat([]byte{0}, 70*1024)...), http.StatusRequestEntityTooLarge},
		} {
			t.Run(name, func(t *testing.T) {
				rec := serve(multipartUpload(t, tc.target, tc.filename, tc.data))
				if rec.Code != tc.want {
					t.Fatalf("status %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
				}
			})
		}
	})

	t.Run("no store answers 503", func(t *testing.T) {
		bare := admin.NewHandler(nil)
		r := chi.NewRouter()
		r.Post("/admin/branding/assets/{kind}", bare.BrandingAssetUpload)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, multipartUpload(t, "/admin/branding/assets/logo-full", "l.png", pngFixture))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
	})
}

func keysOf(m *memoryObjectStore) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := make([]string, 0, len(m.objects))
	for k := range m.objects {
		keys = append(keys, k)
	}
	return keys
}
