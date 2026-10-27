package middleware_test

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	chimw "github.com/go-chi/chi/v5/middleware"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/ssewriter"
)

func TestOtelMiddleware_SetsStatusCode(t *testing.T) {
	handler := middleware.OtelMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))

	req := httptest.NewRequest("GET", "/test", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Errorf("expected 201, got %d", rec.Code)
	}
}

func TestOtelMiddleware_DefaultStatus200(t *testing.T) {
	handler := middleware.OtelMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))

	req := httptest.NewRequest("GET", "/test", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}

// An event stream behind the OTel middleware and chi's Compress (the project
// event stream's chain) reaches the client at its first flush. chi flushes by
// asserting http.Flusher on the writer it wraps, which is the OTel recorder;
// without a Flush there, the stream's frames waited in the server's buffer and
// no client ever received one.
func TestOtelMiddleware_AnEventStreamBehindCompressFlushesAtOnce(t *testing.T) {
	release := make(chan struct{})
	stream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sse, err := ssewriter.New(w)
		if err != nil {
			t.Errorf("ssewriter: %v", err)
			return
		}
		_ = sse.Comment("connected")
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	server := httptest.NewServer(middleware.OtelMiddleware(chimw.Compress(5, "application/json")(stream)))
	defer server.Close()
	defer close(release)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Accept-Encoding", "gzip")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("no response while the stream is open: %v", err)
	}
	defer response.Body.Close()
	line, err := bufio.NewReader(response.Body).ReadString('\n')
	if err != nil || line != ": connected\n" {
		t.Fatalf("first frame = %q, %v", line, err)
	}
}
