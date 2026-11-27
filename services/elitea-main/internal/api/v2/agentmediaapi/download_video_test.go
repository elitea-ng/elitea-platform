package agentmediaapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentmedia"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
)

// #3132: an avatar pack's MP4 and WebM loops load in a <video> element. The
// download route is public (no credential, as an <img> or <video> sends
// none), answers the video's own Content-Type from the stored extension, and
// serves the ranges a <video> asks for.
func TestDownloadServesVideoLoopsWithTheirTypeAndRangesWithoutACredential(t *testing.T) {
	store := newMemoryStore()
	// No auth middleware: the request carries no user, as a media element's does not.
	r := chi.NewRouter()
	r.Get(DownloadPath, Download(store))

	for extension, want := range map[string]string{"mp4": "video/mp4", "webm": "video/webm"} {
		data := []byte("not really a " + extension + " but bytes enough to range over")
		sum := sha256.Sum256(data)
		file := hex.EncodeToString(sum[:]) + "." + extension
		ref, err := storage.NewObjectRef("7", Bucket, file)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Put(context.Background(), ref, bytes.NewReader(data), storage.PutOptions{ContentType: want}); err != nil {
			t.Fatal(err)
		}
		url := agentmedia.URLOf("7", file)

		whole := httptest.NewRecorder()
		r.ServeHTTP(whole, httptest.NewRequest(http.MethodGet, url, nil))
		if whole.Code != http.StatusOK || !bytes.Equal(whole.Body.Bytes(), data) {
			t.Fatalf("%s: download %d", extension, whole.Code)
		}
		if got := whole.Header().Get("Content-Type"); got != want {
			t.Fatalf("%s: Content-Type = %q, want %q", extension, got, want)
		}
		if got := whole.Header().Get("Accept-Ranges"); got != "bytes" {
			t.Fatalf("%s: Accept-Ranges = %q", extension, got)
		}

		ranged := httptest.NewRequest(http.MethodGet, url, nil)
		ranged.Header.Set("Range", "bytes=0-")
		partial := httptest.NewRecorder()
		r.ServeHTTP(partial, ranged)
		if partial.Code != http.StatusPartialContent || partial.Header().Get("Content-Type") != want {
			t.Fatalf("%s: range %d %q", extension, partial.Code, partial.Header().Get("Content-Type"))
		}
	}
}
