package epoch

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime/securitycontext"
)

const (
	idA = "0192b6e4-0000-7000-8000-0000000000a1"
	idB = "0192b6e4-0000-7000-8000-0000000000b1"
)

var t0 = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

func TestStale(t *testing.T) {
	keys := []SecurityEpoch{
		{Kind: KindBinding, ID: idA, Value: 2},
		{Kind: KindMandate, ID: idB, Value: 5},
		{Kind: KindExternalIdentity, ID: idA, Value: Unpinned},
	}
	current := []SecurityEpoch{
		{Kind: KindBinding, ID: idA, Value: 2},
		{Kind: KindMandate, ID: idB, Value: 6},
		{Kind: KindExternalIdentity, ID: idA, Value: 9},
	}
	stale := Stale(keys, current)
	if len(stale) != 1 || stale[0] != keys[1] {
		t.Fatalf("stale = %+v", stale)
	}
	current[2].Value = Revoked
	if got := Stale(keys, current); len(got) != 2 {
		t.Fatalf("an inactive unpinned record is stale: %+v", got)
	}
	if got := Stale(keys, current[:1]); len(got) != 2 {
		t.Fatalf("a short answer leaves the rest stale: %+v", got)
	}
}

func TestEqualAndContains(t *testing.T) {
	a := []SecurityEpoch{{Kind: KindBinding, ID: idA, Value: 1}, {Kind: KindGrant, ID: idB, Value: 2}}
	b := []SecurityEpoch{a[1], a[0]}
	if !Equal(a, b) || Equal(a, a[:1]) || Equal(a, []SecurityEpoch{a[0], {Kind: KindGrant, ID: idB, Value: 3}}) {
		t.Fatal("Equal is not order-insensitive set equality")
	}
	if !Contains(a, SecurityEpoch{Kind: KindBinding, ID: idA, Value: 2}) || Contains(a, SecurityEpoch{Kind: KindBinding, ID: idA, Value: 1}) ||
		!Contains(a, SecurityEpoch{Kind: KindGrant, ID: idB, Value: Unpinned}) {
		t.Fatal("Contains must match only an older epoch of the same record")
	}
}

func TestWellFormed(t *testing.T) {
	for _, e := range []SecurityEpoch{
		{Kind: "other", ID: idA}, {Kind: KindBinding, ID: "not-a-uuid"}, {Kind: KindBinding, ID: idA, Value: -5},
	} {
		if e.WellFormed() {
			t.Fatalf("%+v is well formed", e)
		}
	}
	if !(SecurityEpoch{Kind: KindBinding, ID: idA, Value: Unpinned}).WellFormed() {
		t.Fatal("an unpinned key is well formed")
	}
}

func TestPostgresStoreRefusesMalformedKeysWithoutAStatement(t *testing.T) {
	s, err := NewPostgresStore(failingQuerier{t: t})
	if err != nil {
		t.Fatal(err)
	}
	if out, err := s.Read(context.Background(), nil); err != nil || out != nil {
		t.Fatal("no key must send no statement")
	}
	if _, err := s.Read(context.Background(), []SecurityEpoch{{Kind: KindBinding, ID: "x"}}); !errors.Is(err, ErrInvalidKeys) {
		t.Fatalf("err = %v", err)
	}
	many := make([]SecurityEpoch, MaxKeys+1)
	for i := range many {
		many[i] = SecurityEpoch{Kind: KindBinding, ID: idA}
	}
	if _, err := s.Read(context.Background(), many); !errors.Is(err, ErrInvalidKeys) {
		t.Fatalf("err = %v", err)
	}
}

// fakeTransport loops a bus between hubs.
type fakeTransport struct {
	mu        sync.Mutex
	listeners []func([]byte)
	published chan []byte
}

func newFakeTransport() *fakeTransport { return &fakeTransport{published: make(chan []byte, 16)} }

func (f *fakeTransport) Publish(data []byte) error {
	f.mu.Lock()
	listeners := append([]func([]byte){}, f.listeners...)
	f.mu.Unlock()
	for _, l := range listeners {
		l(data)
	}
	f.published <- data
	return nil
}

func (f *fakeTransport) Subscribe(deliver func([]byte)) (func() error, error) {
	f.mu.Lock()
	f.listeners = append(f.listeners, deliver)
	f.mu.Unlock()
	return func() error { return nil }, nil
}

func newTestHub(t *testing.T, reader *sdkmetric.ManualReader, now func() time.Time) *Hub {
	t.Helper()
	opts := HubOptions{Now: now}
	if reader != nil {
		opts.Meter = sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test")
	}
	h, err := NewHub(opts)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// A change on one replica reaches the other replica's listeners by
// reference only, and never loops back to its origin.
func TestHubFansOutByReferenceAcrossReplicas(t *testing.T) {
	bus := newFakeTransport()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a, b := newTestHub(t, nil, nil), newTestHub(t, nil, nil)
	gotA, gotB := make(chan Change, 4), make(chan Change, 4)
	a.Listen(func(c Change) { gotA <- c })
	b.Listen(func(c Change) { gotB <- c })
	go func() { _ = a.Run(ctx, bus) }()
	go func() { _ = b.Run(ctx, bus) }()
	waitFor(t, func() bool { bus.mu.Lock(); defer bus.mu.Unlock(); return len(bus.listeners) == 2 })

	change := Change{Epoch: SecurityEpoch{Kind: KindMandate, ID: idA, Value: 6}, ChangedAt: t0}
	a.Notify(ctx, change)
	if got := <-gotA; got.Epoch != change.Epoch {
		t.Fatalf("local delivery %+v", got)
	}
	select {
	case got := <-gotB:
		if got.Epoch != change.Epoch || !got.ChangedAt.Equal(t0) {
			t.Fatalf("remote delivery %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the other replica never heard of the change")
	}
	raw := <-bus.published
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	for key := range fields {
		switch key {
		case "v", "origin", "kind", "id", "epoch", "changed_at_ms":
		default:
			t.Fatalf("the message carries %q: %s", key, raw)
		}
	}
	select {
	case echo := <-gotA:
		t.Fatalf("the origin heard its own message back: %+v", echo)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestHubDropsMalformedMessages(t *testing.T) {
	h := newTestHub(t, nil, nil)
	got := 0
	h.Listen(func(Change) { got++ })
	for _, raw := range []string{
		``, `{`, `{"v":2,"origin":"x","kind":"binding","id":"` + idA + `","epoch":1}`,
		`{"v":1,"origin":"x","kind":"secret","id":"` + idA + `","epoch":1}`,
		`{"v":1,"origin":"x","kind":"binding","id":"nope","epoch":1}`,
	} {
		h.receive([]byte(raw))
	}
	if got != 0 {
		t.Fatalf("%d malformed messages delivered", got)
	}
}

// The revocation-to-deny latency is observed once per change, from the
// change time the message carried.
func TestRevocationToDenyLatencyIsObservedOnce(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	now := t0.Add(1500 * time.Millisecond)
	h := newTestHub(t, reader, func() time.Time { return now })
	h.receive([]byte(`{"v":1,"origin":"other","kind":"mandate","id":"` + idA + `","epoch":6,"changed_at_ms":` +
		jsonInt(t0.UnixMilli()) + `}`))
	stale := []SecurityEpoch{{Kind: KindMandate, ID: idA, Value: 5}}
	h.ObserveDenied(context.Background(), stale)
	h.ObserveDenied(context.Background(), stale)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	var latencyCount uint64
	var latencySum float64
	denials := map[string]int64{}
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			switch data := m.Data.(type) {
			case metricdata.Histogram[float64]:
				if m.Name == metricRevocationLatency {
					for _, p := range data.DataPoints {
						latencyCount += p.Count
						latencySum += p.Sum
						if p.Attributes.Len() != 0 {
							t.Fatalf("latency carries labels: %v", p.Attributes)
						}
					}
				}
			case metricdata.Sum[int64]:
				if m.Name == metricRevocationDenials {
					for _, p := range data.DataPoints {
						v, _ := p.Attributes.Value("change_time")
						denials[v.AsString()] += p.Value
					}
				}
			}
		}
	}
	if latencyCount != 1 || latencySum != 1.5 {
		t.Fatalf("latency count %d sum %v", latencyCount, latencySum)
	}
	if denials["known"] != 1 || denials["unknown"] != 1 {
		t.Fatalf("denials %v", denials)
	}
}

func jsonInt(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition never held")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type memLoader struct {
	stored map[string]StoredContext
	loads  int
}

func (m *memLoader) LoadSecurityContext(_ context.Context, id string) (StoredContext, bool, error) {
	m.loads++
	s, ok := m.stored[id]
	return s, ok, nil
}

type countingChecker struct {
	stale []SecurityEpoch
	keys  []SecurityEpoch
	calls int
}

func (c *countingChecker) Current(_ context.Context, keys []SecurityEpoch) ([]SecurityEpoch, error) {
	c.calls++
	c.keys = keys
	return c.stale, nil
}

// A governed run's keys come from its stored context, read once; a legacy
// run (no context) is remembered too and never checked.
func TestRunCheckerReadsTheContextOnce(t *testing.T) {
	encoded, ref, err := securitycontext.Context{
		PolicyRevisionID: "r1", PolicyDigest: runtimedomain.SHA256([]byte("p")), TenantProjectID: 7,
		AgentBindingID: idA, AgentBindingEpoch: 3, MandateID: idB, MandateEpoch: 4, IssuedAt: t0,
		DefinitionDigest: runtimedomain.Digest{9},
	}.Marshal("exec-governed")
	if err != nil {
		t.Fatal(err)
	}
	loader := &memLoader{stored: map[string]StoredContext{"exec-governed": {Bytes: encoded, Digest: ref.Digest}}}
	checker := &countingChecker{}
	runs, err := NewRunChecker(loader, checker)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		governed, stale, err := runs.Check(context.Background(), "exec-governed")
		if err != nil || !governed || stale != nil {
			t.Fatalf("governed=%t stale=%v err=%v", governed, stale, err)
		}
	}
	if loader.loads != 1 || checker.calls != 2 || len(checker.keys) != 2 ||
		checker.keys[0] != (SecurityEpoch{Kind: KindBinding, ID: idA, Value: 3}) ||
		checker.keys[1] != (SecurityEpoch{Kind: KindMandate, ID: idB, Value: 4}) {
		t.Fatalf("loads %d checks %d keys %+v", loader.loads, checker.calls, checker.keys)
	}
	for range 2 {
		governed, _, err := runs.Check(context.Background(), "exec-legacy")
		if err != nil || governed {
			t.Fatalf("legacy: governed=%t err=%v", governed, err)
		}
	}
	if loader.loads != 2 || checker.calls != 2 {
		t.Fatalf("legacy run: loads %d checks %d", loader.loads, checker.calls)
	}

	loader.stored["exec-tampered"] = StoredContext{Bytes: encoded, Digest: [32]byte{1}}
	if _, _, err := runs.Check(context.Background(), "exec-tampered"); !errors.Is(err, ErrContextInvalid) {
		t.Fatalf("a context that does not verify: %v", err)
	}
}

type failingQuerier struct{ t *testing.T }

func (q failingQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	q.t.Fatal("a statement was sent")
	return nil, nil
}
