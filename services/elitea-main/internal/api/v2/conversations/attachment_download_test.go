package conversations_test

// downloadConversationAttachment (client contract 1.1): the bytes of one chat
// attachment, scoped by the conversation the caller may read.

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/conversations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

const downloadConversation = "0b9b7c55-6f0e-4b8e-9a51-3d6c1f2a9e10"

// readableObjectStore serves Get from the bytes Put stored, honouring a
// range the way the real backends do.
type readableObjectStore struct {
	*fakeAttachmentObjectStore
	gets int
}

func (s *readableObjectStore) Get(_ context.Context, ref storage.ObjectRef, rng *storage.ByteRange) (io.ReadCloser, storage.ObjectInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gets++
	data, ok := s.objects[ref.Key()]
	if !ok {
		return nil, storage.ObjectInfo{}, storage.ErrNotFound
	}
	total := int64(len(data))
	if rng != nil {
		end := rng.End
		if end < 0 || end >= total {
			end = total - 1
		}
		data = data[rng.Start : end+1]
	}
	return io.NopCloser(bytes.NewReader(data)), storage.ObjectInfo{Key: ref.Key(), Size: int64(len(data)), TotalSize: total, ETag: "abc123"}, nil
}

type downloadFixture struct {
	router  chi.Router
	repo    *mockRepo
	objects *readableObjectStore
}

func newDownloadFixture(t *testing.T) *downloadFixture {
	t.Helper()
	attachments := newFakeAttachmentStore()
	objects := &readableObjectStore{fakeAttachmentObjectStore: newFakeAttachmentObjectStore()}
	repo := &mockRepo{}
	handler := conversations.NewHandler(repo).WithObjectStore(objects).WithAttachmentStore(attachments)

	put := func(name, mediaType string, content []byte) {
		bucketID, _ := attachments.RequireAttachmentBucket(context.Background(), 1, "chat-attachments", 30)
		key := downloadConversation + "/" + name
		objects.objects[key] = content
		_ = attachments.RecordAttachmentObject(context.Background(), bucketID, key, int64(len(content)), mediaType, nil)
	}
	png := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0}, 600)...)
	put("photo.png", "image/png", png)
	put("notes.md", "text/markdown", []byte("# Notes\nhello"))
	put("drawing.svg", "image/svg+xml", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`))
	put("fake.png", "image/png", []byte("<html><body>not an image</body></html>"))
	put("report été.pdf", "application/pdf", append([]byte("%PDF-1.7\n"), bytes.Repeat([]byte("x"), 100)...))
	put("100%41.txt", "text/plain", []byte("percent"))

	router := chi.NewRouter()
	router.Get("/{projectID}/attachments/{conversationID}/{name}", handler.DownloadAttachment)
	router.Head("/{projectID}/attachments/{conversationID}/{name}", handler.DownloadAttachment)
	return &downloadFixture{router: router, repo: repo, objects: objects}
}

func (f *downloadFixture) do(method, name string, header http.Header) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, "/1/attachments/"+downloadConversation+"/"+name, nil)
	for key, values := range header {
		request.Header[key] = values
	}
	recorder := httptest.NewRecorder()
	f.router.ServeHTTP(recorder, request)
	return recorder
}

func TestDownloadAttachmentServesTheBytesAsADownload(t *testing.T) {
	f := newDownloadFixture(t)
	recorder := f.do(http.MethodGet, "photo.png", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d body %s", recorder.Code, recorder.Body.String())
	}
	header := recorder.Header()
	for name, want := range map[string]string{
		"Content-Type":            "image/png",
		"X-Content-Type-Options":  "nosniff",
		"Content-Security-Policy": "sandbox",
		"Cache-Control":           "private, no-store",
		"Accept-Ranges":           "bytes",
		"Content-Length":          "608",
		"ETag":                    `"abc123"`,
		"Content-Disposition":     `attachment; filename="photo.png"; filename*=UTF-8''photo.png`,
	} {
		if got := header.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if recorder.Body.Len() != 608 || !bytes.HasPrefix(recorder.Body.Bytes(), []byte("\x89PNG")) {
		t.Fatalf("body is %d bytes, want the 608-byte PNG", recorder.Body.Len())
	}

	// A conditional GET with the same ETag is answered without the body.
	notModified := f.do(http.MethodGet, "photo.png", http.Header{"If-None-Match": {`"abc123"`}})
	if notModified.Code != http.StatusNotModified || notModified.Body.Len() != 0 {
		t.Fatalf("If-None-Match: status %d, %d body bytes", notModified.Code, notModified.Body.Len())
	}
}

func TestDownloadAttachmentNeverServesAnActiveOrMislabelledType(t *testing.T) {
	f := newDownloadFixture(t)
	for name, want := range map[string]string{
		"drawing.svg":                "application/octet-stream", // active: SVG runs script
		"fake.png":                   "application/octet-stream", // says PNG, is HTML
		"notes.md":                   "text/markdown; charset=utf-8",
		"report%20%C3%A9t%C3%A9.pdf": "application/pdf",
	} {
		recorder := f.do(http.MethodGet, name, nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s: status %d body %s", name, recorder.Code, recorder.Body.String())
		}
		if got := recorder.Header().Get("Content-Type"); got != want {
			t.Errorf("%s: Content-Type %q, want %q", name, got, want)
		}
	}
	// A literal "%41" in a stored name is not decoded a second time.
	if recorder := f.do(http.MethodGet, "100%2541.txt", nil); recorder.Code != http.StatusOK || recorder.Body.String() != "percent" {
		t.Errorf("a name with a literal percent: %d %q", recorder.Code, recorder.Body.String())
	}
	disposition := f.do(http.MethodGet, "report%20%C3%A9t%C3%A9.pdf", nil).Header().Get("Content-Disposition")
	if disposition != `attachment; filename="report _t_.pdf"; filename*=UTF-8''report%20%C3%A9t%C3%A9.pdf` {
		t.Errorf("a non-ASCII name's Content-Disposition = %q", disposition)
	}
}

func TestDownloadAttachmentServesARange(t *testing.T) {
	f := newDownloadFixture(t)
	recorder := f.do(http.MethodGet, "notes.md", http.Header{"Range": {"bytes=2-6"}})
	if recorder.Code != http.StatusPartialContent {
		t.Fatalf("status %d, want 206", recorder.Code)
	}
	if got := recorder.Body.String(); got != "Notes" {
		t.Fatalf("range body %q, want %q", got, "Notes")
	}
	if got := recorder.Header().Get("Content-Range"); got != "bytes 2-6/13" {
		t.Fatalf("Content-Range %q", got)
	}
	// The type is still judged on the file's first bytes, not the slice.
	if got := recorder.Header().Get("Content-Type"); got != "text/markdown; charset=utf-8" {
		t.Fatalf("a ranged read's Content-Type %q", got)
	}

	suffix := f.do(http.MethodGet, "notes.md", http.Header{"Range": {"bytes=-5"}})
	if suffix.Code != http.StatusPartialContent || suffix.Body.String() != "hello" {
		t.Fatalf("suffix range: %d %q", suffix.Code, suffix.Body.String())
	}
	unsatisfiable := f.do(http.MethodGet, "notes.md", http.Header{"Range": {"bytes=99-"}})
	if unsatisfiable.Code != http.StatusRequestedRangeNotSatisfiable || unsatisfiable.Header().Get("Content-Range") != "bytes */13" {
		t.Fatalf("a range past the end: %d %q", unsatisfiable.Code, unsatisfiable.Header().Get("Content-Range"))
	}
}

func TestDownloadAttachmentHeadSendsHeadersOnly(t *testing.T) {
	f := newDownloadFixture(t)
	recorder := f.do(http.MethodHead, "notes.md", nil)
	if recorder.Code != http.StatusOK || recorder.Body.Len() != 0 {
		t.Fatalf("HEAD: status %d with %d body bytes", recorder.Code, recorder.Body.Len())
	}
	if recorder.Header().Get("Content-Length") != "13" {
		t.Fatalf("HEAD Content-Length %q", recorder.Header().Get("Content-Length"))
	}
}

func TestDownloadAttachmentRefusesTraversalAndForeignNames(t *testing.T) {
	f := newDownloadFixture(t)
	for _, name := range []string{"..", "%2E%2E", "a%2Fb.png", "..%2Fphoto.png", "a%5Cb", "x%00y", "%0Aname"} {
		if recorder := f.do(http.MethodGet, name, nil); recorder.Code != http.StatusBadRequest {
			t.Errorf("name %q: status %d, want 400", name, recorder.Code)
		}
	}
	if recorder := f.do(http.MethodGet, "missing.png", nil); recorder.Code != http.StatusNotFound {
		t.Errorf("an unknown file: status %d, want 404", recorder.Code)
	}
	if f.objects.gets != 0 {
		t.Errorf("refused names reached the object store %d times", f.objects.gets)
	}

	numeric := httptest.NewRecorder()
	f.router.ServeHTTP(numeric, httptest.NewRequest(http.MethodGet, "/1/attachments/42/photo.png", nil))
	if numeric.Code != http.StatusBadRequest {
		t.Errorf("a numeric conversation id: status %d, want 400", numeric.Code)
	}
}

func TestDownloadAttachmentAnswers404ToACallerWhoCannotSeeTheConversation(t *testing.T) {
	f := newDownloadFixture(t)
	f.repo.authorizeFn = func(context.Context, string, string) error {
		return apierr.NotFound("chat resource not found")
	}
	recorder := f.do(http.MethodGet, "photo.png", nil)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", recorder.Code)
	}
	if strings.Contains(recorder.Body.String(), "PNG") || f.objects.gets != 0 {
		t.Fatal("bytes were read for a caller who cannot see the conversation")
	}
}

func TestDownloadAttachmentWithoutStorageIs501(t *testing.T) {
	handler := conversations.NewHandler(&mockRepo{})
	router := chi.NewRouter()
	router.Get("/{projectID}/attachments/{conversationID}/{name}", handler.DownloadAttachment)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/1/attachments/"+downloadConversation+"/photo.png", nil))
	if recorder.Code != http.StatusNotImplemented {
		t.Fatalf("status %d, want 501", recorder.Code)
	}
}
