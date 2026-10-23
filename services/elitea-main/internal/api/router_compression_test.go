package api

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"context"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/applications"
)

// The API served every JSON answer uncompressed.
//
// DEFECT: the router mounted no compression middleware, so `Accept-Encoding:
// gzip` was ignored on every /api/v2 route. `GET
// /api/v2/configurations/available/` is a 136 KB catalogue that gzips to about
// 17 KB, and the credential form waits for the whole of it on every load.
//
// The three cases below are the contract the fix must keep: JSON is
// compressed, an event stream is not, and a byte-range request is not.
func TestCompressJSONResponsesCompressesOnlyJSON(t *testing.T) {
	t.Parallel()

	const payload = `{"items":["aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"]}`

	router := chi.NewRouter()
	router.Use(compressJSONResponses())
	router.Get("/json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, payload)
	})
	router.Get("/stream", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("the response writer of an event stream must stay a http.Flusher")
			return
		}
		_, _ = io.WriteString(w, "data: one\n\n")
		flusher.Flush()
	})
	router.Get("/object", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Range", "bytes 0-9/100")
		w.Header().Set("Content-Length", "10")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "0123456789")
	})

	t.Run("json_is_gzipped", func(t *testing.T) {
		rec := serveWithEncoding(router, "/json", "")

		if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
			t.Fatalf("Content-Encoding = %q, want gzip", got)
		}
		reader, err := gzip.NewReader(rec.Body)
		if err != nil {
			t.Fatalf("reading the gzip body: %v", err)
		}
		body, err := io.ReadAll(reader)
		if err != nil {
			t.Fatalf("decompressing the body: %v", err)
		}
		if string(body) != payload {
			t.Fatalf("decompressed body = %q, want %q", body, payload)
		}
	})

	t.Run("a_caller_that_asks_for_no_encoding_still_gets_plain_json", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/json", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if got := rec.Header().Get("Content-Encoding"); got != "" {
			t.Fatalf("Content-Encoding = %q, want none", got)
		}
		if rec.Body.String() != payload {
			t.Fatalf("body = %q, want %q", rec.Body.String(), payload)
		}
	})

	t.Run("an_event_stream_is_never_compressed", func(t *testing.T) {
		rec := serveWithEncoding(router, "/stream", "")

		if got := rec.Header().Get("Content-Encoding"); got != "" {
			t.Fatalf("Content-Encoding = %q, want none — gzip buffers an SSE stream", got)
		}
		if !strings.Contains(rec.Body.String(), "data: one") {
			t.Fatalf("body = %q, want the raw event", rec.Body.String())
		}
	})

	t.Run("a_byte_range_answer_is_never_compressed", func(t *testing.T) {
		// A 206 carries Content-Range and Content-Length that describe the RAW
		// object. chi compresses by content type and ignores the status code,
		// so it would gzip the body and drop Content-Length.
		rec := serveWithEncoding(router, "/object", "bytes=0-9")

		if got := rec.Header().Get("Content-Encoding"); got != "" {
			t.Fatalf("Content-Encoding = %q, want none for a 206", got)
		}
		if got := rec.Header().Get("Content-Length"); got != "10" {
			t.Fatalf("Content-Length = %q, want 10", got)
		}
		if rec.Body.String() != "0123456789" {
			t.Fatalf("body = %q, want the raw range", rec.Body.String())
		}
	})
}

func serveWithEncoding(router chi.Router, path, byteRange string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Accept-Encoding", "gzip")
	if byteRange != "" {
		req.Header.Set("Range", byteRange)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// The middleware must be MOUNTED, not merely written. A helper nobody calls is
// the recurring shape here, and the compression defect looks identical whether
// the middleware is absent or unwired.
func TestProductionRouterCompressesTheJSONAPI(t *testing.T) {
	t.Parallel()

	pool, err := pgxpool.New(context.Background(), "postgres://nouser:nopass@127.0.0.1:1/nodb")
	if err != nil {
		t.Fatalf("pgxpool.New (lazy, must not dial): %v", err)
	}
	defer pool.Close()

	router := NewRouter(RouterConfig{
		AuthValidator:      testTokenValidator{user: authenticatedTestUser()},
		PrincipalValidator: testPrincipalValidator{},
		AppsRepo:           struct{ applications.Repository }{},
		Pool:               pool,
	})

	// The call is authenticated, and the unreachable pool then answers a JSON
	// error from inside the group. The body shape does not matter here — the
	// encoding of a JSON answer does.
	req := testAuthHeader(httptest.NewRequest(http.MethodGet, "/api/v2/elitea_core/applications/prompt_lib/1", nil))
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json — this test needs a JSON answer", got)
	}
	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip — the /api/v2 group serves JSON uncompressed", got)
	}

	// The compressor sits above the authentication middleware, so an
	// anonymous error answer is compressed too.
	anonymous := httptest.NewRequest(http.MethodGet, "/api/v2/elitea_core/applications/prompt_lib/1", nil)
	anonymous.Header.Set("Accept-Encoding", "gzip")
	anonymousRec := httptest.NewRecorder()
	router.ServeHTTP(anonymousRec, anonymous)

	if got := anonymousRec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("anonymous Content-Encoding = %q, want gzip", got)
	}
}
