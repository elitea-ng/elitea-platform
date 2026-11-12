package agentmediaapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentmedia"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/llmproxy"
)

// memoryStore is an in-memory storage.ObjectStore: Put and Get only.
type memoryStore struct {
	storage.ObjectStore
	mu      sync.Mutex
	objects map[string][]byte
	types   map[string]string
	puts    map[string]int
	// noTotal leaves TotalSize at 0 on a ranged Get, as the storage
	// contract allows a backend to.
	noTotal bool
}

func (m *memoryStore) Stat(_ context.Context, ref storage.ObjectRef) (storage.ObjectInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.objects[ref.StorageKey("")]
	if !ok {
		return storage.ObjectInfo{}, storage.ErrNotFound
	}
	return storage.ObjectInfo{Key: ref.Key(), Size: int64(len(data))}, nil
}

func newMemoryStore() *memoryStore {
	return &memoryStore{objects: map[string][]byte{}, types: map[string]string{}, puts: map[string]int{}}
}

func (m *memoryStore) Put(_ context.Context, ref storage.ObjectRef, body io.Reader, opts storage.PutOptions) (storage.ObjectInfo, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[ref.StorageKey("")] = data
	m.types[ref.StorageKey("")] = opts.ContentType
	m.puts[ref.StorageKey("")]++
	return storage.ObjectInfo{Key: ref.Key(), Size: int64(len(data))}, nil
}

func (m *memoryStore) Get(_ context.Context, ref storage.ObjectRef, rng *storage.ByteRange) (io.ReadCloser, storage.ObjectInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.objects[ref.StorageKey("")]
	if !ok {
		return nil, storage.ObjectInfo{}, storage.ErrNotFound
	}
	info := storage.ObjectInfo{Key: ref.Key(), Size: int64(len(data))}
	if rng != nil {
		end := rng.End
		if end < 0 || end >= int64(len(data)) {
			end = int64(len(data)) - 1
		}
		if rng.Start >= int64(len(data)) {
			return nil, storage.ObjectInfo{}, errors.New("range start past the end")
		}
		if !m.noTotal {
			info.TotalSize = int64(len(data))
		}
		data = data[rng.Start : end+1]
		info.Size = int64(len(data))
	}
	return io.NopCloser(bytes.NewReader(data)), info, nil
}

func (m *memoryStore) putCount(ref storage.ObjectRef) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.puts[ref.StorageKey("")]
}

func (m *memoryStore) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.objects)
}

// fakeGateway answers the two image routes with canned images.
type fakeGateway struct {
	llmproxy.Gateway
	status    int
	image     []byte
	mu        sync.Mutex
	generates []map[string]any
	edits     []string
	identity  llmproxy.Identity
}

func (g *fakeGateway) answer() *llmproxy.Response {
	if g.status != 0 && g.status != http.StatusOK {
		return &llmproxy.Response{StatusCode: g.status, Body: io.NopCloser(strings.NewReader(`{"error":"x"}`))}
	}
	b64 := base64.StdEncoding.EncodeToString(g.image)
	body := `{"data":[{"url":"https://provider.example/x.png"},{"b64_json":"` + b64 + `"},{"b64_json":"` + b64 + `"}]}`
	return &llmproxy.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}
}

func (g *fakeGateway) ImageGenerate(_ context.Context, req *llmproxy.Request) (*llmproxy.Response, error) {
	var body map[string]any
	_ = json.NewDecoder(req.Body).Decode(&body)
	g.mu.Lock()
	g.generates = append(g.generates, body)
	g.identity = req.Identity
	g.mu.Unlock()
	return g.answer(), nil
}

func (g *fakeGateway) ImageEdit(_ context.Context, req *llmproxy.Request) (*llmproxy.Response, error) {
	_, params, err := mime.ParseMediaType(req.Header.Get("Content-Type"))
	if err != nil {
		return nil, err
	}
	reader := multipart.NewReader(req.Body, params["boundary"])
	var imageType, prompt string
	for {
		part, err := reader.NextPart()
		if err != nil {
			break
		}
		data, _ := io.ReadAll(part)
		switch part.FormName() {
		case "image":
			imageType = part.Header.Get("Content-Type")
		case "prompt":
			prompt = string(data)
		}
	}
	g.mu.Lock()
	g.edits = append(g.edits, imageType+"|"+prompt)
	g.mu.Unlock()
	return g.answer(), nil
}

func pngOf(t *testing.T, edge int, c color.Color) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, edge, edge))
	for y := 0; y < edge; y++ {
		for x := 0; x < edge; x++ {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func router(h *Handler, store storage.ObjectStore) http.Handler {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(auth.ContextWithUser(req.Context(), auth.User{ID: "42", UserID: "42"})))
		})
	})
	r.Post("/api/v2/elitea_core"+UploadPath, h.Upload)
	r.Post("/api/v2/elitea_core"+GeneratePath, h.Generate)
	r.Get(DownloadPath, Download(store))
	return r
}

// upload posts one file the way Go's multipart writer does by default:
// every part labelled application/octet-stream, the name chosen by the caller.
func upload(t *testing.T, handler http.Handler, project, filename, contentType string, data []byte) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", `form-data; name="file"; filename="`+filename+`"`)
	header.Set("Content-Type", contentType)
	part, err := form.CreatePart(header)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write(data)
	_ = form.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/v2/elitea_core/agent_media/prompt_lib/"+project, &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestUploadDetectsTypeFromBytesAndServesHardened(t *testing.T) {
	store := newMemoryStore()
	handler := router(NewHandler(store, nil), store)
	still := pngOf(t, 64, color.NRGBA{R: 255, A: 255})

	// A PNG named .mp4 and labelled octet-stream is stored as a PNG.
	rec := upload(t, handler, "7", "avatar.mp4", "application/octet-stream", still)
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body)
	}
	var object MediaObject
	if err := json.Unmarshal(rec.Body.Bytes(), &object); err != nil {
		t.Fatal(err)
	}
	if object.MediaType != "image/png" || !strings.HasSuffix(object.URL, ".png") || object.Width != 64 {
		t.Fatalf("object: %+v", object)
	}
	if !strings.HasPrefix(object.URL, agentmedia.URLPrefix+"7/") {
		t.Fatalf("url: %s", object.URL)
	}

	// The same bytes again are the same object.
	again := upload(t, handler, "7", "other.png", "image/png", still)
	var second MediaObject
	_ = json.Unmarshal(again.Body.Bytes(), &second)
	if second.URL != object.URL || store.count() != 1 {
		t.Fatalf("not content-addressed: %s vs %s (%d objects)", second.URL, object.URL, store.count())
	}

	get := httptest.NewRecorder()
	handler.ServeHTTP(get, httptest.NewRequest(http.MethodGet, object.URL, nil))
	if get.Code != http.StatusOK || !bytes.Equal(get.Body.Bytes(), still) {
		t.Fatalf("download: %d", get.Code)
	}
	for key, want := range map[string]string{
		"Content-Type":            "image/png",
		"X-Content-Type-Options":  "nosniff",
		"Content-Security-Policy": "default-src 'none'; sandbox",
		"Content-Disposition":     "attachment",
		"Cache-Control":           cacheImmutable,
	} {
		if got := get.Header().Get(key); got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}

	ranged := httptest.NewRequest(http.MethodGet, object.URL, nil)
	ranged.Header.Set("Range", "bytes=0-7")
	partial := httptest.NewRecorder()
	handler.ServeHTTP(partial, ranged)
	if partial.Code != http.StatusPartialContent || partial.Body.Len() != 8 {
		t.Fatalf("range: %d %d", partial.Code, partial.Body.Len())
	}
	if got := partial.Header().Get("Content-Range"); got != fmt.Sprintf("bytes 0-7/%d", len(still)) {
		t.Fatalf("Content-Range = %q", got)
	}

	// A range past the end is 416 with the length, not 404.
	past := httptest.NewRequest(http.MethodGet, object.URL, nil)
	past.Header.Set("Range", fmt.Sprintf("bytes=%d-", len(still)+10))
	unsatisfiable := httptest.NewRecorder()
	handler.ServeHTTP(unsatisfiable, past)
	if unsatisfiable.Code != http.StatusRequestedRangeNotSatisfiable ||
		unsatisfiable.Header().Get("Content-Range") != fmt.Sprintf("bytes */%d", len(still)) {
		t.Fatalf("range past the end: %d %q", unsatisfiable.Code, unsatisfiable.Header().Get("Content-Range"))
	}

	// A backend that leaves TotalSize at 0 still gets a 206 with a
	// Content-Range, never a 200 carrying only the range.
	store.noTotal = true
	tail := httptest.NewRequest(http.MethodGet, object.URL, nil)
	tail.Header.Set("Range", "bytes=8-")
	tailRec := httptest.NewRecorder()
	handler.ServeHTTP(tailRec, tail)
	store.noTotal = false
	if tailRec.Code != http.StatusPartialContent || tailRec.Header().Get("Content-Range") != fmt.Sprintf("bytes 8-%d/%d", len(still)-1, len(still)) {
		t.Fatalf("range without a total: %d %q", tailRec.Code, tailRec.Header().Get("Content-Range"))
	}

	// Another project's id in the path does not reach this project's object.
	file := strings.TrimPrefix(object.URL, agentmedia.URLPrefix+"7/")
	cross := httptest.NewRecorder()
	handler.ServeHTTP(cross, httptest.NewRequest(http.MethodGet, agentmedia.URLPrefix+"8/"+file, nil))
	if cross.Code != http.StatusNotFound {
		t.Fatalf("cross-project read: %d", cross.Code)
	}
	bad := httptest.NewRecorder()
	handler.ServeHTTP(bad, httptest.NewRequest(http.MethodGet, agentmedia.URLPrefix+"7/x.html", nil))
	if bad.Code != http.StatusNotFound {
		t.Fatalf("unknown shape: %d", bad.Code)
	}
}

func TestUploadRefusals(t *testing.T) {
	store := newMemoryStore()
	handler := router(NewHandler(store, nil), store)
	cases := map[string]struct {
		filename, contentType string
		data                  []byte
		status                int
	}{
		"svg script named png": {"a.png", "image/png", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`), http.StatusBadRequest},
		"html named png":       {"a.png", "image/png", []byte(`<html><body>x</body></html>`), http.StatusBadRequest},
		"oversize":             {"a.png", "image/png", append(pngOf(t, 8, color.Black), bytes.Repeat([]byte{0}, agentmedia.MaxBytes)...), http.StatusBadRequest},
		"oversize edge":        {"a.png", "image/png", pngOf(t, 600, color.Black), http.StatusBadRequest},
		"empty":                {"a.png", "image/png", nil, http.StatusBadRequest},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rec := upload(t, handler, "7", tc.filename, tc.contentType, tc.data)
			if rec.Code != tc.status {
				t.Fatalf("status %d: %s", rec.Code, rec.Body)
			}
			var body refusal
			_ = json.Unmarshal(rec.Body.Bytes(), &body)
			if body.Code != agentmedia.Code {
				t.Fatalf("code %q", body.Code)
			}
		})
	}
	if store.count() != 0 {
		t.Fatalf("a refused file was stored (%d objects)", store.count())
	}
	if rec := upload(t, router(NewHandler(nil, nil), nil), "7", "a.png", "image/png", pngOf(t, 8, color.Black)); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no store: %d", rec.Code)
	}
}

func generate(t *testing.T, handler http.Handler, project string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/v2/elitea_core/agent_media_generate/prompt_lib/"+project, bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestGenerateStillsFromSoul(t *testing.T) {
	store := newMemoryStore()
	gateway := &fakeGateway{image: pngOf(t, 1024, color.NRGBA{B: 255, A: 255})}
	handler := router(NewHandler(store, gateway), store)
	rec := generate(t, handler, "7", map[string]any{
		"kind": "still", "model": "gpt-image-1", "display_name": "Mira", "role": "Release coordinator",
		"voice": []string{"concise", "warm"}, "count": 2,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("generate: %d %s", rec.Code, rec.Body)
	}
	var answer GenerateResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &answer)
	// The url-only item is skipped, never fetched; both b64 items are the
	// same bytes, so one object.
	if len(answer.Media) != 2 || answer.Media[0].Width != agentmedia.StillEdge || answer.Media[0].GeneratedBy != "gpt-image-1" {
		t.Fatalf("media: %+v", answer.Media)
	}
	sent := gateway.generates[0]
	prompt, _ := sent["prompt"].(string)
	if !strings.Contains(prompt, "Mira") || !strings.Contains(prompt, "Release coordinator") || !strings.Contains(prompt, "concise, warm") {
		t.Fatalf("prompt: %q", prompt)
	}
	if sent["response_format"] != "b64_json" || sent["n"].(float64) != 2 {
		t.Fatalf("body: %v", sent)
	}
	if gateway.identity.ProjectID != "7" || gateway.identity.UserID != "42" {
		t.Fatalf("identity: %+v", gateway.identity)
	}
}

func TestGenerateLoopAssemblesAnimatedWebP(t *testing.T) {
	store := newMemoryStore()
	gateway := &fakeGateway{image: pngOf(t, 512, color.NRGBA{G: 255, A: 255})}
	handler := router(NewHandler(store, gateway), store)
	still := upload(t, handler, "7", "s.png", "application/octet-stream", pngOf(t, 128, color.White))
	var stillObject MediaObject
	_ = json.Unmarshal(still.Body.Bytes(), &stillObject)

	rec := generate(t, handler, "7", map[string]any{
		"kind": "loop", "model": "gpt-image-1", "state": "thinking", "still_url": stillObject.URL, "frames": 5,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("loop: %d %s", rec.Code, rec.Body)
	}
	var answer GenerateResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &answer)
	if len(answer.Media) != 1 || answer.Media[0].Format != string(agentmedia.FormatAnimatedWebP) || answer.Media[0].DurationMS != 5*loopFrameMS {
		t.Fatalf("media: %+v", answer.Media)
	}
	if len(gateway.edits) != 5 || !strings.HasPrefix(gateway.edits[0], "image/png|") || !strings.Contains(gateway.edits[0], "thinking") {
		t.Fatalf("edits: %v", gateway.edits)
	}

	// A still of another project is refused before any gateway call.
	foreign := generate(t, handler, "8", map[string]any{
		"kind": "loop", "model": "m", "state": "thinking", "still_url": stillObject.URL,
	})
	if foreign.Code != http.StatusBadRequest || len(gateway.edits) != 5 {
		t.Fatalf("foreign still: %d, edits %d", foreign.Code, len(gateway.edits))
	}
}

func TestGenerateRefusals(t *testing.T) {
	store := newMemoryStore()
	cases := map[string]struct {
		gateway llmproxy.Gateway
		body    map[string]any
		status  int
	}{
		"no gateway":     {nil, map[string]any{"kind": "still", "model": "m"}, http.StatusServiceUnavailable},
		"video kind":     {&fakeGateway{}, map[string]any{"kind": "video", "model": "m"}, http.StatusBadRequest},
		"lottie key":     {&fakeGateway{}, map[string]any{"kind": "still", "model": "m", "lottie": true}, http.StatusBadRequest},
		"no model":       {&fakeGateway{}, map[string]any{"kind": "still"}, http.StatusBadRequest},
		"budget 402":     {&fakeGateway{status: http.StatusPaymentRequired}, map[string]any{"kind": "still", "model": "m"}, http.StatusPaymentRequired},
		"provider 500":   {&fakeGateway{status: http.StatusInternalServerError}, map[string]any{"kind": "still", "model": "m"}, http.StatusBadGateway},
		"too many":       {&fakeGateway{}, map[string]any{"kind": "still", "model": "m", "count": 9}, http.StatusBadRequest},
		"unknown state":  {&fakeGateway{}, map[string]any{"kind": "loop", "model": "m", "state": "dancing"}, http.StatusBadRequest},
		"external still": {&fakeGateway{}, map[string]any{"kind": "loop", "model": "m", "state": "idle", "still_url": "https://evil.example/a.png"}, http.StatusBadRequest},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rec := generate(t, router(NewHandler(store, tc.gateway), store), "7", tc.body)
			if rec.Code != tc.status {
				t.Fatalf("status %d, want %d: %s", rec.Code, tc.status, rec.Body)
			}
		})
	}
	if store.count() != 0 {
		t.Fatalf("a refused generation stored %d objects", store.count())
	}
}
