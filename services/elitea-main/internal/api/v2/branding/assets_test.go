package branding

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
)

// memoryStore is an in-memory storage.ObjectStore: Put/Get/Delete/List.
type memoryStore struct {
	mu      sync.Mutex
	objects map[string][]byte // StorageKey("") -> bytes
}

func newMemoryStore() *memoryStore { return &memoryStore{objects: map[string][]byte{}} }

func (m *memoryStore) Put(_ context.Context, ref storage.ObjectRef, body io.Reader, _ storage.PutOptions) (storage.ObjectInfo, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[ref.StorageKey("")] = data
	return storage.ObjectInfo{Key: ref.Key(), Size: int64(len(data))}, nil
}

func (m *memoryStore) Get(_ context.Context, ref storage.ObjectRef, _ *storage.ByteRange) (io.ReadCloser, storage.ObjectInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.objects[ref.StorageKey("")]
	if !ok {
		return nil, storage.ObjectInfo{}, storage.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), storage.ObjectInfo{Key: ref.Key(), Size: int64(len(data))}, nil
}

func (m *memoryStore) Delete(_ context.Context, ref storage.ObjectRef) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, ref.StorageKey(""))
	return nil
}

func (m *memoryStore) List(_ context.Context, q storage.ListQuery) (storage.ListPage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	prefix := q.Bucket.BucketPrefix("")
	var page storage.ListPage
	for key, data := range m.objects {
		if rest, ok := strings.CutPrefix(key, prefix); ok {
			page.Objects = append(page.Objects, storage.ObjectInfo{Key: rest, Size: int64(len(data))})
		}
	}
	return page, nil
}

func (m *memoryStore) DeleteBatch(context.Context, []storage.ObjectRef) (storage.BatchResult, error) {
	return storage.BatchResult{}, storage.ErrNotSupported
}
func (m *memoryStore) Stat(context.Context, storage.ObjectRef) (storage.ObjectInfo, error) {
	return storage.ObjectInfo{}, storage.ErrNotSupported
}
func (m *memoryStore) PresignGet(context.Context, storage.ObjectRef, time.Duration) (string, error) {
	return "", storage.ErrNotSupported
}
func (m *memoryStore) PresignPut(context.Context, storage.ObjectRef, time.Duration, storage.PutOptions) (string, error) {
	return "", storage.ErrNotSupported
}
func (m *memoryStore) StartMultipart(context.Context, storage.ObjectRef, storage.PutOptions) (storage.UploadID, error) {
	return "", storage.ErrNotSupported
}
func (m *memoryStore) PresignPart(context.Context, storage.ObjectRef, storage.UploadID, int32, time.Duration) (string, error) {
	return "", storage.ErrNotSupported
}
func (m *memoryStore) CompleteMultipart(context.Context, storage.ObjectRef, storage.UploadID, []storage.Part) (storage.ObjectInfo, error) {
	return storage.ObjectInfo{}, storage.ErrNotSupported
}
func (m *memoryStore) AbortMultipart(context.Context, storage.ObjectRef, storage.UploadID) error {
	return storage.ErrNotSupported
}
func (m *memoryStore) Capabilities() storage.Capabilities { return storage.Capabilities{} }

const cleanSVG = `<?xml version="1.0" encoding="UTF-8"?>
<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink" viewBox="0 0 10 10">
  <defs><linearGradient id="g"><stop offset="0"/></linearGradient></defs>
  <style>.a{fill:currentColor}</style>
  <rect class="a" width="10" height="10" fill="url(#g)"/>
  <use xlink:href="#g"/>
</svg>`

var pngBytes = append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0}, 32)...)

func assetStatus(t *testing.T, err error) int {
	t.Helper()
	var refusal *AssetError
	if !errors.As(err, &refusal) {
		t.Fatalf("expected an AssetError, got %v", err)
	}
	return refusal.Status
}

func TestAssetStore_PutRules(t *testing.T) {
	ctx := context.Background()
	s := NewAssetStore(newMemoryStore())

	t.Run("a clean SVG logo is stored under its digest", func(t *testing.T) {
		asset, err := s.Put(ctx, KindLogoFull, "Logo.SVG", []byte(cleanSVG))
		if err != nil {
			t.Fatalf("Put: %v", err)
		}
		if asset.Extension != "svg" || asset.ContentType != "image/svg+xml" || len(asset.Digest) != 64 {
			t.Fatalf("asset = %+v", asset)
		}
		if asset.Path != AssetPathPrefix+"logo-full/"+asset.Digest+".svg" {
			t.Fatalf("path = %q", asset.Path)
		}
		kind, digest, ext, ok := ParseAssetPath(asset.Path)
		if !ok || kind != KindLogoFull || digest != asset.Digest || ext != "svg" {
			t.Fatalf("ParseAssetPath(%q) = %q %q %q %v", asset.Path, kind, digest, ext, ok)
		}
		again, err := s.Put(ctx, KindLogoFull, "other-name.svg", []byte(cleanSVG))
		if err != nil || again.Path != asset.Path {
			t.Fatalf("same bytes must be the same object: %v %q", err, again.Path)
		}
	})

	t.Run("png and woff2 are sniffed", func(t *testing.T) {
		if _, err := s.Put(ctx, KindLogoMark, "mark.png", pngBytes); err != nil {
			t.Fatalf("real png refused: %v", err)
		}
		if _, err := s.Put(ctx, KindLogoMark, "mark.png", []byte("GIF89a....")); assetStatus(t, err) != http.StatusUnsupportedMediaType {
			t.Fatalf("fake png: %v", err)
		}
		if _, err := s.Put(ctx, KindFont, "Inter.woff2", append([]byte("wOF2"), bytes.Repeat([]byte{1}, 16)...)); err != nil {
			t.Fatalf("real woff2 refused: %v", err)
		}
		if _, err := s.Put(ctx, KindFont, "Inter.woff2", []byte("wOFF....")); assetStatus(t, err) != http.StatusUnsupportedMediaType {
			t.Fatalf("woff v1 as woff2: %v", err)
		}
	})

	t.Run("extension and size rules per kind", func(t *testing.T) {
		if _, err := s.Put(ctx, KindFont, "Inter.ttf", []byte("x")); assetStatus(t, err) != http.StatusUnsupportedMediaType {
			t.Fatalf("ttf font: %v", err)
		}
		if _, err := s.Put(ctx, KindLogoEmail, "logo.svg", []byte(cleanSVG)); assetStatus(t, err) != http.StatusUnsupportedMediaType {
			t.Fatalf("svg e-mail logo (raster only): %v", err)
		}
		if _, err := s.Put(ctx, KindFavicon, "f.png", append(pngBytes, bytes.Repeat([]byte{0}, maxFaviconBytes)...)); assetStatus(t, err) != http.StatusRequestEntityTooLarge {
			t.Fatalf("oversized favicon: %v", err)
		}
		if _, err := s.Put(ctx, KindLogoFull, "empty.png", nil); assetStatus(t, err) != http.StatusBadRequest {
			t.Fatalf("empty file: %v", err)
		}
		if _, err := s.Put(ctx, "banner", "x.png", pngBytes); assetStatus(t, err) != http.StatusNotFound {
			t.Fatalf("unknown kind: %v", err)
		}
	})
}

func TestCheckSVG_Refusals(t *testing.T) {
	bad := map[string]string{
		"script":          `<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`,
		"event handler":   `<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"/>`,
		"foreignObject":   `<svg xmlns="http://www.w3.org/2000/svg"><foreignObject><div/></foreignObject></svg>`,
		"external href":   `<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink"><image xlink:href="https://evil.example/x.png"/></svg>`,
		"javascript href": `<svg xmlns="http://www.w3.org/2000/svg"><a href="javascript:alert(1)"><text>x</text></a></svg>`,
		"data svg href":   `<svg xmlns="http://www.w3.org/2000/svg"><image href="data:image/svg+xml;base64,AAAA"/></svg>`,
		"style url":       `<svg xmlns="http://www.w3.org/2000/svg"><style>rect{fill:url(https://evil.example/x)}</style></svg>`,
		"style import":    `<svg xmlns="http://www.w3.org/2000/svg"><style>@import "https://evil.example/x.css";</style></svg>`,
		"inline style":    `<svg xmlns="http://www.w3.org/2000/svg"><rect style="fill:url(https://e/x)"/></svg>`,
		"doctype":         `<!DOCTYPE svg [<!ENTITY x "y">]><svg xmlns="http://www.w3.org/2000/svg"/>`,
		"xml-stylesheet":  `<?xml-stylesheet href="https://evil.example/x.css"?><svg xmlns="http://www.w3.org/2000/svg"/>`,
		"html root":       `<html><body><svg/></body></html>`,
		"not xml":         `{"json": true}`,
		"empty":           ``,
	}
	for name, doc := range bad {
		t.Run(name, func(t *testing.T) {
			if err := checkSVG([]byte(doc)); err == nil {
				t.Fatalf("accepted:\n%s", doc)
			}
		})
	}
	if err := checkSVG([]byte(cleanSVG)); err != nil {
		t.Fatalf("clean SVG refused: %v", err)
	}
	// A data:image/png reference is a pixel, not a document.
	inlinePNG := `<svg xmlns="http://www.w3.org/2000/svg"><image href="data:image/png;base64,iVBORw0KGgo="/></svg>`
	if err := checkSVG([]byte(inlinePNG)); err != nil {
		t.Fatalf("inline png refused: %v", err)
	}
}

func TestParseAssetPath(t *testing.T) {
	digest := strings.Repeat("ab", 32)
	good := AssetPath(KindFavicon, digest, "ico")
	if _, _, _, ok := ParseAssetPath(good); !ok {
		t.Fatalf("rejected %q", good)
	}
	for _, path := range []string{
		"/api/v2/branding/assets/favicon/" + digest + ".exe",
		"/api/v2/branding/assets/font/" + digest + ".png",
		"/api/v2/branding/assets/banner/" + digest + ".png",
		"/api/v2/branding/assets/favicon/../" + digest + ".ico",
		"/api/v2/branding/assets/favicon/ABCD.ico",
		"https://cdn.example" + good,
		"/icons/1/x.png",
	} {
		if _, _, _, ok := ParseAssetPath(path); ok {
			t.Errorf("accepted %q", path)
		}
	}
}

func TestAssetStore_DownloadAndList(t *testing.T) {
	ctx := context.Background()
	store := newMemoryStore()
	s := NewAssetStore(store)
	asset, err := s.Put(ctx, KindFavicon, "f.png", pngBytes)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	router := chi.NewRouter()
	router.Get(AssetPathPrefix+"{kind}/{file}", s.Download)
	router.Head(AssetPathPrefix+"{kind}/{file}", s.Download)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, asset.Path, nil))
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), pngBytes) {
		t.Fatalf("GET %s: %d %d bytes", asset.Path, rec.Code, rec.Body.Len())
	}
	h := rec.Header()
	for name, want := range map[string]string{
		"Content-Type":            "image/png",
		"X-Content-Type-Options":  "nosniff",
		"Content-Security-Policy": "default-src 'none'; sandbox",
		"Content-Disposition":     "attachment",
		"Cache-Control":           cacheImmutable,
		"ETag":                    `"` + asset.Digest + `"`,
	} {
		if got := h.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}

	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, asset.Path, nil))
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 || rec.Header().Get("Content-Length") == "" {
		t.Fatalf("HEAD: %d body %d content-length %q", rec.Code, rec.Body.Len(), rec.Header().Get("Content-Length"))
	}

	for _, path := range []string{
		AssetPathPrefix + "favicon/" + strings.Repeat("00", 32) + ".png", // unknown digest
		AssetPathPrefix + "favicon/" + asset.Digest + ".svg",             // wrong extension
		AssetPathPrefix + "banner/" + asset.Digest + ".png",              // unknown kind
	} {
		rec = httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s: %d, want 404", path, rec.Code)
		}
	}

	paths, err := s.List(ctx)
	if err != nil || len(paths) != 1 || paths[0] != asset.Path {
		t.Fatalf("List = %v, %v", paths, err)
	}
	if err := s.Delete(ctx, asset.Path); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if paths, _ := s.List(ctx); len(paths) != 0 {
		t.Fatalf("after Delete, List = %v", paths)
	}

	// No store: uploads say so, downloads 404.
	none := NewAssetStore(nil)
	if _, err := none.Put(ctx, KindFavicon, "f.png", pngBytes); !errors.Is(err, ErrAssetStorageUnavailable) {
		t.Fatalf("Put without a store: %v", err)
	}
	rec = httptest.NewRecorder()
	r2 := chi.NewRouter()
	r2.Get(AssetPathPrefix+"{kind}/{file}", none.Download)
	r2.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, asset.Path, nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("download without a store: %d", rec.Code)
	}
}
