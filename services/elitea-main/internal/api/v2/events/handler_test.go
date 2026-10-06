package events

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/natsbus"
)

// fakeSource is an in-memory EventSource: it hands the test a send channel it
// can push decoded events onto, and records the channel it was asked to
// subscribe to. It lets the SSE Stream handler be exercised without a live
// Redis or NATS server.
type fakeSource struct {
	mu          sync.Mutex
	channel     string
	events      chan natsbus.Event
	err         error
	cancelCalls int
}

func newFakeSource() *fakeSource {
	return &fakeSource{events: make(chan natsbus.Event, 8)}
}

func (f *fakeSource) Raw(_ context.Context, channel string) (<-chan natsbus.Event, func(), error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.channel = channel
	if f.err != nil {
		return nil, nil, f.err
	}
	return f.events, func() {
		f.mu.Lock()
		f.cancelCalls++
		f.mu.Unlock()
	}, nil
}

func (f *fakeSource) cancelled() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cancelCalls
}

func newRequestWithProjectID(ctx context.Context, projectID string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("projectID", projectID)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

func TestStream_DeliversEventThenClosesOnCtxCancel(t *testing.T) {
	src := newFakeSource()
	h := NewHandlerFromSource(src)

	ctx, cancel := context.WithCancel(context.Background())
	rec := httptest.NewRecorder()
	req := newRequestWithProjectID(ctx, "42")

	done := make(chan struct{})
	go func() {
		h.Stream(rec, req)
		close(done)
	}()

	// Push an event, then cancel the request context to end the stream.
	src.events <- natsbus.Event{Type: "message.created", Payload: json.RawMessage(`{"n":1}`)}
	// Give the handler a moment to write it before we cancel.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stream did not return after ctx cancel")
	}

	if src.channel != "project:42:events" {
		t.Errorf("subscribed channel = %q, want project:42:events", src.channel)
	}
	if src.cancelled() != 1 {
		t.Errorf("cancel func called %d times, want 1", src.cancelled())
	}

	body := rec.Body.String()
	if !strings.Contains(body, ": connected") {
		t.Errorf("missing connected comment; body=%q", body)
	}
	if !strings.Contains(body, "event: message.created") || !strings.Contains(body, `data: {"n":1}`) {
		t.Errorf("event not written; body=%q", body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q", ct)
	}
}

func TestStream_ReturnsWhenSourceChannelCloses(t *testing.T) {
	src := newFakeSource()
	h := NewHandlerFromSource(src)

	rec := httptest.NewRecorder()
	req := newRequestWithProjectID(context.Background(), "7")

	done := make(chan struct{})
	go func() {
		h.Stream(rec, req)
		close(done)
	}()

	close(src.events) // upstream gone → handler should return

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stream did not return when source channel closed")
	}
	if src.cancelled() != 1 {
		t.Errorf("cancel func called %d times, want 1", src.cancelled())
	}
}

func TestStream_SourceError(t *testing.T) {
	src := newFakeSource()
	src.err = errors.New("source down")
	h := NewHandlerFromSource(src)

	rec := httptest.NewRecorder()
	req := newRequestWithProjectID(context.Background(), "1")
	h.Stream(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

func TestStream_StreamingUnsupported(t *testing.T) {
	src := newFakeSource()
	h := NewHandlerFromSource(src)

	// A plain writer with no Flush method → ssewriter.New fails.
	rec := &noFlushWriter{header: http.Header{}}
	req := newRequestWithProjectID(context.Background(), "1")
	h.Stream(rec, req)

	if rec.status != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.status)
	}
}

// noFlushWriter is an http.ResponseWriter WITHOUT an http.Flusher, exercising
// the "streaming not supported" branch of Stream.
type noFlushWriter struct {
	header http.Header
	status int
}

func (n *noFlushWriter) Header() http.Header         { return n.header }
func (n *noFlushWriter) Write(b []byte) (int, error) { return len(b), nil }
func (n *noFlushWriter) WriteHeader(status int)      { n.status = status }

func TestRoutes(t *testing.T) {
	h := NewHandlerFromSource(newFakeSource())
	if h.Routes() == nil {
		t.Fatal("Routes returned nil")
	}
}

// permissionResolverFunc adapts a function to auth.PermissionResolver.
type permissionResolverFunc func(
	context.Context,
	auth.User,
	string,
	string,
) (auth.PermissionResolution, error)

func (f permissionResolverFunc) ResolvePermissions(
	ctx context.Context,
	principal auth.User,
	mode string,
	projectID string,
) (auth.PermissionResolution, error) {
	return f(ctx, principal, mode, projectID)
}

// withTestUser stands in for apimw.Auth, which router.go wraps this mount in.
func withTestUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(auth.ContextWithUser(r.Context(), auth.User{ID: "1"})))
	})
}

// streamRouter reproduces router.go's shape: an authenticated caller, the
// {projectID} in the MOUNT pattern, and the SSE subrouter under it.
func streamRouter(source EventSource, resolver auth.PermissionResolver) *chi.Mux {
	h := NewHandlerFromSource(source, WithPermissionResolver(resolver))
	r := chi.NewRouter()
	r.Use(withTestUser)
	r.Route("/api/v2/events/prompt_lib/{projectID}", func(r chi.Router) {
		r.Mount("/", h.Routes())
	})
	return r
}

const streamPath = "/api/v2/events/prompt_lib/7/"

// refusedStreamRequest is how every REFUSED case below builds its request.
//
// The context carries a short deadline on purpose. A refused request never
// reaches Stream, so the deadline never fires — but a regression that DROPS the
// gate does reach Stream, and Stream then blocks on the event channel until its
// request context ends. Without the deadline that regression hangs the package
// until the test binary times out, which reads as an infrastructure fault
// rather than as the missing gate. With it, the case fails and names the gate.
func refusedStreamRequest(t *testing.T) *http.Request {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	t.Cleanup(cancel)
	return httptest.NewRequest(http.MethodGet, streamPath, nil).WithContext(ctx)
}

/* ── the refused direction ─────────────────────────────────────────────── */

// A caller who does not resolve StreamPermission is refused, and the event
// source is never subscribed to (#496).
//
// The status alone is not enough here. The handler takes over the connection
// with ssewriter.New before it does anything else, so a check placed inside
// Stream would already have opened the stream. Asserting that Raw was never
// called is what proves the refusal happened at the route.
func TestTheProjectStreamIsGated(t *testing.T) {
	source := newFakeSource()
	resolver := permissionResolverFunc(func(
		_ context.Context,
		_ auth.User,
		mode string,
		projectID string,
	) (auth.PermissionResolution, error) {
		if mode != auth.PermissionModeDefault {
			t.Errorf("resolver called in mode %q, want %q", mode, auth.PermissionModeDefault)
		}
		if projectID != "7" {
			t.Errorf("resolver called for project %q, want the {projectID} of the mount", projectID)
		}
		return auth.PermissionResolution{UserID: 1, Permissions: []string{}}, nil
	})

	rec := httptest.NewRecorder()
	streamRouter(source, resolver).ServeHTTP(rec, refusedStreamRequest(t))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d. The project event bus is readable without %s.",
			rec.Code, http.StatusForbidden, StreamPermission)
	}
	source.mu.Lock()
	subscribed := source.channel
	source.mu.Unlock()
	if subscribed != "" {
		t.Fatalf("the refused request still subscribed to %q", subscribed)
	}
}

// A caller with no identity is refused before the resolver is asked.
func TestTheProjectStreamRefusesAnUnauthenticatedCaller(t *testing.T) {
	source := newFakeSource()
	h := NewHandlerFromSource(source, WithPermissionResolver(permissionResolverFunc(func(
		_ context.Context,
		_ auth.User,
		_ string,
		_ string,
	) (auth.PermissionResolution, error) {
		t.Fatal("the resolver must not be asked for an unauthenticated caller")
		return auth.PermissionResolution{}, nil
	})))
	r := chi.NewRouter()
	r.Route("/api/v2/events/prompt_lib/{projectID}", func(r chi.Router) {
		r.Mount("/", h.Routes())
	})

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, refusedStreamRequest(t))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d without a caller, want %d", rec.Code, http.StatusUnauthorized)
	}
}

// A Handler built without WithPermissionResolver streams nothing.
func TestTheProjectStreamFailsClosedWithoutAResolver(t *testing.T) {
	source := newFakeSource()
	r := chi.NewRouter()
	r.Use(withTestUser)
	r.Route("/api/v2/events/prompt_lib/{projectID}", func(r chi.Router) {
		r.Mount("/", NewHandlerFromSource(source).Routes())
	})

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, refusedStreamRequest(t))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d with no resolver composed, want %d", rec.Code, http.StatusForbidden)
	}
	source.mu.Lock()
	subscribed := source.channel
	source.mu.Unlock()
	if subscribed != "" {
		t.Fatalf("a handler with no resolver still subscribed to %q", subscribed)
	}
}

/* ── the entitled direction ────────────────────────────────────────────── */

// A caller who resolves StreamPermission reaches the stream, and reaches the
// channel of the project named in the path.
//
// Without this direction a gate that refuses everyone reads as a working gate,
// which is the shape of #354, #359 and #402. migrations/shared/0062 grants
// models.project_context.view in DEFAULT mode to admin, editor and viewer, and
// events_stream_grant_postgres_integration_test.go measures that on a database
// created EMPTY, so this pass is a pass on a clean deployment too.
func TestTheProjectStreamPassesWithItsPermission(t *testing.T) {
	source := newFakeSource()
	resolver := permissionResolverFunc(func(
		_ context.Context,
		_ auth.User,
		_ string,
		_ string,
	) (auth.PermissionResolution, error) {
		return auth.PermissionResolution{UserID: 1, Permissions: []string{StreamPermission}}, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, streamPath, nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		streamRouter(source, resolver).ServeHTTP(rec, req)
	}()

	deadline := time.After(2 * time.Second)
	for {
		source.mu.Lock()
		subscribed := source.channel
		source.mu.Unlock()
		if subscribed == "project:7:events" {
			break
		}
		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatalf("the entitled caller subscribed to %q, want project:7:events", subscribed)
		default:
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d for an entitled caller, want %d", rec.Code, http.StatusOK)
	}
}

// A caller who holds a different project-scoped permission is still refused.
//
// Without this the gate could be wired to any string and both directions above
// would pass.
func TestTheProjectStreamRefusesADifferentPermission(t *testing.T) {
	source := newFakeSource()
	resolver := permissionResolverFunc(func(
		_ context.Context,
		_ auth.User,
		_ string,
		_ string,
	) (auth.PermissionResolution, error) {
		return auth.PermissionResolution{
			UserID:      1,
			Permissions: []string{"models.notifications.notifications.list"},
		}, nil
	})

	rec := httptest.NewRecorder()
	streamRouter(source, resolver).ServeHTTP(rec, refusedStreamRequest(t))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d for a caller holding an unrelated permission, want %d",
			rec.Code, http.StatusForbidden)
	}
}
