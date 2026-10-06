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

// fakeSource is an in-memory EventSource: one send channel per logical
// channel the handler subscribes to, and a record of what it subscribed to.
// It lets the SSE Stream handler be exercised without a live NATS server.
type fakeSource struct {
	mu          sync.Mutex
	channel     string   // the first channel subscribed to
	channels    []string // every channel subscribed to, in order
	streams     map[string]chan natsbus.Event
	err         error
	cancelCalls int
}

func newFakeSource() *fakeSource {
	return &fakeSource{streams: map[string]chan natsbus.Event{}}
}

// stream is the send side of one logical channel.
func (f *fakeSource) stream(channel string) chan natsbus.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch, ok := f.streams[channel]
	if !ok {
		ch = make(chan natsbus.Event, 8)
		f.streams[channel] = ch
	}
	return ch
}

func (f *fakeSource) Raw(_ context.Context, channel string) (<-chan natsbus.Event, func(), error) {
	f.mu.Lock()
	if f.channel == "" {
		f.channel = channel
	}
	f.channels = append(f.channels, channel)
	err := f.err
	f.mu.Unlock()
	if err != nil {
		return nil, nil, err
	}
	return f.stream(channel), func() {
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
	src.stream("project:42:events") <- natsbus.Event{Type: "budget.soft_alert", Payload: json.RawMessage(`{"n":1}`)}
	// Give the handler a moment to write it before we cancel.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stream did not return after ctx cancel")
	}

	if got := strings.Join(src.channels, ","); got != "project:42:events,project:42:presence" {
		t.Errorf("subscribed channels = %q, want the gateway's and the presence family of project 42", got)
	}
	if src.cancelled() != 2 {
		t.Errorf("cancel funcs called %d times, want 2 (one per family)", src.cancelled())
	}

	body := rec.Body.String()
	if !strings.Contains(body, ": connected") {
		t.Errorf("missing connected comment; body=%q", body)
	}
	if !strings.Contains(body, "event: budget.soft_alert") || !strings.Contains(body, `data: {"n":1}`) {
		t.Errorf("event not written; body=%q", body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q", ct)
	}
}

// Each family forwards its ONE type (#1076). Domain events (conversation.created
// carries a private conversation's name and creator) and forged frames are
// dropped, payload and all — and so is a frame of the OTHER family's type: a
// canvas.editors roster on the gateway's subject (the gateway forging presence
// into a project's stream) and a budget.soft_alert on the presence subject.
// The allowed frames around them still arrive in order.
func TestStream_ForwardsEachTypeOnlyFromItsOwnFamily(t *testing.T) {
	src := newFakeSource()
	h := NewHandlerFromSource(src)

	rec := &lockedRecorder{ResponseRecorder: httptest.NewRecorder()}
	req := newRequestWithProjectID(context.Background(), "42")
	done := make(chan struct{})
	go func() {
		h.Stream(rec, req)
		close(done)
	}()

	gateway := []natsbus.Event{
		{Type: "conversation.created", Payload: json.RawMessage(`{"name":"secret-conversation","created_by":9}`)},
		{Type: "canvas.editors", Payload: json.RawMessage(`{"editors":["secret-forged-by-the-gateway"]}`)},
		{Type: "", Payload: json.RawMessage(`{"forged":"secret-empty-type"}`)},
		{Type: "budget.soft_alert ", Payload: json.RawMessage(`{"forged":"secret-near-miss"}`)},
		{Type: "budget.soft_alert", Payload: json.RawMessage(`{"cost_just_billed_nano":5}`)},
	}
	presence := []natsbus.Event{
		{Type: "artifact.uploaded", Payload: json.RawMessage(`{"object":"secret-file"}`)},
		{Type: "budget.soft_alert", Payload: json.RawMessage(`{"forged":"secret-alert-on-presence"}`)},
		{Type: "canvas.editors", Payload: json.RawMessage(`{"editors":[]}`)},
	}
	// Feed one family, wait for its allowed frame, then the other: the two
	// subscriptions are concurrent, so order is only defined within one.
	for _, frame := range gateway {
		src.stream("project:42:events") <- frame
	}
	waitForBody(t, rec, "event: budget.soft_alert\n")
	for _, frame := range presence {
		src.stream("project:42:presence") <- frame
	}
	waitForBody(t, rec, "event: canvas.editors")
	close(src.stream("project:42:presence"))

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stream did not return after the source closed")
	}

	body := rec.Body.String()
	if strings.Contains(body, "secret") {
		t.Fatalf("a frame outside its family's allowlist reached the client; body=%q", body)
	}
	for _, unwanted := range []string{"conversation.created", "artifact.uploaded"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("event %q reached the client; body=%q", unwanted, body)
		}
	}
	if strings.Count(body, "event: canvas.editors") != 1 || strings.Count(body, "event: budget.soft_alert\n") != 1 {
		t.Fatalf("want exactly one roster and one alert; body=%q", body)
	}
}

// lockedRecorder lets a test read the SSE body while Stream is still writing.
type lockedRecorder struct {
	mu sync.Mutex
	*httptest.ResponseRecorder
}

func (l *lockedRecorder) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.ResponseRecorder.Write(b)
}

func (l *lockedRecorder) Flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ResponseRecorder.Flush()
}

func (l *lockedRecorder) body() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.Body.String()
}

// waitForBody waits until the recorded SSE body contains want.
func waitForBody(t *testing.T, rec *lockedRecorder, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if strings.Contains(rec.body(), want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%q never reached the client; body=%q", want, rec.body())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestForwardedIsExactlyPresenceAndSoftAlert(t *testing.T) {
	for _, eventType := range []string{"canvas.editors", "budget.soft_alert"} {
		if !Forwarded(eventType) {
			t.Errorf("Forwarded(%q) = false, want true", eventType)
		}
	}
	for _, eventType := range []string{
		"conversation.created", "artifact.uploaded", "pipeline.run.started",
		"pipeline.run.succeeded", "pipeline.run.failed", "schedule.fired",
		"moderation.request.decided", "agent.version.published", "", "canvas.*",
	} {
		if Forwarded(eventType) {
			t.Errorf("Forwarded(%q) = true; every forwarded type is readable by any project viewer", eventType)
		}
	}
	if len(streamFamilies) != 2 {
		t.Errorf("streamFamilies has %d entries, want 2; widening it is a privacy decision — update this test deliberately", len(streamFamilies))
	}
	// And each from its own family only.
	for _, c := range []struct {
		channel, eventType string
		want               bool
	}{
		{"project:7:presence", "canvas.editors", true},
		{"project:7:events", "budget.soft_alert", true},
		{"project:7:events", "canvas.editors", false},
		{"project:7:presence", "budget.soft_alert", false},
		{"project:8:presence", "canvas.editors", false},
		{"project:7:other", "canvas.editors", false},
	} {
		if got := ForwardedOn("7", c.channel, c.eventType); got != c.want {
			t.Errorf("ForwardedOn(7, %q, %q) = %v, want %v", c.channel, c.eventType, got, c.want)
		}
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

	close(src.stream("project:7:events")) // upstream gone → handler should return

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stream did not return when source channel closed")
	}
	if src.cancelled() != 2 {
		t.Errorf("cancel funcs called %d times, want 2", src.cancelled())
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
