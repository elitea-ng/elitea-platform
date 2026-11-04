package webpush

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// memoryVault is a KeyVault in memory.
type memoryVault struct {
	mu     sync.Mutex
	values map[string]string
	err    error
	calls  int
}

func (v *memoryVault) EnsureAdminHiddenSecret(_ context.Context, name string, generate func() (string, error)) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.calls++
	if v.err != nil {
		return "", v.err
	}
	if v.values == nil {
		v.values = map[string]string{}
	}
	if value, ok := v.values[name]; ok {
		return value, nil
	}
	value, err := generate()
	if err != nil {
		return "", err
	}
	v.values[name] = value
	return value, nil
}

// memoryStore is the outbox in memory.
type memoryStore struct {
	mu            sync.Mutex
	queue         []int64
	notifications map[int64]Notification
	subscriptions map[string]memorySubscription
	deliveries    map[string]*memoryDelivery
	successes     map[string]int
	recordFails   int
	records       int
	leases        int
	now           func() time.Time
}

type memorySubscription struct {
	userID       int64
	endpoint     string
	p256dh, auth string
	rejected     int
}

type memoryDelivery struct {
	Delivery
	nextAttempt time.Time
}

func newMemoryStore(now func() time.Time) *memoryStore {
	return &memoryStore{notifications: map[int64]Notification{}, subscriptions: map[string]memorySubscription{},
		deliveries: map[string]*memoryDelivery{}, successes: map[string]int{}, now: now}
}

// notify writes notification rows as their writers do; the outbox trigger
// queues the two types it watches.
func (s *memoryStore) notify(rows ...Notification) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, row := range rows {
		s.notifications[row.ID] = row
		if row.EventType == EventRunFinished || row.EventType == EventApprovalWaiting {
			s.queue = append(s.queue, row.ID)
		}
	}
}

func deliveryKey(subscriptionID string, notificationID int64) string {
	return subscriptionID + "/" + string(rune(notificationID))
}

func (s *memoryStore) FanOut(_ context.Context, limit int, plan Plan, expires time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	taken := s.queue[:min(limit, len(s.queue))]
	s.queue = s.queue[len(taken):]
	for _, id := range taken {
		row, ok := s.notifications[id]
		if !ok {
			continue
		}
		payload, urgency, ok := plan(row)
		if !ok {
			continue
		}
		for subID, sub := range s.subscriptions {
			key := deliveryKey(subID, row.ID)
			if _, ok := s.deliveries[key]; ok || sub.userID != row.UserID {
				continue
			}
			s.deliveries[key] = &memoryDelivery{Delivery: Delivery{NotificationID: row.ID, SubscriptionID: subID,
				Endpoint: sub.endpoint, P256dh: sub.p256dh, Auth: sub.auth, Payload: payload, Urgency: urgency,
				ExpiresAt: expires}, nextAttempt: s.now()}
		}
	}
	return len(taken), nil
}

func (s *memoryStore) ClaimDue(_ context.Context, limit int, lease time.Duration) ([]Delivery, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.leases++
	leaseID := "lease-" + string(rune('a'+s.leases))
	var due []Delivery
	for key, delivery := range s.deliveries {
		if !s.now().Before(delivery.ExpiresAt) {
			delete(s.deliveries, key)
			continue
		}
		if len(due) < limit && !s.now().Before(delivery.nextAttempt) {
			delivery.LeaseID, delivery.nextAttempt = leaseID, s.now().Add(lease)
			due = append(due, delivery.Delivery)
		}
	}
	return due, nil
}

func (s *memoryStore) Record(_ context.Context, outcomes []Outcome) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records++
	if s.recordFails > 0 {
		s.recordFails--
		return errors.New("database down")
	}
	for _, outcome := range outcomes {
		subID := outcome.Delivery.SubscriptionID
		key := deliveryKey(subID, outcome.Delivery.NotificationID)
		held := s.deliveries[key] != nil && s.deliveries[key].LeaseID == outcome.Delivery.LeaseID
		switch outcome.Action {
		case Delivered:
			if held {
				delete(s.deliveries, key)
			}
			if sub, ok := s.subscriptions[subID]; ok {
				sub.rejected = 0
				s.subscriptions[subID] = sub
			}
			s.successes[subID]++
		case Gone:
			s.deleteSubscription(subID)
		case Rejected:
			if held {
				delete(s.deliveries, key)
			}
			if sub, ok := s.subscriptions[subID]; ok {
				sub.rejected++
				s.subscriptions[subID] = sub
				if sub.rejected >= MaxRejectedSends {
					s.deleteSubscription(subID)
				}
			}
		case Retry:
			if held {
				s.deliveries[key].Attempts++
				s.deliveries[key].LeaseID, s.deliveries[key].nextAttempt = "", outcome.RetryAt
			}
		case Drop:
			if held {
				delete(s.deliveries, key)
			}
		}
	}
	return nil
}

func (s *memoryStore) deleteSubscription(id string) {
	delete(s.subscriptions, id)
	for k, d := range s.deliveries {
		if d.SubscriptionID == id {
			delete(s.deliveries, k)
		}
	}
}

// browser is a push subscriber: its keys decrypt what the endpoint gets.
type browser struct {
	private *ecdh.PrivateKey
	auth    []byte
}

func newBrowser(t *testing.T) browser {
	t.Helper()
	private, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	_, _ = rand.Read(auth)
	return browser{private: private, auth: auth}
}

func (b browser) keys() (string, string) {
	return base64.RawURLEncoding.EncodeToString(b.private.PublicKey().Bytes()),
		base64.RawURLEncoding.EncodeToString(b.auth)
}

// pushService is a fake push endpoint: it decrypts every message with the
// browser's keys and answers the status the test sets per path.
type pushService struct {
	t       *testing.T
	browser browser
	mu      sync.Mutex
	status  map[string]int
	got     map[string][]Payload
	headers []http.Header
}

func (p *pushService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	p.mu.Lock()
	defer p.mu.Unlock()
	if r.Header.Get("Content-Encoding") != "aes128gcm" || !strings.HasPrefix(r.Header.Get("Authorization"), "vapid t=") {
		p.t.Errorf("headers = %v", r.Header)
	}
	p.headers = append(p.headers, r.Header.Clone())
	var payload Payload
	if err := json.Unmarshal(decrypt(p.t, body, p.browser.private, p.browser.auth), &payload); err != nil {
		p.t.Errorf("payload: %v", err)
	}
	p.got[r.URL.Path] = append(p.got[r.URL.Path], payload)
	status := p.status[r.URL.Path]
	if status == 0 {
		status = http.StatusCreated
	}
	w.WriteHeader(status)
}

func runFinishedRow(id, user int64, outcome string) Notification {
	meta, _ := json.Marshal(map[string]any{
		"outcome": outcome, "project_id": 3, "conversation_uuid": "0b7f5c86-29f4-4c5a-9b0f-5a6f8e0b2c11",
		"chain_root_execution_id": strings.Repeat("a", 32), "execution_id": strings.Repeat("b", 32),
		"origin": "cloud", "continuations": 2,
	})
	return Notification{ID: id, UserID: user, ProjectID: 3, EventType: EventRunFinished, Meta: meta}
}

type dispatchFixture struct {
	store   *memoryStore
	push    *pushService
	server  *httptest.Server
	clock   time.Time
	service *Service
	d       *Dispatcher
	enabled bool
}

func newDispatchFixture(t *testing.T) *dispatchFixture {
	t.Helper()
	f := &dispatchFixture{clock: time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC), enabled: true}
	f.store = newMemoryStore(func() time.Time { return f.clock })
	f.push = &pushService{t: t, browser: newBrowser(t), status: map[string]int{}, got: map[string][]Payload{}}
	f.server = httptest.NewTLSServer(f.push)
	t.Cleanup(f.server.Close)
	f.service = NewService(NewKeySource(&memoryVault{}, nil),
		SettingsFunc(func(context.Context) bool { return f.enabled }), "mailto:ops@example.com")
	d, err := NewDispatcher(f.store, f.service, f.server.Client(), nil)
	if err != nil {
		t.Fatal(err)
	}
	d.now = func() time.Time { return f.clock }
	f.d = d
	return f
}

func (f *dispatchFixture) subscribe(id string, user int64, path string) {
	p256dh, auth := f.push.browser.keys()
	f.store.subscriptions[id] = memorySubscription{userID: user, endpoint: f.server.URL + path, p256dh: p256dh, auth: auth}
}

func TestDispatcherSendsAContentFreeDecryptablePayload(t *testing.T) {
	f := newDispatchFixture(t)
	f.subscribe("s1", 7, "/a")
	f.subscribe("s2", 8, "/other-user")
	f.store.notify(
		runFinishedRow(1, 7, "waiting"),
		Notification{ID: 2, UserID: 7, ProjectID: 3, EventType: "mention", Meta: json.RawMessage(`{"text":"secret prompt"}`)},
		Notification{ID: 3, UserID: 7, ProjectID: 3, EventType: EventApprovalWaiting, Meta: json.RawMessage(
			`{"conversation_uuid":"0b7f5c86-29f4-4c5a-9b0f-5a6f8e0b2c11","message_id":"0b7f5c86-29f4-4c5a-9b0f-5a6f8e0b2c12",` +
				`"execution_id":"` + strings.Repeat("c", 32) + `","tool_name":"delete_repo","message":"A schedule run waits"}`)},
	)
	if err := f.d.Dispatch(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := f.push.got["/a"]
	if len(got) != 2 || len(f.push.got["/other-user"]) != 0 {
		t.Fatalf("sends = %v", f.push.got)
	}
	byType := map[string]Payload{}
	for _, payload := range got {
		byType[payload.Type] = payload
	}
	if p := byType[EventRunFinished]; p.NotificationID != 1 || p.Outcome != "waiting" || p.ProjectID != 3 ||
		p.ConversationUUID != "0b7f5c86-29f4-4c5a-9b0f-5a6f8e0b2c11" {
		t.Fatalf("run_finished payload = %+v", p)
	}
	if p := byType[EventApprovalWaiting]; p.NotificationID != 3 || p.Outcome != "" || p.ExecutionID != strings.Repeat("c", 32) {
		t.Fatalf("approval payload = %+v", p)
	}
	for _, header := range f.push.headers {
		if header.Get("Urgency") != "high" || header.Get("TTL") != "86400" {
			t.Fatalf("headers = %v", header)
		}
	}
	if len(f.store.queue) != 0 || len(f.store.deliveries) != 0 || f.store.successes["s1"] != 2 {
		t.Fatalf("queue = %v, deliveries = %d, successes = %v", f.store.queue, len(f.store.deliveries), f.store.successes)
	}
	// A second pass sends nothing new.
	if err := f.d.Dispatch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.push.got["/a"]) != 2 {
		t.Fatal("a pass resent a delivered message")
	}
}

func TestDispatcherDeletesAGoneSubscription(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusGone} {
		f := newDispatchFixture(t)
		f.subscribe("gone", 7, "/gone")
		f.subscribe("live", 7, "/live")
		f.push.status["/gone"] = status
		f.store.notify(runFinishedRow(1, 7, "completed"))
		if err := f.d.Dispatch(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, ok := f.store.subscriptions["gone"]; ok {
			t.Fatalf("%d did not delete the subscription", status)
		}
		if _, ok := f.store.subscriptions["live"]; !ok || f.store.successes["live"] != 1 {
			t.Fatalf("%d touched the other subscription", status)
		}
	}
}

func TestDispatcherRetriesWithBackoffInsideTheTTL(t *testing.T) {
	f := newDispatchFixture(t)
	f.subscribe("s1", 7, "/flaky")
	f.push.status["/flaky"] = http.StatusServiceUnavailable
	f.store.notify(runFinishedRow(1, 7, "failed"))
	var outcome Outcome
	f.d.client = recordingDoer{next: f.server.Client(), seen: func(*http.Request) {}}
	if err := f.d.Dispatch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.store.deliveries) != 1 {
		t.Fatalf("the failed delivery is gone: %d", len(f.store.deliveries))
	}
	for _, delivery := range f.store.deliveries {
		if delivery.Attempts != 1 {
			t.Fatalf("attempts = %d", delivery.Attempts)
		}
		outcome = f.d.send(context.Background(), Keys{}, "", delivery.Delivery)
	}
	if outcome.Action != Drop {
		t.Fatal("a send without keys must drop")
	}
	// The push service heals: the pass after the backoff delivers.
	f.push.status["/flaky"] = http.StatusCreated
	f.clock = f.clock.Add(firstRetry)
	if err := f.d.Dispatch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.store.deliveries) != 0 || f.store.successes["s1"] != 1 {
		t.Fatalf("not delivered after the heal: %d left", len(f.store.deliveries))
	}

	// After 24 hours the delivery is dropped, not retried.
	f.push.status["/flaky"] = http.StatusInternalServerError
	f.store.notify(runFinishedRow(2, 7, "failed"))
	if err := f.d.Dispatch(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.clock = f.clock.Add(TTL)
	if err := f.d.Dispatch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.store.deliveries) != 0 {
		t.Fatal("a delivery outlived the TTL")
	}
}

func TestRetryNeverPassesTheTTL(t *testing.T) {
	f := newDispatchFixture(t)
	outcome := f.d.retry(Outcome{Delivery: Delivery{Attempts: 3, ExpiresAt: f.clock.Add(time.Minute)}})
	if outcome.Action != Drop {
		t.Fatalf("a retry past the TTL = %v", outcome.Action)
	}
	outcome = f.d.retry(Outcome{Delivery: Delivery{Attempts: 0, ExpiresAt: f.clock.Add(TTL)}})
	if outcome.Action != Retry || !outcome.RetryAt.Equal(f.clock.Add(firstRetry)) {
		t.Fatalf("first retry = %+v", outcome)
	}
}

func TestBackoffDoublesToOneHour(t *testing.T) {
	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute,
		16 * time.Minute, 32 * time.Minute, time.Hour, time.Hour}
	for i, expected := range want {
		if got := Backoff(i + 1); got != expected {
			t.Fatalf("Backoff(%d) = %v, want %v", i+1, got, expected)
		}
	}
}

func TestDispatcherSkipsWhilePushIsOff(t *testing.T) {
	f := newDispatchFixture(t)
	f.subscribe("s1", 7, "/a")
	f.enabled = false
	f.store.notify(runFinishedRow(1, 7, "completed"))
	if err := f.d.Dispatch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.store.queue) != 0 || len(f.store.deliveries) != 0 || len(f.push.got) != 0 {
		t.Fatal("a pass with push off added a delivery or sent, or did not drain the outbox")
	}
	// Turning push on later does not send the old row.
	f.enabled = true
	if err := f.d.Dispatch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.push.got) != 0 {
		t.Fatal("an old row was sent after push came on")
	}
}

func TestDispatcherClaimsUnderALease(t *testing.T) {
	f := newDispatchFixture(t)
	f.subscribe("s1", 7, "/a")
	f.store.notify(runFinishedRow(1, 7, "completed"))
	if _, err := f.store.FanOut(context.Background(), NotificationBatch, BuildPayload, f.clock.Add(TTL)); err != nil {
		t.Fatal(err)
	}
	// Another replica claimed the delivery and is sending it.
	claimed, _ := f.store.ClaimDue(context.Background(), DeliveryBatch, ClaimLease)
	if len(claimed) != 1 {
		t.Fatalf("claimed = %d", len(claimed))
	}
	if err := f.d.Dispatch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.push.got) != 0 {
		t.Fatal("a claimed delivery was sent by a second sender")
	}
	// That replica died: after the lease the delivery goes out once.
	f.clock = f.clock.Add(ClaimLease)
	if err := f.d.Dispatch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.push.got["/a"]) != 1 || len(f.store.deliveries) != 0 {
		t.Fatalf("after the lease: sends = %d, deliveries = %d", len(f.push.got["/a"]), len(f.store.deliveries))
	}
	// The dead replica's late record changes nothing.
	if err := f.store.Record(context.Background(), []Outcome{{Delivery: claimed[0], Action: Retry,
		RetryAt: f.clock.Add(time.Hour)}}); err != nil || len(f.store.deliveries) != 0 {
		t.Fatalf("a stale claim's record = %v, deliveries = %d", err, len(f.store.deliveries))
	}
}

func TestDispatcherRecordsAgainAfterAFailedWrite(t *testing.T) {
	f := newDispatchFixture(t)
	f.subscribe("s1", 7, "/a")
	f.store.notify(runFinishedRow(1, 7, "completed"))
	f.store.recordFails = recordAttempts - 1
	ctx, cancel := context.WithCancel(context.Background())
	f.d.client = cancelAfterDoer{next: f.server.Client(), cancel: cancel}
	// The pass is cancelled once the send is out (a shutdown); the write
	// still happens, and a failed write is tried again.
	if err := f.d.Dispatch(ctx); err != nil {
		t.Fatal(err)
	}
	if f.store.records != recordAttempts || len(f.store.deliveries) != 0 || f.store.successes["s1"] != 1 {
		t.Fatalf("records = %d, deliveries = %d", f.store.records, len(f.store.deliveries))
	}
}

func TestDispatcherRejectedTokenDropsAndThenDeletes(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		f := newDispatchFixture(t)
		f.subscribe("refused", 7, "/refused")
		f.push.status["/refused"] = status
		for id := range int64(MaxRejectedSends) {
			f.store.notify(runFinishedRow(id+1, 7, "failed"))
			if err := f.d.Dispatch(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(f.store.deliveries) != 0 {
				t.Fatalf("%d: a refused send waits for a retry", status)
			}
			if _, ok := f.store.subscriptions["refused"]; ok != (id+1 < MaxRejectedSends) {
				t.Fatalf("%d: after %d refusals the subscription stayed = %v", status, id+1, ok)
			}
		}
	}
	// A success between refusals starts the count again.
	f := newDispatchFixture(t)
	f.subscribe("s1", 7, "/flip")
	for id := range int64(2 * MaxRejectedSends) {
		f.push.status["/flip"] = http.StatusForbidden
		if id%2 == 1 {
			f.push.status["/flip"] = http.StatusCreated
		}
		f.store.notify(runFinishedRow(id+1, 7, "failed"))
		if err := f.d.Dispatch(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := f.store.subscriptions["s1"]; !ok {
		t.Fatal("refusals with successes between deleted the subscription")
	}
}

func TestBuildPayloadCopiesIDsOnly(t *testing.T) {
	cases := []struct {
		name    string
		row     Notification
		ok      bool
		urgency string
	}{
		{"completed", runFinishedRow(1, 7, "completed"), true, "normal"},
		{"waiting", runFinishedRow(1, 7, "waiting"), true, "high"},
		{"limit", runFinishedRow(1, 7, "limit_reached"), true, "normal"},
		{"cancelled is never sent", runFinishedRow(1, 7, "cancelled"), false, ""},
		{"other type", Notification{ID: 1, EventType: "mention"}, false, ""},
		{"approval", Notification{ID: 1, ProjectID: 2, EventType: EventApprovalWaiting, Meta: json.RawMessage(`{}`)}, true, "high"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, urgency, ok := BuildPayload(tc.row)
			if ok != tc.ok || urgency != tc.urgency {
				t.Fatalf("ok = %v urgency = %q", ok, urgency)
			}
		})
	}
	raw, _, _ := BuildPayload(Notification{ID: 4, ProjectID: 2, EventType: EventRunFinished, Meta: json.RawMessage(
		`{"outcome":"completed","conversation_uuid":"not an id: the prompt text","title":"My secret plan"}`)})
	if strings.Contains(string(raw), "prompt") || strings.Contains(string(raw), "secret") {
		t.Fatalf("payload carries text: %s", raw)
	}
}

func TestConfigNeedsSwitchKeyAndSubject(t *testing.T) {
	on := SettingsFunc(func(context.Context) bool { return true })
	off := SettingsFunc(func(context.Context) bool { return false })
	ctx := context.Background()

	if config := NewService(NewKeySource(&memoryVault{}, nil), on, "mailto:a@example.com").Config(ctx); !config.Enabled || config.PublicKey == "" {
		t.Fatalf("on = %+v", config)
	}
	if config := NewService(NewKeySource(&memoryVault{}, nil), off, "mailto:a@example.com").Config(ctx); config.Enabled || config.PublicKey == "" {
		t.Fatalf("switch off = %+v", config)
	}
	if config := NewService(NewKeySource(nil, nil), on, "mailto:a@example.com").Config(ctx); config.Enabled || config.PublicKey != "" {
		t.Fatalf("no vault = %+v", config)
	}
	failing := &memoryVault{err: errors.New("vault down")}
	source := NewKeySource(failing, nil)
	if config := NewService(source, on, "mailto:a@example.com").Config(ctx); config.Enabled {
		t.Fatalf("failing vault = %+v", config)
	}
	NewService(source, on, "mailto:a@example.com").Config(ctx)
	if failing.calls != 1 {
		t.Fatalf("a failed vault is asked again before the retry wait: %d calls", failing.calls)
	}
	if config := NewService(NewKeySource(&memoryVault{}, nil), on, "").Config(ctx); config.Enabled {
		t.Fatalf("no subject = %+v", config)
	}
}

func TestKeySourceKeepsOneKeyPerInstall(t *testing.T) {
	vault := &memoryVault{}
	first, ok := NewKeySource(vault, nil).Keys(context.Background())
	second, ok2 := NewKeySource(vault, nil).Keys(context.Background())
	if !ok || !ok2 || first.Public != second.Public {
		t.Fatal("two sources over one vault disagree")
	}
	other, _ := NewKeySource(&memoryVault{}, nil).Keys(context.Background())
	if other.Public == first.Public {
		t.Fatal("two installs share a key")
	}
}

// slowVault answers after release; it counts the round trips.
type slowVault struct {
	memoryVault
	started chan struct{}
	release chan struct{}
}

func (v *slowVault) EnsureAdminHiddenSecret(ctx context.Context, name string, generate func() (string, error)) (string, error) {
	v.started <- struct{}{}
	<-v.release
	return v.memoryVault.EnsureAdminHiddenSecret(ctx, name, generate)
}

func TestKeySourceSharesOneVaultRoundTrip(t *testing.T) {
	vault := &slowVault{started: make(chan struct{}, 8), release: make(chan struct{})}
	source := NewKeySource(vault, nil)
	var group sync.WaitGroup
	publics := make([]string, 4)
	for i := range publics {
		group.Go(func() {
			keys, _ := source.Keys(context.Background())
			publics[i] = keys.Public
		})
	}
	<-vault.started
	// While the vault answers, the source's lock is free.
	if _, known, _ := source.cached(); known {
		t.Fatal("a load in flight reads as known")
	}
	close(vault.release)
	group.Wait()
	if vault.calls != 1 {
		t.Fatalf("vault round trips = %d, want 1", vault.calls)
	}
	for _, public := range publics {
		if public == "" || public != publics[0] {
			t.Fatalf("callers disagree: %v", publics)
		}
	}
}

func TestSubject(t *testing.T) {
	cases := []struct{ explicit, base, want string }{
		{"mailto:ops@example.com", "", "mailto:ops@example.com"},
		{"https://ai.example.com/contact", "", "https://ai.example.com/contact"},
		{"", "https://ai.example.com/elitea/", "https://ai.example.com"},
		{"", "http://ai.example.com", ""},
		{"ops@example.com", "https://ai.example.com", ""},
		{"", "", ""},
	}
	for _, tc := range cases {
		got, err := Subject(tc.explicit, tc.base)
		if got != tc.want || (tc.want == "") != (err != nil) {
			t.Errorf("Subject(%q, %q) = %q, %v; want %q", tc.explicit, tc.base, got, err, tc.want)
		}
	}
}

// cancelAfterDoer sends, then cancels the pass.
type cancelAfterDoer struct {
	next   HTTPDoer
	cancel context.CancelFunc
}

func (d cancelAfterDoer) Do(request *http.Request) (*http.Response, error) {
	defer d.cancel()
	return d.next.Do(request.WithContext(context.WithoutCancel(request.Context())))
}

type recordingDoer struct {
	next HTTPDoer
	seen func(*http.Request)
}

func (d recordingDoer) Do(request *http.Request) (*http.Response, error) {
	d.seen(request)
	return d.next.Do(request)
}
