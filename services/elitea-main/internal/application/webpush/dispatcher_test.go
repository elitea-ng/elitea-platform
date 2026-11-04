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
	locked        bool
	cursor        int64
	notifications []Notification
	subscriptions map[string]memorySubscription
	deliveries    map[string]*Delivery
	successes     map[string]int
	now           func() time.Time
}

type memorySubscription struct {
	userID       int64
	endpoint     string
	p256dh, auth string
}

func newMemoryStore(now func() time.Time) *memoryStore {
	return &memoryStore{subscriptions: map[string]memorySubscription{}, deliveries: map[string]*Delivery{},
		successes: map[string]int{}, now: now}
}

func (s *memoryStore) WithSenderLock(_ context.Context, pass func(Pass) error) (bool, error) {
	s.mu.Lock()
	if s.locked {
		s.mu.Unlock()
		return false, nil
	}
	s.locked = true
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.locked = false; s.mu.Unlock() }()
	return true, pass(s)
}

func (s *memoryStore) NewNotifications(_ context.Context, limit int) ([]Notification, int64, error) {
	var rows []Notification
	last := int64(0)
	for _, row := range s.notifications {
		if row.ID <= s.cursor || len(rows) >= limit {
			continue
		}
		last = row.ID
		if row.EventType == EventRunFinished || row.EventType == EventApprovalWaiting {
			rows = append(rows, row)
		}
	}
	return rows, last, nil
}

func (s *memoryStore) Enqueue(_ context.Context, row Notification, payload []byte, urgency string, expires time.Time) error {
	for id, sub := range s.subscriptions {
		if sub.userID != row.UserID {
			continue
		}
		key := id + "/" + string(rune(row.ID))
		if _, ok := s.deliveries[key]; ok {
			continue
		}
		s.deliveries[key] = &Delivery{NotificationID: row.ID, SubscriptionID: id, Endpoint: sub.endpoint,
			P256dh: sub.p256dh, Auth: sub.auth, Payload: payload, Urgency: urgency, ExpiresAt: expires}
	}
	return nil
}

func (s *memoryStore) AdvanceCursor(_ context.Context, to int64) error {
	s.cursor = to
	return nil
}

func (s *memoryStore) DueDeliveries(_ context.Context, limit int) ([]Delivery, error) {
	var due []Delivery
	for key, delivery := range s.deliveries {
		if !s.now().Before(delivery.ExpiresAt) {
			delete(s.deliveries, key)
			continue
		}
		if len(due) < limit {
			due = append(due, *delivery)
		}
	}
	return due, nil
}

func (s *memoryStore) Record(_ context.Context, outcomes []Outcome) error {
	for _, outcome := range outcomes {
		key := outcome.Delivery.SubscriptionID + "/" + string(rune(outcome.Delivery.NotificationID))
		switch outcome.Action {
		case Delivered:
			delete(s.deliveries, key)
			s.successes[outcome.Delivery.SubscriptionID]++
		case Gone:
			delete(s.subscriptions, outcome.Delivery.SubscriptionID)
			for k, d := range s.deliveries {
				if d.SubscriptionID == outcome.Delivery.SubscriptionID {
					delete(s.deliveries, k)
				}
			}
		case Retry:
			s.deliveries[key].Attempts++
		case Drop:
			delete(s.deliveries, key)
		}
	}
	return nil
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
	f.store.notifications = []Notification{
		runFinishedRow(1, 7, "waiting"),
		{ID: 2, UserID: 7, ProjectID: 3, EventType: "mention", Meta: json.RawMessage(`{"text":"secret prompt"}`)},
		{ID: 3, UserID: 7, ProjectID: 3, EventType: EventApprovalWaiting, Meta: json.RawMessage(
			`{"conversation_uuid":"0b7f5c86-29f4-4c5a-9b0f-5a6f8e0b2c11","message_id":"0b7f5c86-29f4-4c5a-9b0f-5a6f8e0b2c12",` +
				`"execution_id":"` + strings.Repeat("c", 32) + `","tool_name":"delete_repo","message":"A schedule run waits"}`)},
	}
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
	if f.store.cursor != 3 || len(f.store.deliveries) != 0 || f.store.successes["s1"] != 2 {
		t.Fatalf("cursor = %d, deliveries = %d, successes = %v", f.store.cursor, len(f.store.deliveries), f.store.successes)
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
		f.store.notifications = []Notification{runFinishedRow(1, 7, "completed")}
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
	f.store.notifications = []Notification{runFinishedRow(1, 7, "failed")}
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
		outcome = f.d.send(context.Background(), Keys{}, "", *delivery)
	}
	if outcome.Action != Drop {
		t.Fatal("a send without keys must drop")
	}
	// The push service heals: the next pass delivers.
	f.push.status["/flaky"] = http.StatusCreated
	if err := f.d.Dispatch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.store.deliveries) != 0 || f.store.successes["s1"] != 1 {
		t.Fatalf("not delivered after the heal: %d left", len(f.store.deliveries))
	}

	// After 24 hours the delivery is dropped, not retried.
	f.push.status["/flaky"] = http.StatusInternalServerError
	f.store.notifications = append(f.store.notifications, runFinishedRow(2, 7, "failed"))
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
	f.store.notifications = []Notification{runFinishedRow(1, 7, "completed")}
	if err := f.d.Dispatch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.store.cursor != 1 || len(f.store.deliveries) != 0 || len(f.push.got) != 0 {
		t.Fatal("a pass with push off queued or sent, or did not move the cursor")
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

func TestDispatcherIsASingleSender(t *testing.T) {
	f := newDispatchFixture(t)
	f.store.locked = true // another replica holds the lock
	f.subscribe("s1", 7, "/a")
	f.store.notifications = []Notification{runFinishedRow(1, 7, "completed")}
	if err := f.d.Dispatch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.store.cursor != 0 || len(f.push.got) != 0 {
		t.Fatal("a pass ran without the lock")
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

type recordingDoer struct {
	next HTTPDoer
	seen func(*http.Request)
}

func (d recordingDoer) Do(request *http.Request) (*http.Response, error) {
	d.seen(request)
	return d.next.Do(request)
}
