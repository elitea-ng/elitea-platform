package auditexport

import (
	"context"
	"crypto/hmac"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/egress"
)

var (
	oldKey = SigningKey{ID: "k1", Secret: []byte("old-secret-old-secret-old-secret-00"), Created: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	newKey = SigningKey{ID: "k2", Secret: []byte("new-secret-new-secret-new-secret-11"), Created: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)}
)

// receiver is a webhook receiver that verifies signatures with both keys, the
// way a customer's endpoint does during a rotation.
type receiver struct {
	t    *testing.T
	keys map[string][]byte
	now  func() time.Time

	mu       sync.Mutex
	status   []int // scripted answers, then 200
	requests int
	got      map[string]int // event_id -> deliveries
	keyIDs   []string
	// refuse answers this code to any batch holding the event id.
	refuse map[string]int
}

func newReceiver(t *testing.T, keys ...SigningKey) *receiver {
	r := &receiver{t: t, keys: map[string][]byte{}, got: map[string]int{}, refuse: map[string]int{}, now: time.Now}
	for _, k := range keys {
		r.keys[k.ID] = k.Secret
	}
	return r
}

func (r *receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests++
	if len(r.status) > 0 {
		code := r.status[0]
		r.status = r.status[1:]
		if code != http.StatusOK {
			w.WriteHeader(code)
			return
		}
	}
	// Verify the signature before decoding the body.
	secret, ok := r.keys[req.Header.Get(HeaderKeyID)]
	if !ok {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	parts := strings.Split(req.Header.Get(HeaderSignature), ",")
	if len(parts) != 2 || !strings.HasPrefix(parts[0], "t=") || !strings.HasPrefix(parts[1], "v1=") {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	ts, err := strconv.ParseInt(strings.TrimPrefix(parts[0], "t="), 10, 64)
	if err != nil || r.now().Sub(time.Unix(ts, 0)).Abs() > 5*time.Minute {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	want := Sign(secret, time.Unix(ts, 0), body)
	gotSig, _ := hex.DecodeString(strings.TrimPrefix(parts[1], "v1="))
	wantSig, _ := hex.DecodeString(want)
	if !hmac.Equal(gotSig, wantSig) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var batch webhookBodyForTest
	if err := json.Unmarshal(body, &batch); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	for _, e := range batch.Events {
		if code, ok := r.refuse[e.EventID]; ok {
			w.WriteHeader(code)
			return
		}
	}
	r.keyIDs = append(r.keyIDs, req.Header.Get(HeaderKeyID))
	for _, e := range batch.Events {
		r.got[e.EventID]++
	}
	w.WriteHeader(http.StatusOK)
}

type webhookBodyForTest struct {
	Schema  string `json:"schema"`
	BatchID string `json:"batch_id"`
	Events  []struct {
		EventID string `json:"event_id"`
	} `json:"events"`
}

func (r *receiver) delivered(id string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.got[id]
}

func (r *receiver) script(codes ...int) {
	r.mu.Lock()
	r.status = codes
	r.mu.Unlock()
}

func testEvents(n int, from int) []Event {
	at := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	events := make([]Event, n)
	for i := range events {
		events[i] = FromAuditRow(AuditRow{ID: int64(from + i), Timestamp: at, UserID: ptr(int64(1)), EventType: "api", Action: "GET /x"})
	}
	return events
}

func allowLoopback(t *testing.T) *http.Client {
	t.Helper()
	allowlist, err := egress.ParseAllowlist([]string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Transport: egress.New(allowlist).RoundTripper(), Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func fixedKeys(keys ...SigningKey) KeySource {
	return func() ([]SigningKey, error) { return keys, nil }
}

func newTestWebhook(t *testing.T, url string, keys KeySource, tweak func(*WebhookConfig)) *WebhookSink {
	t.Helper()
	cfg := WebhookConfig{
		URL: url, Keys: keys, Client: allowLoopback(t), BatchSize: 100, MaxAttempts: 3,
		Sleep: func(context.Context, time.Duration) error { return nil },
	}
	if tweak != nil {
		tweak(&cfg)
	}
	sink, err := NewWebhookSink(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return sink
}

func TestWebhookSignsWithNewestKeyAndReceiverVerifiesBoth(t *testing.T) {
	rcv := newReceiver(t, oldKey, newKey)
	srv := httptest.NewServer(rcv)
	defer srv.Close()

	// The key file lists the retiring key first; the newest still signs.
	sink := newTestWebhook(t, srv.URL, fixedKeys(oldKey, newKey), nil)
	events := testEvents(3, 1)
	if err := sink.Send(context.Background(), events); err != nil {
		t.Fatalf("send: %v", err)
	}
	for _, e := range events {
		if rcv.delivered(e.EventID) != 1 {
			t.Fatalf("event %s delivered %d times", e.EventID, rcv.delivered(e.EventID))
		}
	}
	if rcv.keyIDs[0] != "k2" {
		t.Fatalf("signed with key %q, want the newest (k2)", rcv.keyIDs[0])
	}

	// A receiver that has only the retired key rejects the new signature, and
	// one that has only the new key accepts it: rotation order is receiver
	// first, then Elitea.
	onlyOld := httptest.NewServer(newReceiver(t, oldKey))
	defer onlyOld.Close()
	if err := newTestWebhook(t, onlyOld.URL, fixedKeys(newKey), nil).Send(context.Background(), events); err == nil {
		t.Fatal("a receiver without the new key accepted it")
	}
	// Rolling back is one edit: with only the old key, the old key signs.
	if err := newTestWebhook(t, onlyOld.URL, fixedKeys(oldKey), nil).Send(context.Background(), events); err != nil {
		t.Fatalf("rollback to the old key: %v", err)
	}
}

func TestWebhookSignatureCoversTimestampAndBody(t *testing.T) {
	body := []byte(`{"a":1}`)
	at := time.Unix(1_800_000_000, 0)
	sig := Sign(newKey.Secret, at, body)
	if SignatureHeader(newKey.Secret, at, body) != fmt.Sprintf("t=1800000000,v1=%s", sig) {
		t.Fatal("header format")
	}
	if sig == Sign(newKey.Secret, at.Add(time.Second), body) {
		t.Fatal("timestamp is not covered")
	}
	if sig == Sign(newKey.Secret, at, []byte(`{"a":2}`)) {
		t.Fatal("body is not covered")
	}
	if sig == Sign(oldKey.Secret, at, body) {
		t.Fatal("key is not covered")
	}
}

func TestWebhookRetriesTransientFailuresWithinASend(t *testing.T) {
	rcv := newReceiver(t, newKey)
	rcv.script(http.StatusServiceUnavailable, http.StatusTooManyRequests)
	srv := httptest.NewServer(rcv)
	defer srv.Close()
	var slept []time.Duration
	sink := newTestWebhook(t, srv.URL, fixedKeys(newKey), func(c *WebhookConfig) {
		c.Sleep = func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }
	})
	events := testEvents(2, 1)
	if err := sink.Send(context.Background(), events); err != nil {
		t.Fatalf("send after two transient failures: %v", err)
	}
	if rcv.requests != 3 || len(slept) != 2 {
		t.Fatalf("requests = %d, sleeps = %d; want 3 and 2", rcv.requests, len(slept))
	}
	if slept[0] > 250*time.Millisecond || slept[1] > 500*time.Millisecond || slept[0] <= 0 {
		t.Fatalf("backoff out of the jittered exponential bounds: %v", slept)
	}
}

func TestWebhookGivesUpAfterMaxAttemptsAndKeepsEventPending(t *testing.T) {
	rcv := newReceiver(t, newKey)
	rcv.script(500, 500, 500, 500)
	srv := httptest.NewServer(rcv)
	defer srv.Close()
	sink := newTestWebhook(t, srv.URL, fixedKeys(newKey), nil)
	err := sink.Send(context.Background(), testEvents(1, 1))
	if err == nil {
		t.Fatal("exhausted retries must return an error so the relay does not acknowledge")
	}
	var permanent *PermanentError
	if errors.As(err, &permanent) {
		t.Fatal("a 500 is transient")
	}
	if rcv.requests != 3 {
		t.Fatalf("requests = %d, want MaxAttempts", rcv.requests)
	}
}

func TestWebhookTreatsAuthAndRedirectFailuresAsTransient(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusFound} {
		rcv := newReceiver(t, newKey)
		rcv.script(code, code, code)
		srv := httptest.NewServer(rcv)
		err := newTestWebhook(t, srv.URL, fixedKeys(newKey), nil).Send(context.Background(), testEvents(1, 1))
		srv.Close()
		var permanent *PermanentError
		if err == nil || errors.As(err, &permanent) {
			t.Fatalf("status %d: err = %v; a wrong key or URL must wait and alert, not dead-letter evidence", code, err)
		}
	}
}

func TestWebhookDefaultGuardRefusesLoopbackAndPrivateReceivers(t *testing.T) {
	rcv := newReceiver(t, newKey)
	srv := httptest.NewServer(rcv)
	defer srv.Close()
	client := &http.Client{Transport: egress.New(nil).RoundTripper(), Timeout: 2 * time.Second}
	sink := newTestWebhook(t, srv.URL, fixedKeys(newKey), func(c *WebhookConfig) { c.Client = client })
	if err := sink.Send(context.Background(), testEvents(1, 1)); err == nil {
		t.Fatal("a loopback receiver was reached without an allowlist entry")
	}
	if rcv.requests != 0 {
		t.Fatal("the request left the process")
	}
}

func TestWebhookRefusesRedirects(t *testing.T) {
	target := newReceiver(t, newKey)
	dest := httptest.NewServer(target)
	defer dest.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, dest.URL, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()
	if err := newTestWebhook(t, redirector.URL, fixedKeys(newKey), nil).Send(context.Background(), testEvents(1, 1)); err == nil {
		t.Fatal("a redirect was treated as delivery")
	}
	if target.requests != 0 {
		t.Fatal("the signed body followed a redirect")
	}
}

func TestWebhookSplitsLargeBatches(t *testing.T) {
	rcv := newReceiver(t, newKey)
	srv := httptest.NewServer(rcv)
	defer srv.Close()
	sink := newTestWebhook(t, srv.URL, fixedKeys(newKey), func(c *WebhookConfig) { c.BatchSize = 2 })
	if err := sink.Send(context.Background(), testEvents(5, 1)); err != nil {
		t.Fatal(err)
	}
	if rcv.requests != 3 {
		t.Fatalf("requests = %d, want 3 POSTs for 5 events at 2 per batch", rcv.requests)
	}
}

type memoryDeadLetters struct {
	mu      sync.Mutex
	records []string
	fail    bool
}

func (m *memoryDeadLetters) RecordDeadLetters(_ context.Context, sink string, events []Event, _ string, attempts int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail {
		return errors.New("store down")
	}
	for _, e := range events {
		m.records = append(m.records, fmt.Sprintf("%s/%s/%d", sink, e.Ref(), attempts))
	}
	return nil
}

func TestWebhookSplitsARefusedBatchAndDeliversTheNeighbours(t *testing.T) {
	for _, code := range []int{http.StatusRequestEntityTooLarge, http.StatusBadRequest, http.StatusUnprocessableEntity} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			rcv := newReceiver(t, newKey)
			events := testEvents(10, 1)
			bad := events[6]
			rcv.refuse[bad.EventID] = code
			srv := httptest.NewServer(rcv)
			defer srv.Close()
			sink := newTestWebhook(t, srv.URL, fixedKeys(newKey), nil)

			err := sink.Send(context.Background(), events)
			var permanent *PermanentError
			if !errors.As(err, &permanent) || len(permanent.Events) != 1 || permanent.Events[0].EventID != bad.EventID {
				t.Fatalf("err = %v; want a permanent rejection naming only the bad event", err)
			}
			if got := settledCount(err, len(events)); got != 6 {
				t.Fatalf("settled = %d, want the 6 events before the bad one", got)
			}
			for i, e := range events {
				want := 0
				if i < 6 {
					want = 1
				}
				if rcv.delivered(e.EventID) != want {
					t.Fatalf("event %d delivered %d times, want %d", i, rcv.delivered(e.EventID), want)
				}
			}
		})
	}
}

// offerLikeTheRelay offers the events from a position that advances by the
// settled count, the way the audit_events relay does, for a number of passes.
func offerLikeTheRelay(sink Sink, events []Event, passes int) (next int) {
	for pass := 0; pass < passes && next < len(events); pass++ {
		err := sink.Send(context.Background(), events[next:])
		next += settledCount(err, len(events)-next)
	}
	return next
}

func TestPipelineDeadLettersOnlyTheRefusedEventAndDeliversItsNeighbours(t *testing.T) {
	rcv := newReceiver(t, newKey)
	events := testEvents(10, 10)
	bad := events[4]
	rcv.refuse[bad.EventID] = http.StatusRequestEntityTooLarge
	srv := httptest.NewServer(rcv)
	defer srv.Close()
	metrics := NewMetrics()
	store := &memoryDeadLetters{}
	sink := Wrap(newTestWebhook(t, srv.URL, fixedKeys(newKey), nil), PipelineConfig{
		Metrics: metrics, DeadLetters: store, MaxPermanentAttempts: 3, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	if next := offerLikeTheRelay(sink, events, 2); next != 4 {
		t.Fatalf("after 2 passes position = %d; the refused event must hold the position before the limit", next)
	}
	if len(store.records) != 0 {
		t.Fatalf("dead-lettered before the limit: %v", store.records)
	}
	if next := offerLikeTheRelay(sink, events[4:], 1); next != 6 {
		t.Fatalf("third pass settled %d of the remaining 6; want all (the refused one dead-lettered)", next)
	}
	if len(store.records) != 1 || store.records[0] != "webhook/admin_audit:14/3" {
		t.Fatalf("dead letters = %v; only the refused event", store.records)
	}
	for _, e := range events {
		want := 1
		if e.EventID == bad.EventID {
			want = 0
		}
		if rcv.delivered(e.EventID) != want {
			t.Fatalf("event %s delivered %d times, want %d", e.Ref(), rcv.delivered(e.EventID), want)
		}
	}
	if got := metrics.DeadLettered("webhook", SourceAdminAudit); got != 1 {
		t.Fatalf("dead_lettered metric = %d, want 1", got)
	}
	if got := metrics.Delivered("webhook", SourceAdminAudit); got != 9 {
		t.Fatalf("delivered metric = %d, want 9", got)
	}
	if got := metrics.Failed("webhook", "permanent"); got != 3 {
		t.Fatalf("failed{permanent} = %d, want 3", got)
	}
	var text strings.Builder
	_ = metrics.WriteText(&text)
	if !strings.Contains(text.String(), `elitea_audit_export_dead_lettered_total{sink="webhook",source="admin_audit"} 1`) {
		t.Fatalf("exposition lacks the dead letter series:\n%s", text.String())
	}
}

func TestPipelineCountsRefusalsPerEventNotPerBatch(t *testing.T) {
	rcv := newReceiver(t, newKey)
	events := testEvents(6, 1)
	rcv.refuse[events[2].EventID] = http.StatusBadRequest
	srv := httptest.NewServer(rcv)
	defer srv.Close()
	store := &memoryDeadLetters{}
	sink := Wrap(newTestWebhook(t, srv.URL, fixedKeys(newKey), nil), PipelineConfig{
		Metrics: NewMetrics(), DeadLetters: store, MaxPermanentAttempts: 3, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	// The batch behind the refused event grows between passes; the count
	// still belongs to the refused event.
	for _, upTo := range []int{4, 5, 6} {
		_ = sink.Send(context.Background(), events[2:upTo])
	}
	if len(store.records) != 1 || store.records[0] != "webhook/admin_audit:3/3" {
		t.Fatalf("dead letters = %v", store.records)
	}
}

func TestPipelineKeepsBatchPendingWhenDeadLetterCannotBeRecorded(t *testing.T) {
	rcv := newReceiver(t, newKey)
	rcv.script(422, 422)
	srv := httptest.NewServer(rcv)
	defer srv.Close()
	sink := Wrap(newTestWebhook(t, srv.URL, fixedKeys(newKey), nil), PipelineConfig{
		Metrics: NewMetrics(), DeadLetters: &memoryDeadLetters{fail: true}, MaxPermanentAttempts: 2,
	})
	events := testEvents(1, 1)
	_ = sink.Send(context.Background(), events)
	if err := sink.Send(context.Background(), events); err == nil {
		t.Fatal("acknowledged a batch whose dead letter was not recorded")
	}
}

// TestKillingAndRestoringTheReceiverLosesNothing is the issue's acceptance
// test: a receiver that is down while events keep arriving, then comes back,
// receives every event at least once, and nothing is dead-lettered.
func TestKillingAndRestoringTheReceiverLosesNothing(t *testing.T) {
	rcv := newReceiver(t, newKey)
	var mu sync.Mutex
	down := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		isDown := down
		mu.Unlock()
		if isDown {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		rcv.ServeHTTP(w, r)
	}))
	defer srv.Close()

	metrics := NewMetrics()
	store := &memoryDeadLetters{}
	sink := Wrap(newTestWebhook(t, srv.URL, fixedKeys(newKey), nil), PipelineConfig{
		Metrics: metrics, DeadLetters: store, MaxPermanentAttempts: 2,
	})

	// What the relay does: offer the oldest undelivered batch until it is
	// acknowledged, then move on.
	all := testEvents(30, 1)
	next := 0
	offer := func() bool {
		batch := all[next:min(next+10, len(all))]
		if err := sink.Send(context.Background(), batch); err != nil {
			return false
		}
		next += len(batch)
		return true
	}

	if !offer() {
		t.Fatal("healthy receiver refused")
	}
	mu.Lock()
	down = true
	mu.Unlock()
	for i := 0; i < 20; i++ { // a long outage: far more passes than any attempt limit
		if offer() {
			t.Fatal("a batch was acknowledged while the receiver was down")
		}
	}
	mu.Lock()
	down = false
	mu.Unlock()
	for next < len(all) {
		if !offer() {
			t.Fatal("receiver restored but delivery failed")
		}
	}
	for _, e := range all {
		if rcv.delivered(e.EventID) < 1 {
			t.Fatalf("event %s was lost", e.Ref())
		}
	}
	if len(store.records) != 0 {
		t.Fatalf("an outage dead-lettered %v", store.records)
	}
	if got := metrics.Failed("webhook", "transient"); got < 20 {
		t.Fatalf("failed{transient} = %d, want the outage counted", got)
	}
}

func TestFanoutDoesNotResendToHealthySink(t *testing.T) {
	healthy := &countingSink{name: "a", got: map[string]int{}}
	flaky := &countingSink{name: "b", failFirst: 3, got: map[string]int{}}
	metrics := NewMetrics()
	fan := Fanout{Wrap(healthy, PipelineConfig{Metrics: metrics}), Wrap(flaky, PipelineConfig{Metrics: metrics})}
	all := testEvents(8, 1)
	// The security relay re-offers its pending head, and the head grows while
	// one sink is down.
	for _, upTo := range []int{2, 4, 6, 8} {
		_ = fan.Send(context.Background(), all[:upTo])
	}
	for _, e := range all {
		if healthy.got[e.EventID] != 1 {
			t.Fatalf("healthy sink received %s %d times, want once", e.Ref(), healthy.got[e.EventID])
		}
		if flaky.got[e.EventID] != 1 {
			t.Fatalf("flaky sink received %s %d times after recovering, want once", e.Ref(), flaky.got[e.EventID])
		}
	}
	if err := fan.Send(context.Background(), all); err != nil {
		t.Fatalf("everything is settled: %v", err)
	}
	if healthy.calls != 4 || flaky.calls != 4 {
		t.Fatalf("calls healthy=%d flaky=%d; a fully settled batch is not sent again", healthy.calls, flaky.calls)
	}
}

type countingSink struct {
	name      string
	calls     int
	failFirst int
	got       map[string]int
}

func (c *countingSink) Name() string { return c.name }
func (c *countingSink) Send(_ context.Context, events []Event) error {
	c.calls++
	if c.calls <= c.failFirst {
		return errors.New("down")
	}
	for _, e := range events {
		if c.got != nil {
			c.got[e.EventID]++
		}
	}
	return nil
}
