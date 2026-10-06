package nats

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/sony/gobreaker/v2"
)

// --- fakes for the narrow operation seams -----------------------------------

type fakePublisher struct {
	mu      sync.Mutex
	calls   int
	lastMsg *nats.Msg
	ackVal  string
	dup     bool // PubAck.Duplicate — models a dedup-window suppression
	err     error
	block   time.Duration // simulate a slow/partitioned server
	ctxSaw  error         // records ctx.Err() observed inside the op
}

func (f *fakePublisher) PublishMsg(ctx context.Context, m *nats.Msg, _ ...jetstream.PublishOpt) (*jetstream.PubAck, error) {
	f.mu.Lock()
	f.calls++
	f.lastMsg = m
	block, err, ackVal, dup := f.block, f.err, f.ackVal, f.dup
	f.mu.Unlock()
	if block > 0 {
		select {
		case <-time.After(block):
		case <-ctx.Done():
			f.mu.Lock()
			f.ctxSaw = ctx.Err()
			f.mu.Unlock()
			return nil, ctx.Err()
		}
	}
	if err != nil {
		return nil, err
	}
	return &jetstream.PubAck{Value: ackVal, Duplicate: dup}, nil
}

func (f *fakePublisher) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type fakeReader struct {
	mu    sync.Mutex
	raw   *jetstream.RawStreamMsg
	err   error
	block time.Duration // simulate a slow read; blocks up to this duration (honours ctx)
}

func (f *fakeReader) GetLastMsgForSubject(ctx context.Context, _ string) (*jetstream.RawStreamMsg, error) {
	f.mu.Lock()
	block, err, raw := f.block, f.err, f.raw
	f.mu.Unlock()

	if block > 0 {
		select {
		case <-time.After(block):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err != nil {
		return nil, err
	}
	return raw, nil
}

type fakeKV struct {
	existing map[string]bool
	err      error
	mu       sync.Mutex
}

func (f *fakeKV) Create(_ context.Context, key string, _ []byte, _ ...jetstream.KVCreateOpt) (uint64, error) {
	if f.err != nil {
		return 0, f.err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.existing == nil {
		f.existing = map[string]bool{}
	}
	if f.existing[key] {
		return 0, jetstream.ErrKeyExists
	}
	f.existing[key] = true
	return 1, nil
}

// fakeConn is a stand-in for *nats.Conn used to exercise Close().
type fakeConn struct {
	closed bool
	closes int

	// core-publish recording (PublishSoftAlertEvent surface).
	published  []fakePub
	publishErr error
	flushErr   error
	flushes    int
}

type fakePub struct {
	subject string
	data    []byte
}

func (f *fakeConn) Close()         { f.closed = true; f.closes++ }
func (f *fakeConn) IsClosed() bool { return f.closed }

func (f *fakeConn) Publish(subj string, data []byte) error {
	if f.publishErr != nil {
		return f.publishErr
	}
	f.published = append(f.published, fakePub{subject: subj, data: data})
	return nil
}

func (f *fakeConn) FlushTimeout(time.Duration) error {
	f.flushes++
	return f.flushErr
}

// fakeBinder exercises bindAssets without a live server: each stream is a
// stub whose CachedInfo carries the configuration under test.
type fakeBinder struct {
	streams map[string]jetstream.StreamConfig
	kvTTL   time.Duration
	noKV    bool
	err     error
}

func (f *fakeBinder) Stream(_ context.Context, name string) (jetstream.Stream, error) {
	if f.err != nil {
		return nil, f.err
	}
	cfg, ok := f.streams[name]
	if !ok {
		return nil, jetstream.ErrStreamNotFound
	}
	return stubStream{info: &jetstream.StreamInfo{Config: cfg}}, nil
}

func (f *fakeBinder) KeyValue(_ context.Context, bucket string) (jetstream.KeyValue, error) {
	if f.noKV {
		return nil, jetstream.ErrBucketNotFound
	}
	return stubKV{ttl: f.kvTTL}, nil
}

// bootstrapStreams is what deploy/helm/nats-bootstrap/files/bootstrap.sh
// creates, reduced to the properties bindAssets checks.
func bootstrapStreams() map[string]jetstream.StreamConfig {
	return map[string]jetstream.StreamConfig{
		BudgetStream: {
			Name: BudgetStream, Subjects: []string{"gateway.budget.counter.>"},
			AllowMsgCounter: true, AllowDirect: true, Duplicates: 12 * time.Minute,
		},
		RateLimitStream: {
			Name: RateLimitStream, Subjects: []string{"gateway.ratelimit.counter.>"},
			AllowMsgCounter: true, AllowDirect: true, MaxAge: 5 * time.Minute,
		},
		DeltasStream: {
			Name: DeltasStream, Subjects: []string{DeltaSubject}, Duplicates: 12 * time.Minute,
		},
	}
}

func goodBinder() *fakeBinder {
	return &fakeBinder{streams: bootstrapStreams(), kvTTL: 4 * time.Hour}
}

type stubStream struct {
	jetstream.Stream
	info *jetstream.StreamInfo
}

func (s stubStream) CachedInfo() *jetstream.StreamInfo { return s.info }

// stubKV satisfies jetstream.KeyValue enough for bindAssets; only Status and
// Create are reachable.
type stubKV struct {
	jetstream.KeyValue
	ttl time.Duration
}

func (k stubKV) Status(context.Context) (jetstream.KeyValueStatus, error) {
	return stubKVStatus{ttl: k.ttl}, nil
}

type stubKVStatus struct {
	jetstream.KeyValueStatus
	ttl time.Duration
}

func (s stubKVStatus) TTL() time.Duration { return s.ttl }

// newTestClient builds a Client wired to fakes with a low failure threshold so
// breaker behaviour is exercisable in a unit test.
func newTestClient(cfg Config, pub publisher, rd counterReader, kv kvCreator) *Client {
	cfg = cfg.withDefaults()
	c := &Client{cfg: cfg, pub: pub, budget: rd, cooldown: kv}
	c.breaker = newBreaker(cfg, func(from, to gobreaker.State) {
		c.mu.RLock()
		fn := c.onStateChange
		c.mu.RUnlock()
		if fn != nil {
			fn(from, to)
		}
	})
	return c
}

// --- tests ------------------------------------------------------------------

func TestConfigDefaults(t *testing.T) {
	c := Config{}.withDefaults()
	if c.Name != "elitea-llm-gateway" {
		t.Errorf("Name default = %q", c.Name)
	}
	if c.CBFailureThreshold != 3 {
		t.Errorf("CBFailureThreshold default = %d, want 3", c.CBFailureThreshold)
	}
	if c.CBOpenDuration != 10*time.Second {
		t.Errorf("CBOpenDuration default = %v, want 10s", c.CBOpenDuration)
	}
}

func TestConnectEmptyURL(t *testing.T) {
	if _, err := Connect(context.Background(), Config{}); err == nil {
		t.Fatal("Connect with empty URL should error")
	}
}

func TestBudgetSubject(t *testing.T) {
	got := BudgetSubject("project", "42", 1700000000)
	want := "gateway.budget.counter.project.42.1700000000"
	if got != want {
		t.Errorf("BudgetSubject = %q, want %q", got, want)
	}
}

func TestOpTimeoutIs150ms(t *testing.T) {
	// The hardening contract: OpTimeout is exactly 150ms and ConnectTimeout 1s.
	if OpTimeout != 150*time.Millisecond {
		t.Errorf("OpTimeout = %v, want 150ms", OpTimeout)
	}
	if ConnectTimeout != 1*time.Second {
		t.Errorf("ConnectTimeout = %v, want 1s", ConnectTimeout)
	}
}

func TestIncrBudgetReturnsRunningTotal(t *testing.T) {
	pub := &fakePublisher{ackVal: "1500000000"} // 1.5 USD in nano
	c := newTestClient(Config{}, pub, nil, nil)
	got, err := c.IncrBudget(context.Background(), "gateway.budget.counter.project.1.100", 500000000)
	if err != nil {
		t.Fatalf("IncrBudget: %v", err)
	}
	if got != 1500000000 {
		t.Errorf("total = %d, want 1500000000", got)
	}
	// The Nats-Incr header MUST carry the delta.
	if h := pub.lastMsg.Header.Get(IncrHeader); h != "500000000" {
		t.Errorf("Nats-Incr header = %q, want 500000000", h)
	}
}

func TestIncrBudgetNegativeDelta(t *testing.T) {
	pub := &fakePublisher{ackVal: "0"}
	c := newTestClient(Config{}, pub, nil, nil)
	if _, err := c.IncrBudget(context.Background(), "s", -250); err != nil {
		t.Fatalf("IncrBudget negative: %v", err)
	}
	if h := pub.lastMsg.Header.Get(IncrHeader); h != "-250" {
		t.Errorf("Nats-Incr header = %q, want -250", h)
	}
}

func TestIncrBudgetBadAckValueIsConfigError(t *testing.T) {
	// A non-counter stream returns an empty/blank val — a config error, mapped
	// through but NOT masked as ErrUnavailable.
	pub := &fakePublisher{ackVal: ""}
	c := newTestClient(Config{}, pub, nil, nil)
	_, err := c.IncrBudget(context.Background(), "s", 1)
	if err == nil {
		t.Fatal("expected error on empty counter ack")
	}
	if errors.Is(err, ErrUnavailable) {
		t.Error("empty ack must not be mapped to ErrUnavailable (it is a config error)")
	}
}

func TestReadBudgetMissingCounterIsZero(t *testing.T) {
	rd := &fakeReader{err: jetstream.ErrMsgNotFound}
	c := newTestClient(Config{}, nil, rd, nil)
	got, err := c.ReadBudget(context.Background(), "s")
	if err != nil {
		t.Fatalf("ReadBudget: %v", err)
	}
	if got != 0 {
		t.Errorf("missing counter = %d, want 0", got)
	}
}

func TestReadBudgetParsesPayload(t *testing.T) {
	rd := &fakeReader{raw: &jetstream.RawStreamMsg{Data: []byte("9223372036854775807")}}
	c := newTestClient(Config{}, nil, rd, nil)
	got, err := c.ReadBudget(context.Background(), "s")
	if err != nil {
		t.Fatalf("ReadBudget: %v", err)
	}
	if got != 9223372036854775807 {
		t.Errorf("total = %d, want max int64", got)
	}
}

func TestCounterValueEmpty(t *testing.T) {
	v, err := counterValue(&jetstream.RawStreamMsg{})
	if err != nil || v != 0 {
		t.Errorf("counterValue(empty) = %d, %v; want 0, nil", v, err)
	}
	if v, err := counterValue(nil); err != nil || v != 0 {
		t.Errorf("counterValue(nil) = %d, %v; want 0, nil", v, err)
	}
}

func TestCounterValueGarbage(t *testing.T) {
	if _, err := counterValue(&jetstream.RawStreamMsg{Data: []byte("not-a-number")}); err == nil {
		t.Error("counterValue should error on non-numeric payload")
	}
}

func TestTryAlertCooldownFirstFiresThenSuppresses(t *testing.T) {
	kv := &fakeKV{}
	c := newTestClient(Config{}, nil, nil, kv)
	fire, err := c.TryAlertCooldown(context.Background(), "project:1:80")
	if err != nil || !fire {
		t.Fatalf("first claim: fire=%v err=%v; want true,nil", fire, err)
	}
	fire, err = c.TryAlertCooldown(context.Background(), "project:1:80")
	if err != nil {
		t.Fatalf("second claim err: %v", err)
	}
	if fire {
		t.Error("second claim within cooldown must suppress (fire=false)")
	}
}

func TestPublishDeltaSetsMsgID(t *testing.T) {
	pub := &fakePublisher{ackVal: "0"}
	c := newTestClient(Config{}, pub, nil, nil)
	if err := c.PublishDelta(context.Background(), "evt-123", []byte(`{"x":1}`)); err != nil {
		t.Fatalf("PublishDelta: %v", err)
	}
	if pub.lastMsg.Subject != DeltaSubject {
		t.Errorf("subject = %q, want %q", pub.lastMsg.Subject, DeltaSubject)
	}
	if id := pub.lastMsg.Header.Get(jetstream.MsgIDHeader); id != "evt-123" {
		t.Errorf("Nats-Msg-Id = %q, want evt-123", id)
	}
}

func TestOpTimeoutTripsBreaker(t *testing.T) {
	// A publisher that blocks longer than OpTimeout must surface ErrUnavailable
	// (deadline) rather than hang, and the op's ctx MUST have been cancelled.
	// After CBFailureThreshold consecutive timeout failures the breaker MUST open.
	pub := &fakePublisher{ackVal: "0", block: 500 * time.Millisecond}
	c := newTestClient(Config{CBFailureThreshold: 3}, pub, nil, nil)
	start := time.Now()
	_, err := c.IncrBudget(context.Background(), "s", 1)
	elapsed := time.Since(start)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("slow op err = %v, want ErrUnavailable", err)
	}
	if elapsed > 400*time.Millisecond {
		t.Errorf("op took %v; OpTimeout(150ms) not enforced", elapsed)
	}
	if pub.ctxSaw == nil {
		t.Error("op ctx was not cancelled at the OpTimeout deadline")
	}

	// Drive enough consecutive timeout failures to trip the breaker.
	for i := 1; i < 3; i++ {
		if _, e := c.IncrBudget(context.Background(), "s", 1); !errors.Is(e, ErrUnavailable) {
			t.Fatalf("timeout op %d err = %v, want ErrUnavailable", i+1, e)
		}
	}
	if c.BreakerState() != gobreaker.StateOpen {
		t.Errorf("breaker state = %v after %d timeout failures, want Open", c.BreakerState(), 3)
	}
}

func TestBreakerOpensAfterThresholdAndMapsUnavailable(t *testing.T) {
	pub := &fakePublisher{err: nats.ErrNoResponders}
	c := newTestClient(Config{CBFailureThreshold: 3}, pub, nil, nil)

	var mu sync.Mutex
	var transitions []gobreaker.State
	c.onStateChange = func(_, to gobreaker.State) {
		mu.Lock()
		transitions = append(transitions, to)
		mu.Unlock()
	}

	for i := 0; i < 3; i++ {
		if _, err := c.IncrBudget(context.Background(), "s", 1); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("call %d err = %v, want ErrUnavailable", i, err)
		}
	}
	if c.BreakerState() != gobreaker.StateOpen {
		t.Fatalf("breaker state = %v, want Open after %d failures", c.BreakerState(), 3)
	}
	// While open, the op is short-circuited — the publisher is NOT called again.
	before := pub.callCount()
	if _, err := c.IncrBudget(context.Background(), "s", 1); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("open-state err = %v, want ErrUnavailable", err)
	}
	if pub.callCount() != before {
		t.Error("open breaker must short-circuit without invoking the publisher")
	}
	mu.Lock()
	sawOpen := false
	for _, s := range transitions {
		if s == gobreaker.StateOpen {
			sawOpen = true
		}
	}
	mu.Unlock()
	if !sawOpen {
		t.Error("OnBreakerStateChange never reported the Open transition")
	}
}

func TestBreakerRecoversToClosed(t *testing.T) {
	pub := &fakePublisher{err: nats.ErrTimeout}
	c := newTestClient(Config{CBFailureThreshold: 2, CBOpenDuration: 60 * time.Millisecond}, pub, nil, nil)

	for i := 0; i < 2; i++ {
		_, _ = c.IncrBudget(context.Background(), "s", 1)
	}
	if c.BreakerState() != gobreaker.StateOpen {
		t.Fatalf("state = %v, want Open", c.BreakerState())
	}
	// Replace the bare sleep with deterministic polling: wait until the breaker
	// transitions out of Open (to HalfOpen) so the test is not flaky on loaded
	// runners where 80ms may not be enough. Generous timeout: 5 seconds.
	waitDeadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(waitDeadline) {
		if c.BreakerState() != gobreaker.StateOpen {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if c.BreakerState() == gobreaker.StateOpen {
		t.Fatal("breaker did not leave Open state within 5s (CBOpenDuration=60ms)")
	}
	// Now fix the publisher so the half-open probe succeeds and closes the breaker.
	pub.mu.Lock()
	pub.err = nil
	pub.ackVal = "10"
	pub.mu.Unlock()
	if _, err := c.IncrBudget(context.Background(), "s", 5); err != nil {
		t.Fatalf("half-open probe err: %v", err)
	}
	if c.BreakerState() != gobreaker.StateClosed {
		t.Errorf("state = %v, want Closed after successful probe", c.BreakerState())
	}
}

func TestMapErrPassthrough(t *testing.T) {
	sentinel := errors.New("boom")
	if got := mapErr(sentinel); errors.Is(got, ErrUnavailable) {
		t.Error("unrelated error must not be mapped to ErrUnavailable")
	}
	if mapErr(nil) != nil {
		t.Error("mapErr(nil) must be nil")
	}
	for _, e := range []error{
		gobreaker.ErrOpenState, gobreaker.ErrTooManyRequests,
		context.DeadlineExceeded, nats.ErrTimeout, nats.ErrNoResponders, nats.ErrConnectionClosed,
	} {
		if got := mapErr(e); !errors.Is(got, ErrUnavailable) {
			t.Errorf("mapErr(%v) not mapped to ErrUnavailable", e)
		}
	}
}

func TestReadBudgetMapsInfraError(t *testing.T) {
	rd := &fakeReader{err: nats.ErrTimeout}
	c := newTestClient(Config{}, nil, rd, nil)
	if _, err := c.ReadBudget(context.Background(), "s"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("ReadBudget infra err = %v, want ErrUnavailable", err)
	}
}

func TestReadBudgetGarbagePayloadIsConfigError(t *testing.T) {
	rd := &fakeReader{raw: &jetstream.RawStreamMsg{Data: []byte("xyz")}}
	c := newTestClient(Config{}, nil, rd, nil)
	_, err := c.ReadBudget(context.Background(), "s")
	if err == nil {
		t.Fatal("expected parse error")
	}
	if errors.Is(err, ErrUnavailable) {
		t.Error("parse error must not be ErrUnavailable")
	}
}

// TestSlowReadTripsOpTimeout asserts that when the counterReader blocks beyond
// OpTimeout the ReadBudget call returns ErrUnavailable and does not hang. This
// exercises the ctx-propagation path in fakeReader.GetLastMsgForSubject which
// was previously untested because the old fake ignored its context entirely.
func TestSlowReadTripsOpTimeout(t *testing.T) {
	rd := &fakeReader{block: 500 * time.Millisecond} // much longer than OpTimeout(150ms)
	c := newTestClient(Config{CBFailureThreshold: 3}, nil, rd, nil)

	start := time.Now()
	_, err := c.ReadBudget(context.Background(), "s")
	elapsed := time.Since(start)

	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("slow read err = %v, want ErrUnavailable", err)
	}
	if elapsed > 400*time.Millisecond {
		t.Errorf("ReadBudget took %v; OpTimeout(150ms) not enforced", elapsed)
	}
}

func TestTryAlertCooldownMapsInfraError(t *testing.T) {
	kv := &fakeKV{err: nats.ErrNoResponders}
	c := newTestClient(Config{}, nil, nil, kv)
	if _, err := c.TryAlertCooldown(context.Background(), "k"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("cooldown infra err = %v, want ErrUnavailable", err)
	}
}

func TestPublishDeltaMapsInfraError(t *testing.T) {
	pub := &fakePublisher{err: nats.ErrConnectionClosed}
	c := newTestClient(Config{}, pub, nil, nil)
	if err := c.PublishDelta(context.Background(), "e", []byte("{}")); !errors.Is(err, ErrUnavailable) {
		t.Errorf("PublishDelta infra err = %v, want ErrUnavailable", err)
	}
}

func TestConnectUnreachableURLFailsFast(t *testing.T) {
	// A syntactically valid but unroutable URL must fail (fast, bounded by the
	// 1s connect timeout) rather than hang. Uses the reserved TEST-NET-1 block.
	start := time.Now()
	_, err := Connect(context.Background(), Config{URL: "nats://192.0.2.1:4222"})
	if err == nil {
		t.Fatal("Connect to unreachable server should error")
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("Connect took %v; connect timeout not enforced", time.Since(start))
	}
}

func TestBindAssetsBindsWhatTheBootstrapCreated(t *testing.T) {
	c := &Client{cfg: Config{}.withDefaults()}
	if err := c.bindAssets(context.Background(), goodBinder()); err != nil {
		t.Fatalf("bindAssets: %v", err)
	}
	if c.budget == nil || c.ratelimit == nil || c.cooldown == nil {
		t.Errorf("not every handle bound: budget=%v ratelimit=%v cooldown=%v", c.budget != nil, c.ratelimit != nil, c.cooldown != nil)
	}
}

// The gateway creates nothing (#1076). A missing asset is a boot error that
// names the asset and the Job that creates it.
func TestBindAssetsMissingAssetNamesTheBootstrap(t *testing.T) {
	for _, name := range []string{BudgetStream, RateLimitStream, DeltasStream} {
		b := goodBinder()
		delete(b.streams, name)
		c := &Client{cfg: Config{}.withDefaults()}
		err := c.bindAssets(context.Background(), b)
		if err == nil || !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), "nats-bootstrap") {
			t.Errorf("missing %s: err = %v, want one naming the stream and nats-bootstrap", name, err)
		}
	}
	b := goodBinder()
	b.noKV = true
	c := &Client{cfg: Config{}.withDefaults()}
	if err := c.bindAssets(context.Background(), b); err == nil || !strings.Contains(err.Error(), AlertCooldownBucket) {
		t.Errorf("missing cooldown bucket: err = %v", err)
	}
}

// Each property the gateway's correctness rests on is verified on bind.
func TestBindAssetsRefusesAMisconfiguredAsset(t *testing.T) {
	cases := map[string]func(b *fakeBinder){
		"budget not a counter": func(b *fakeBinder) {
			cfg := b.streams[BudgetStream]
			cfg.AllowMsgCounter = false
			b.streams[BudgetStream] = cfg
		},
		"budget dedup window shorter than the recovery replay": func(b *fakeBinder) {
			cfg := b.streams[BudgetStream]
			cfg.Duplicates = 2 * time.Minute
			b.streams[BudgetStream] = cfg
		},
		"budget wrong subjects": func(b *fakeBinder) {
			cfg := b.streams[BudgetStream]
			cfg.Subjects = []string{"gateway.budget.>"}
			b.streams[BudgetStream] = cfg
		},
		"ratelimit not a counter": func(b *fakeBinder) {
			cfg := b.streams[RateLimitStream]
			cfg.AllowMsgCounter = false
			b.streams[RateLimitStream] = cfg
		},
		"ratelimit without MaxAge": func(b *fakeBinder) {
			cfg := b.streams[RateLimitStream]
			cfg.MaxAge = 0
			b.streams[RateLimitStream] = cfg
		},
		"deltas without a dedup window": func(b *fakeBinder) {
			cfg := b.streams[DeltasStream]
			cfg.Duplicates = 0
			b.streams[DeltasStream] = cfg
		},
		"deltas wrong subject": func(b *fakeBinder) {
			cfg := b.streams[DeltasStream]
			cfg.Subjects = []string{"gateway.budget.deltas"}
			b.streams[DeltasStream] = cfg
		},
		"budget without allow_direct": func(b *fakeBinder) {
			cfg := b.streams[BudgetStream]
			cfg.AllowDirect = false
			b.streams[BudgetStream] = cfg
		},
		"ratelimit without allow_direct": func(b *fakeBinder) {
			cfg := b.streams[RateLimitStream]
			cfg.AllowDirect = false
			b.streams[RateLimitStream] = cfg
		},
		"ratelimit MaxAge inside one window": func(b *fakeBinder) {
			cfg := b.streams[RateLimitStream]
			cfg.MaxAge = 30 * time.Second
			b.streams[RateLimitStream] = cfg
		},
		"cooldown without TTL": func(b *fakeBinder) { b.kvTTL = 0 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			b := goodBinder()
			mutate(b)
			c := &Client{cfg: Config{}.withDefaults()}
			if err := c.bindAssets(context.Background(), b); err == nil {
				t.Error("bindAssets accepted it")
			}
		})
	}
}

func TestBindAssetsPropagatesLookupErrors(t *testing.T) {
	c := &Client{cfg: Config{}.withDefaults()}
	if err := c.bindAssets(context.Background(), &fakeBinder{err: errors.New("boom")}); err == nil {
		t.Error("lookup error not propagated")
	}
}

// Connect refuses a URL and TLS material that disagree before it dials.
func TestConnectRefusesMismatchedURLAndMaterial(t *testing.T) {
	cases := map[string]Config{
		"tls url without material": {URL: "tls://127.0.0.1:1"},
		"nats url with material":   {URL: "nats://127.0.0.1:1", TLSCAFile: "/ca", TLSCertFile: "/crt", TLSKeyFile: "/key"},
		"credential with material": {URL: "tls://u:p@127.0.0.1:1", TLSCAFile: "/ca", TLSCertFile: "/crt", TLSKeyFile: "/key"},
		"half material":            {URL: "tls://127.0.0.1:1", TLSCertFile: "/crt", TLSKeyFile: "/key"},
		"unreadable material":      {URL: "tls://127.0.0.1:1", TLSCAFile: "/nonexistent/ca", TLSCertFile: "/nonexistent/crt", TLSKeyFile: "/nonexistent/key"},
	}
	for name, cfg := range cases {
		if _, err := Connect(context.Background(), cfg); err == nil {
			t.Errorf("%s: Connect succeeded", name)
		} else if strings.Contains(err.Error(), "u:p@") {
			t.Errorf("%s: error echoes the credential: %v", name, err)
		}
	}
}

// TestReadBudgetNegativeTotal asserts FIX 4: a correction-overshoot that
// produces a negative running total is returned as the correct negative int64,
// not as a huge positive value caused by a uint64 round-trip.
func TestReadBudgetNegativeTotal(t *testing.T) {
	// -500 nano-USD: a correction applied that overshot the counter to negative.
	rd := &fakeReader{raw: &jetstream.RawStreamMsg{Data: []byte("-500")}}
	c := newTestClient(Config{}, nil, rd, nil)
	got, err := c.ReadBudget(context.Background(), "s")
	if err != nil {
		t.Fatalf("ReadBudget negative total: %v", err)
	}
	if got != -500 {
		t.Errorf("ReadBudget negative total = %d, want -500 (got large positive = uint64 wrap bug)", got)
	}
}

// TestIncrBudgetNegativeTotal asserts FIX 4 for IncrBudget: a correction that
// drives the running total negative must return the correct int64, not the
// result of a corrupting uint64 round-trip.
func TestIncrBudgetNegativeTotalFromAck(t *testing.T) {
	pub := &fakePublisher{ackVal: "-250"}
	c := newTestClient(Config{}, pub, nil, nil)
	got, err := c.IncrBudget(context.Background(), "s", -750)
	if err != nil {
		t.Fatalf("IncrBudget negative ack total: %v", err)
	}
	if got != -250 {
		t.Errorf("IncrBudget negative ack total = %d, want -250 (got large positive = uint64 wrap bug)", got)
	}
}

func TestOnBreakerStateChangeAndAccessors(t *testing.T) {
	pub := &fakePublisher{err: nats.ErrNoResponders}
	c := newTestClient(Config{CBFailureThreshold: 1}, pub, nil, nil)
	var fired bool
	c.OnBreakerStateChange(func(_, _ gobreaker.State) { fired = true })
	_, _ = c.IncrBudget(context.Background(), "s", 1)
	if !fired {
		t.Error("OnBreakerStateChange callback not invoked on transition")
	}
	if c.BreakerState() != gobreaker.StateOpen {
		t.Errorf("state = %v, want Open", c.BreakerState())
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	fc := &fakeConn{}
	c := &Client{nc: fc}
	c.Close()
	c.Close() // second close must be a no-op (IsClosed guard)
	if fc.closes != 1 {
		t.Errorf("Close invoked underlying %d times, want 1", fc.closes)
	}
	// Close on a client with no conn must not panic.
	(&Client{}).Close()
}

func TestJetStreamAccessor(t *testing.T) {
	c := &Client{}
	if c.JetStream() != nil {
		t.Error("nil js should return nil")
	}
}

func TestIncrBudgetIdempotentAppliesAndReturnsTotal(t *testing.T) {
	pub := &fakePublisher{ackVal: "2000000000"} // 2 USD nano
	c := newTestClient(Config{}, pub, &fakeReader{}, &fakeKV{})
	total, applied, err := c.IncrBudgetIdempotent(context.Background(),
		"gateway.budget.counter.project.1.100", "recovery.project.1.100.500000000", 500000000)
	if err != nil {
		t.Fatalf("IncrBudgetIdempotent: %v", err)
	}
	if !applied {
		t.Error("first apply should report applied=true")
	}
	if total != 2000000000 {
		t.Errorf("total = %d, want 2000000000", total)
	}
	// The reused event_id MUST be set as the Nats-Msg-Id for stream dedup.
	if got := pub.lastMsg.Header.Get(jetstream.MsgIDHeader); got != "recovery.project.1.100.500000000" {
		t.Errorf("Nats-Msg-Id = %q, want the reused event_id", got)
	}
	// The delta MUST be set as the Nats-Incr header.
	if got := pub.lastMsg.Header.Get(IncrHeader); got != "500000000" {
		t.Errorf("Nats-Incr = %q, want 500000000", got)
	}
}

func TestIncrBudgetIdempotentSuppressesDuplicate(t *testing.T) {
	// A dedup-window hit returns applied=false and the caller must not re-count.
	pub := &fakePublisher{ackVal: "0", dup: true}
	c := newTestClient(Config{}, pub, &fakeReader{}, &fakeKV{})
	_, applied, err := c.IncrBudgetIdempotent(context.Background(), "s", "evt-1", 100)
	if err != nil {
		t.Fatalf("IncrBudgetIdempotent: %v", err)
	}
	if applied {
		t.Error("duplicate must report applied=false")
	}
}

func TestIncrBudgetIdempotentMapsInfraError(t *testing.T) {
	pub := &fakePublisher{err: nats.ErrNoResponders}
	c := newTestClient(Config{}, pub, &fakeReader{}, &fakeKV{})
	_, _, err := c.IncrBudgetIdempotent(context.Background(), "s", "evt-1", 100)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}

func TestIncrBudgetIdempotentBadAckIsConfigError(t *testing.T) {
	pub := &fakePublisher{ackVal: "not-a-number"}
	c := newTestClient(Config{}, pub, &fakeReader{}, &fakeKV{})
	_, _, err := c.IncrBudgetIdempotent(context.Background(), "s", "evt-1", 100)
	if err == nil || errors.Is(err, ErrUnavailable) {
		t.Fatalf("bad ack should be a config error, got %v", err)
	}
}

func TestParseIntRoundTripGuard(t *testing.T) {
	// Guard the nano-USD headroom claim: int64 holds ≈9.2e9 USD in nano.
	const maxNano = int64(9223372036854775807)
	s := strconv.FormatInt(maxNano, 10)
	back, err := strconv.ParseInt(s, 10, 64)
	if err != nil || back != maxNano {
		t.Fatalf("nano round-trip failed: %v", err)
	}
}

// TestBreakerNotOpenOnClientCancellation asserts Fix #5 (nats-atomicity): a
// burst of context.Canceled errors (client disconnect / browser stop) must NOT
// trip the circuit breaker. Canceled signals caller-side abort, not NATS
// infrastructure failure. Without the IsExcluded hook, 3 cancellations within
// the 5s window open the breaker, triggering false-positive degraded mode.
func TestBreakerNotOpenOnClientCancellation(t *testing.T) {
	// Publisher blocks until the context is cancelled.
	pub := &fakePublisher{block: 5 * time.Second}
	c := newTestClient(Config{CBFailureThreshold: 3}, pub, nil, nil)

	// Fire 3 calls with pre-cancelled contexts, simulating client disconnects.
	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // cancel immediately so the publisher sees ctx.Done()
		_, err := c.IncrBudget(ctx, "s", 1)
		if err == nil {
			t.Fatalf("call %d: expected error on cancelled context", i)
		}
		// The error should be ErrUnavailable (context.Canceled is mapped by mapErr)
		// but it must NOT count towards the breaker threshold.
	}
	if c.BreakerState() != gobreaker.StateClosed {
		t.Errorf("breaker state = %v after 3 client cancellations; want Closed (client abort ≠ NATS failure)", c.BreakerState())
	}
}

// TestBreakerStillOpensOnInfraError asserts that Fix #5 does not accidentally
// suppress real NATS infrastructure errors from tripping the breaker.
func TestBreakerStillOpensOnInfraError(t *testing.T) {
	pub := &fakePublisher{err: nats.ErrNoResponders}
	c := newTestClient(Config{CBFailureThreshold: 3}, pub, nil, nil)

	for i := 0; i < 3; i++ {
		if _, err := c.IncrBudget(context.Background(), "s", 1); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("call %d: err=%v, want ErrUnavailable", i, err)
		}
	}
	if c.BreakerState() != gobreaker.StateOpen {
		t.Errorf("breaker state = %v after 3 infra errors; want Open", c.BreakerState())
	}
}

// ── PublishSoftAlertEvent (gateway.events.*) ─────────────────────────────────

// TestPublishSoftAlertEvent_SubjectAndPayload asserts the event is core-
// published to the natsbus-compatible per-project subject and flushed.
func TestPublishSoftAlertEvent_SubjectAndPayload(t *testing.T) {
	fc := &fakeConn{}
	c := &Client{nc: fc}

	if err := c.PublishSoftAlertEvent(context.Background(), "42", []byte(`{"type":"budget.soft_alert"}`)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fc.published) != 1 {
		t.Fatalf("published %d messages, want 1", len(fc.published))
	}
	if got, want := fc.published[0].subject, "gateway.events.project.42.events"; got != want {
		t.Errorf("subject = %q, want %q (must match elitea-main natsbus subjectFor)", got, want)
	}
	if string(fc.published[0].data) != `{"type":"budget.soft_alert"}` {
		t.Errorf("payload altered in transit: %q", fc.published[0].data)
	}
	if fc.flushes != 1 {
		t.Errorf("flushes = %d, want 1 (publish must be flushed so errors surface)", fc.flushes)
	}
}

// TestPublishSoftAlertEvent_PublishError asserts a transport error is mapped
// and returned (the caller logs it; the alert must not be silently dropped
// without a trace).
func TestPublishSoftAlertEvent_PublishError(t *testing.T) {
	fc := &fakeConn{publishErr: errFakeTransport}
	c := &Client{nc: fc}
	if err := c.PublishSoftAlertEvent(context.Background(), "42", []byte(`{}`)); err == nil {
		t.Fatal("expected error from failed publish, got nil")
	}
}

// TestPublishSoftAlertEvent_FlushError asserts a flush failure surfaces too.
func TestPublishSoftAlertEvent_FlushError(t *testing.T) {
	fc := &fakeConn{flushErr: errFakeTransport}
	c := &Client{nc: fc}
	if err := c.PublishSoftAlertEvent(context.Background(), "42", []byte(`{}`)); err == nil {
		t.Fatal("expected error from failed flush, got nil")
	}
}

// TestPublishSoftAlertEvent_ExpiredContext asserts an already-expired context
// short-circuits BEFORE the publish, not just before the flush. Core NATS
// Publish is async and buffered, so publishing first would enqueue the event for
// the next flush while the method still returned DeadlineExceeded — the caller
// would be told "not published" about an event that was in fact delivered.
func TestPublishSoftAlertEvent_ExpiredContext(t *testing.T) {
	fc := &fakeConn{}
	c := &Client{nc: fc}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	err := c.PublishSoftAlertEvent(ctx, "42", []byte(`{}`))
	if err == nil {
		t.Fatal("expected error for expired context, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded", err)
	}
	if len(fc.published) != 0 {
		t.Errorf("published %d messages, want 0 (a 'did not publish' error must be truthful)", len(fc.published))
	}
	if fc.flushes != 0 {
		t.Errorf("flushes = %d, want 0 (expired ctx must not flush)", fc.flushes)
	}
}

// errFakeTransport is the sentinel transport error for the event-publish tests.
var errFakeTransport = errors.New("fake transport error")

// TestCounterValue_NATS212JSONBody pins the REAL NATS 2.12 counter payload
// shape {"val":"N"} (the original bare-integer-only parse silently degraded
// budget enforcement on live servers — found by the BFF.9x gate run).
func TestCounterValue_NATS212JSONBody(t *testing.T) {
	cases := []struct {
		data string
		want int64
		ok   bool
	}{
		{`{"val":"10000000"}`, 10000000, true},
		{`{"val":"-250"}`, -250, true},
		{`12345`, 12345, true}, // bare form still accepted
		{`{"val":"not-a-number"}`, 0, false},
		{`{"other":"1"}`, 0, false},
		{`garbage`, 0, false},
	}
	for _, c := range cases {
		got, err := counterValue(&jetstream.RawStreamMsg{Data: []byte(c.data)})
		if c.ok && (err != nil || got != c.want) {
			t.Errorf("counterValue(%q) = %d, %v; want %d, nil", c.data, got, err, c.want)
		}
		if !c.ok && err == nil {
			t.Errorf("counterValue(%q) = %d, nil; want error", c.data, got)
		}
	}
}

// ── PublishOpsEvent (gateway.events.ops.*) ───────────────────────────────────

// TestPublishOpsEvent_SubjectIsNotTenantVisible is the privacy assertion for
// the operator-only port. budget.unbilled_stream records which streams the
// gateway failed to bill; on the per-project subject that elitea-main relays to
// project members it would be a live oracle for the conditions that produce it.
// The subject MUST sit outside the gateway.events.project.* tree the EventBus
// subscribes to.
func TestPublishOpsEvent_SubjectIsNotTenantVisible(t *testing.T) {
	fc := &fakeConn{}
	c := &Client{nc: fc}

	if err := c.PublishOpsEvent(context.Background(), []byte(`{"type":"budget.unbilled_stream"}`)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fc.published) != 1 {
		t.Fatalf("published %d messages, want 1", len(fc.published))
	}
	got := fc.published[0].subject
	if got != OpsEventSubject {
		t.Errorf("subject = %q, want %q", got, OpsEventSubject)
	}
	if strings.HasPrefix(got, EventSubjectRoot+".project.") {
		t.Errorf("subject %q is inside the per-project tree elitea-main relays to tenants — the "+
			"unbilled-stream record must be operator-only", got)
	}
	// A project-scoped wildcard subscription must not match it.
	if got == eventSubjectForProject("42") {
		t.Errorf("subject collides with a tenant project subject: %q", got)
	}
	if string(fc.published[0].data) != `{"type":"budget.unbilled_stream"}` {
		t.Errorf("payload altered in transit: %q", fc.published[0].data)
	}
	if fc.flushes != 1 {
		t.Errorf("flushes = %d, want 1 (publish must be flushed so errors surface)", fc.flushes)
	}
}

// TestPublishOpsEvent_ErrorsSurface mirrors the soft-alert coverage: a transport
// or flush failure must be returned, not swallowed.
func TestPublishOpsEvent_ErrorsSurface(t *testing.T) {
	if err := (&Client{nc: &fakeConn{publishErr: errFakeTransport}}).
		PublishOpsEvent(context.Background(), []byte(`{}`)); err == nil {
		t.Error("expected error from failed publish, got nil")
	}
	if err := (&Client{nc: &fakeConn{flushErr: errFakeTransport}}).
		PublishOpsEvent(context.Background(), []byte(`{}`)); err == nil {
		t.Error("expected error from failed flush, got nil")
	}
}

// TestPublishOpsEvent_ExpiredContextDoesNotEnqueue: core NATS Publish is async
// and buffered, so an expired context must short-circuit BEFORE the publish or
// the caller is told "not published" about an event that was in fact delivered.
func TestPublishOpsEvent_ExpiredContextDoesNotEnqueue(t *testing.T) {
	fc := &fakeConn{}
	c := &Client{nc: fc}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	err := c.PublishOpsEvent(ctx, []byte(`{}`))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded", err)
	}
	if len(fc.published) != 0 {
		t.Errorf("published %d messages on an expired context, want 0", len(fc.published))
	}
}

// TestSingleTimeoutMapsUnavailableWithoutABreakerEdge is the premise of issue
// #515, stated where the two pieces of it live.
//
// One slow counter operation is enough to send the governance store down the
// outage branch, because mapErr turns context.DeadlineExceeded into
// ErrUnavailable. It is NOT enough to move the breaker: the default threshold is
// three consecutive failures. So the caller marks the accumulator row
// outage-owned and no state transition follows to hand it back. The recovery
// reconciliation used to be wired to that transition alone, which is why the row
// stayed wedged for the rest of the billing period.
func TestSingleTimeoutMapsUnavailableWithoutABreakerEdge(t *testing.T) {
	pub := &fakePublisher{ackVal: "0", block: 500 * time.Millisecond}
	c := newTestClient(Config{}, pub, nil, nil) // withDefaults ⇒ threshold 3

	var mu sync.Mutex
	var transitions int
	c.onStateChange = func(_, _ gobreaker.State) {
		mu.Lock()
		transitions++
		mu.Unlock()
	}

	// ONE slow operation.
	_, _, err := c.IncrBudgetIdempotent(context.Background(), "s", "evt-1", 1)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("single timeout err = %v, want ErrUnavailable — the caller must take the outage branch", err)
	}

	if c.BreakerState() != gobreaker.StateClosed {
		t.Fatalf("breaker state = %v after one failure, want Closed", c.BreakerState())
	}
	mu.Lock()
	seen := transitions
	mu.Unlock()
	if seen != 0 {
		t.Fatalf("%d breaker transitions after one failure; the recovery edge would have fired", seen)
	}

	// The connection is healthy for everything else, which is why the delta
	// publish that follows the failed increment succeeds and the write-back
	// consumer receives a delta for a row it is barred from touching.
	pub.block = 0
	if err := c.PublishDelta(context.Background(), "evt-1", []byte("{}")); err != nil {
		t.Fatalf("publish on the same healthy connection failed: %v", err)
	}
	mu.Lock()
	seen = transitions
	mu.Unlock()
	if seen != 0 {
		t.Fatalf("%d breaker transitions after the recovery of a single failure", seen)
	}
}
