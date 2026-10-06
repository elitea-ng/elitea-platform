package natsbus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

// fakeConn is an in-memory natsConn: Publish fans out to every ChanSubscribe
// whose subject matches (supporting the '*' single-token and '>' multi-token
// wildcards NATS uses), so a publish→subscribe round-trip is testable without a
// live server.
type fakeConn struct {
	mu          sync.Mutex
	subs        []*fakeSub
	published   [][2]string // [subject, data]
	publishErr  error
	flushErr    error
	rttErr      error
	drainErr    error
	drainCalled bool
	closeCalled bool
	subErr      error
	// calls records FlushTimeout/Drain/Close in order (Close tests).
	calls []string
	// closed is what IsClosed reports; drainCloses makes Drain set it, the
	// way a drain that finishes does.
	closed      bool
	drainCloses bool
}

type fakeSub struct {
	subject string
	ch      chan *nats.Msg
	mu      sync.Mutex
	drained bool
}

func (s *fakeSub) Drain() error {
	s.mu.Lock()
	s.drained = true
	s.mu.Unlock()
	return nil
}

func (s *fakeSub) drainCalled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.drained
}

func (f *fakeConn) Publish(subj string, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.publishErr != nil {
		return f.publishErr
	}
	f.published = append(f.published, [2]string{subj, string(data)})
	for _, s := range f.subs {
		if subjectMatches(s.subject, subj) {
			select {
			case s.ch <- &nats.Msg{Subject: subj, Data: data}:
			default:
			}
		}
	}
	return nil
}

func (f *fakeConn) ChanSubscribe(subj string, ch chan *nats.Msg) (subscription, error) {
	if f.subErr != nil {
		return nil, f.subErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	fs := &fakeSub{subject: subj, ch: ch}
	f.subs = append(f.subs, fs)
	return fs, nil
}

func (f *fakeConn) FlushTimeout(time.Duration) error {
	f.record("flush")
	return f.flushErr
}
func (f *fakeConn) RTT() (time.Duration, error) { return time.Millisecond, f.rttErr }
func (f *fakeConn) Drain() error {
	f.record("drain")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.drainCalled = true
	if f.drainErr == nil && f.drainCloses {
		f.closed = true
	}
	return f.drainErr
}
func (f *fakeConn) IsClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}
func (f *fakeConn) Close() {
	f.record("close")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closeCalled = true
	f.closed = true
}
func (f *fakeConn) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

// subjectMatches implements NATS subject matching for tests ('*' one token,
// '>' tail).
func subjectMatches(filter, subject string) bool {
	ft := splitTokens(filter)
	st := splitTokens(subject)
	for i, f := range ft {
		if f == ">" {
			return true
		}
		if i >= len(st) {
			return false
		}
		if f != "*" && f != st[i] {
			return false
		}
	}
	return len(ft) == len(st)
}

func splitTokens(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == '.' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(r)
	}
	return append(out, cur)
}

func TestSubjectFor(t *testing.T) {
	cases := map[string]string{
		"project:123:events": "gateway.events.project.123.events",
		// The presence family is elitea-main's own subject space (#1076).
		"project:123:presence": "elitea.events.project.123.presence",
		"project:*":            "gateway.events.project.>",
		"elitea:*":           "gateway.events.elitea.>",
		"*":                  "gateway.events.>",
		"":                   "gateway.events",
		"simple":             "gateway.events.simple",
		"a:b:c":              "gateway.events.a.b.c",
	}
	for in, want := range cases {
		if got := subjectFor(in); got != want {
			t.Errorf("subjectFor(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPublish_EnvelopeAndSubject(t *testing.T) {
	fc := &fakeConn{}
	eb := New(fc, "elitea-main")

	err := eb.Publish(context.Background(), "project:7:events", "application.created",
		map[string]string{"project_id": "7", "id": "app-1"})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if len(fc.published) != 1 {
		t.Fatalf("expected 1 publish, got %d", len(fc.published))
	}
	subj, data := fc.published[0][0], fc.published[0][1]
	if subj != "gateway.events.project.7.events" {
		t.Errorf("subject = %q", subj)
	}
	var evt Event
	if err := json.Unmarshal([]byte(data), &evt); err != nil {
		t.Fatalf("envelope not decodable: %v", err)
	}
	if evt.Type != "application.created" || evt.Source != "elitea-main" {
		t.Errorf("envelope fields: type=%q source=%q", evt.Type, evt.Source)
	}
	if evt.Timestamp.IsZero() {
		t.Error("timestamp not set")
	}
	var payload map[string]string
	if err := json.Unmarshal(evt.Payload, &payload); err != nil || payload["id"] != "app-1" {
		t.Errorf("payload not preserved: %v (%v)", payload, err)
	}
}

func TestPublish_MarshalError(t *testing.T) {
	eb := New(&fakeConn{}, "src")
	// A channel value is not JSON-marshalable.
	if err := eb.Publish(context.Background(), "c", "t", make(chan int)); err == nil {
		t.Fatal("expected marshal error")
	}
}

func TestPublish_TransportError(t *testing.T) {
	eb := New(&fakeConn{publishErr: errors.New("down")}, "src")
	if err := eb.Publish(context.Background(), "c", "t", nil); err == nil {
		t.Fatal("expected publish error")
	}
}

func TestPublish_FlushError(t *testing.T) {
	eb := New(&fakeConn{flushErr: errors.New("flush timeout")}, "src")
	if err := eb.Publish(context.Background(), "c", "t", nil); err == nil {
		t.Fatal("expected flush error")
	}
}

// A buffered bus never waits on the server: the same flush failure that the
// default bus reports must not reach a request-path caller, and the message
// must still have been handed to the connection.
func TestPublish_BufferedSkipsFlush(t *testing.T) {
	fc := &fakeConn{flushErr: errors.New("flush timeout")}
	eb := New(fc, "src", WithBufferedPublish())
	if err := eb.Publish(context.Background(), "project:7:events", "t", map[string]int{"a": 1}); err != nil {
		t.Fatalf("buffered publish returned %v, want nil", err)
	}
	if len(fc.published) != 1 || fc.published[0][0] != "gateway.events.project.7.events" {
		t.Fatalf("published = %v, want one message on gateway.events.project.7.events", fc.published)
	}
	// A transport error is still reported.
	failing := New(&fakeConn{publishErr: errors.New("closed")}, "src", WithBufferedPublish())
	if err := failing.Publish(context.Background(), "c", "t", nil); err == nil {
		t.Fatal("buffered publish swallowed a transport error")
	}
}

func TestSubscribe_RoundTrip(t *testing.T) {
	fc := &fakeConn{}
	eb := New(fc, "src")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	got := make(chan Event, 1)
	// Subscribe to the "project:*" catch-all → gateway.events.project.>, which
	// matches the published gateway.events.project.9.events subject.
	eb.Subscribe(ctx, "project:*", func(_ context.Context, e Event) error {
		got <- e
		return nil
	})
	// Give the subscription goroutine time to register.
	waitForSubs(t, fc, 1)

	if err := eb.Publish(ctx, "project:9:events", "message.created", map[string]int{"n": 1}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	select {
	case e := <-got:
		if e.Type != "message.created" {
			t.Errorf("type = %q", e.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler not invoked (wildcard match failed)")
	}
}

func TestSubscribe_MalformedSkippedHandlerErrorLogged(t *testing.T) {
	fc := &fakeConn{}
	eb := New(fc, "src")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var calls int
	var mu sync.Mutex
	eb.Subscribe(ctx, "c", func(_ context.Context, _ Event) error {
		mu.Lock()
		calls++
		mu.Unlock()
		return errors.New("handler boom") // logged, must not stop the loop
	})
	waitForSubs(t, fc, 1)

	// Inject a malformed message directly — skipped, no handler call.
	fc.mu.Lock()
	sub := fc.subs[0]
	fc.mu.Unlock()
	sub.ch <- &nats.Msg{Subject: "gateway.events.c", Data: []byte("{not json")}

	// Then a valid one — handler called despite the previous error.
	_ = eb.Publish(ctx, "c", "ok", nil)
	_ = eb.Publish(ctx, "c", "ok2", nil)

	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		c := calls
		mu.Unlock()
		if c >= 2 {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("expected >=2 handler calls, got %d", c)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestSubscribe_SubError(t *testing.T) {
	fc := &fakeConn{subErr: errors.New("no sub")}
	eb := New(fc, "src")
	// Must not panic; simply logs and returns.
	eb.Subscribe(context.Background(), "c", func(context.Context, Event) error { return nil })
}

func TestSubscribe_CtxCancelStops(t *testing.T) {
	fc := &fakeConn{}
	eb := New(fc, "src")
	ctx, cancel := context.WithCancel(context.Background())
	eb.Subscribe(ctx, "c", func(context.Context, Event) error { return nil })
	waitForSubs(t, fc, 1)
	fc.mu.Lock()
	sub := fc.subs[0]
	fc.mu.Unlock()
	cancel()
	// The subscription (not the connection) is drained on exit; give the
	// goroutine a moment.
	deadline := time.After(2 * time.Second)
	for {
		if sub.drainCalled() {
			return
		}
		select {
		case <-deadline:
			t.Fatal("expected subscription drain on ctx cancel")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestRaw_RoundTripAndCancel(t *testing.T) {
	fc := &fakeConn{}
	eb := New(fc, "src")
	ctx := context.Background()

	out, cancel, err := eb.Raw(ctx, "project:1:events")
	if err != nil {
		t.Fatalf("Raw: %v", err)
	}
	waitForSubs(t, fc, 1)

	_ = eb.Publish(ctx, "project:1:events", "conversation.created", map[string]string{"x": "y"})

	select {
	case e := <-out:
		if e.Type != "conversation.created" {
			t.Errorf("type = %q", e.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event on Raw channel")
	}

	cancel()
	cancel() // idempotent — must not panic on double close
	select {
	case _, ok := <-out:
		if ok {
			// May receive one more buffered value; drain then expect close.
			select {
			case _, ok2 := <-out:
				if ok2 {
					t.Error("channel not closed after cancel")
				}
			case <-time.After(time.Second):
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("channel not closed after cancel")
	}
}

func TestRaw_MalformedSkipped(t *testing.T) {
	fc := &fakeConn{}
	eb := New(fc, "src")
	out, cancel, err := eb.Raw(context.Background(), "c")
	if err != nil {
		t.Fatalf("Raw: %v", err)
	}
	defer cancel()
	waitForSubs(t, fc, 1)

	fc.mu.Lock()
	sub := fc.subs[0]
	fc.mu.Unlock()
	sub.ch <- &nats.Msg{Data: []byte("garbage")}
	_ = eb.Publish(context.Background(), "c", "good", nil)

	select {
	case e := <-out:
		if e.Type != "good" {
			t.Errorf("expected the valid event through, got %q", e.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("valid event not delivered after malformed one")
	}
}

func TestRaw_SubError(t *testing.T) {
	eb := New(&fakeConn{subErr: errors.New("x")}, "src")
	if _, _, err := eb.Raw(context.Background(), "c"); err == nil {
		t.Fatal("expected Raw subscribe error")
	}
}

func TestPing(t *testing.T) {
	if err := New(&fakeConn{}, "s").Ping(context.Background()); err != nil {
		t.Fatalf("Ping ok: %v", err)
	}
	if err := New(&fakeConn{rttErr: errors.New("no rtt")}, "s").Ping(context.Background()); err == nil {
		t.Fatal("expected Ping error")
	}
}

func TestClose(t *testing.T) {
	// Clean shutdown: flush, then drain, and the drain closes the connection
	// itself — no forced Close().
	fc := &fakeConn{drainCloses: true}
	New(fc, "s").Close()
	if got := strings.Join(fc.calls, ","); got != "flush,drain" {
		t.Errorf("calls = %q, want flush,drain", got)
	}
	// Drain error (nats.go returns one while reconnecting): falls back to Close().
	fc2 := &fakeConn{drainErr: errors.New("drain fail")}
	New(fc2, "s").Close()
	if !fc2.closeCalled {
		t.Error("expected Close() fallback on drain error")
	}
	// A failed flush is logged, not fatal: the drain still runs.
	fc3 := &fakeConn{flushErr: errors.New("flush timeout"), drainCloses: true}
	New(fc3, "s").Close()
	if !fc3.drainCalled || fc3.closeCalled {
		t.Errorf("after a flush error: drain=%v close=%v; want drain only", fc3.drainCalled, fc3.closeCalled)
	}
	// An already-closed connection is left alone.
	fc4 := &fakeConn{closed: true}
	New(fc4, "s").Close()
	if len(fc4.calls) != 0 {
		t.Errorf("calls on a closed connection = %v, want none", fc4.calls)
	}
}

// A drain that never finishes (Drain is async in nats.go) must not hold
// shutdown past the bound: Close() is forced at the deadline.
func TestCloseIsBoundedWhenTheDrainHangs(t *testing.T) {
	fc := &fakeConn{} // Drain succeeds but never closes the connection
	start := time.Now()
	New(fc, "s").closeWithin(100 * time.Millisecond)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("closeWithin took %v, want it bounded near 100ms", elapsed)
	}
	if got := strings.Join(fc.calls, ","); got != "flush,drain,close" {
		t.Errorf("calls = %q, want flush,drain,close", got)
	}
}

// Against a real server: publishes still in the client's buffer when Close
// is called reach a subscriber on another connection. Under
// WithBufferedPublish nothing else flushes them.
func TestCloseDeliversBufferedPublishes(t *testing.T) {
	url := os.Getenv("ELITEA_TEST_NATS_URL")
	if url == "" {
		t.Skip("set ELITEA_TEST_NATS_URL to run the NATS shutdown test")
	}
	sub, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect subscriber: %v", err)
	}
	defer sub.Close()
	channel := fmt.Sprintf("project:%d:events", time.Now().UnixNano())
	msgs := make(chan *nats.Msg, 1024)
	if _, err := sub.ChanSubscribe(subjectFor(channel), msgs); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := sub.Flush(); err != nil {
		t.Fatalf("flush subscription: %v", err)
	}

	// A large flusher interval keeps nats.go's background flusher out of the
	// way, so the bytes are still buffered when Close runs.
	pub, err := nats.Connect(url, nats.FlusherTimeout(time.Minute))
	if err != nil {
		t.Fatalf("connect publisher: %v", err)
	}
	eb := NewFromConn(pub, "test", WithBufferedPublish())
	const n = 200
	for i := 0; i < n; i++ {
		if err := eb.Publish(context.Background(), channel, "budget.soft_alert", map[string]int{"i": i}); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}
	eb.Close()
	if !pub.IsClosed() {
		t.Fatal("Close returned with the connection still open")
	}

	deadline := time.After(5 * time.Second)
	for got := 0; got < n; {
		select {
		case <-msgs:
			got++
		case <-deadline:
			t.Fatalf("only %d of %d buffered publishes reached the server", got, n)
		}
	}
}

func TestConnect_BadURL(t *testing.T) {
	// An unroutable URL fails the 1s dial — exercises the error branch of Connect.
	if _, err := Connect("nats://127.0.0.1:1", "test", "test"); err == nil {
		t.Fatal("expected connect error to unreachable server")
	}
}

func waitForSubs(t *testing.T, fc *fakeConn, n int) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		fc.mu.Lock()
		got := len(fc.subs)
		fc.mu.Unlock()
		if got >= n {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("expected %d subscriptions, got %d", n, got)
		case <-time.After(5 * time.Millisecond):
		}
	}
}
