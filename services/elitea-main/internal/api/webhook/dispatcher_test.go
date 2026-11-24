package webhook

// Unit coverage for the event→payload mapping and the signature — the fast,
// DB-free half of #876's dispatcher tests. The Postgres round trip (a real
// delivery, logged, and a real redelivery, both persisted) lives in
// dispatcher_postgres_integration_test.go, package webhook_test.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// mockDeliveryRepository is an in-memory DeliveryRepository — the delivery-
// log equivalent of handler_test.go's mockWebhookRepo. HandleDomainEvent's
// whole contract is that it does NOT block its caller: it delivers on its
// own goroutine (dispatcher.go's HandleDomainEvent), which calls Create()
// concurrently with the test goroutine's waitFor polls and post-wait
// assertions. mu guards every access to created/nextID so -race sees
// synchronized reads and writes instead of a bare shared slice.
type mockDeliveryRepository struct {
	mu      sync.Mutex
	created []Delivery
	nextID  int
}

func (m *mockDeliveryRepository) Create(_ context.Context, d Delivery) (Delivery, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextID++
	d.ID = "del-" + strconv.Itoa(m.nextID)
	d.CreatedAt = time.Now()
	d.UpdatedAt = d.CreatedAt
	m.created = append(m.created, d)
	return d, nil
}

func (m *mockDeliveryRepository) ListRecent(_ context.Context, projectID, webhookID string, limit int) ([]Delivery, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Delivery
	for i := len(m.created) - 1; i >= 0 && len(out) < limit; i-- {
		d := m.created[i]
		if d.ProjectID == projectID && d.WebhookID == webhookID {
			out = append(out, d)
		}
	}
	return out, nil
}

func (m *mockDeliveryRepository) Get(_ context.Context, projectID, webhookID, deliveryID string) (Delivery, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, d := range m.created {
		if d.ID == deliveryID && d.ProjectID == projectID && d.WebhookID == webhookID {
			return d, nil
		}
	}
	return Delivery{}, ErrDeliveryNotFound
}

// snapshotCreated returns a locked copy of the rows Create has logged so
// far. Tests must use this (and count()) instead of reading m.created
// directly — a direct read races the dispatcher's delivery goroutine.
func (m *mockDeliveryRepository) snapshotCreated() []Delivery {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Delivery, len(m.created))
	copy(out, m.created)
	return out
}

func (m *mockDeliveryRepository) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.created)
}

func referenceSignature(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// waitFor polls until cond returns true or the deadline passes, so tests do
// not race the dispatcher's own goroutine (HandleDomainEvent's whole
// contract is that it does NOT block its caller — see events.Publisher.Emit
// and this dispatcher's own doc comment).
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met before deadline")
}

/* ── payload mapping ─────────────────────────────────────────────────── */

func TestDeliveryBodyCarriesTypeProjectPayloadAndTimestamp(t *testing.T) {
	body, err := deliveryBody("conversation.created", "proj-9", map[string]any{"conversation_id": "conv-1"})
	if err != nil {
		t.Fatalf("deliveryBody: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded["type"] != "conversation.created" {
		t.Errorf("type = %v", decoded["type"])
	}
	if decoded["project_id"] != "proj-9" {
		t.Errorf("project_id = %v", decoded["project_id"])
	}
	payload, ok := decoded["payload"].(map[string]any)
	if !ok || payload["conversation_id"] != "conv-1" {
		t.Errorf("payload = %v", decoded["payload"])
	}
	if _, ok := decoded["timestamp"].(string); !ok {
		t.Errorf("timestamp missing or not a string: %v", decoded["timestamp"])
	}
}

/* ── signing ──────────────────────────────────────────────────────────── */

func TestSignBodyMatchesReferenceHMAC(t *testing.T) {
	body := []byte(`{"type":"conversation.created"}`)
	got := signBody("s3cr3t", body)
	want := referenceSignature("s3cr3t", body)
	if got != want {
		t.Errorf("signBody = %s, want %s", got, want)
	}
	// A one-byte change in the secret or the body must change the digest —
	// otherwise a receiver's constant-time compare would accept forgeries.
	if signBody("different", body) == got {
		t.Error("signature did not change with the secret")
	}
	if signBody("s3cr3t", []byte(`{"type":"other"}`)) == got {
		t.Error("signature did not change with the body")
	}
}

func TestDeliverySignsWithTheWebhooksSecretAndCarriesTheEventTypeHeader(t *testing.T) {
	var gotSignature, gotEventType string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSignature = r.Header.Get(SignatureHeader)
		gotEventType = r.Header.Get(EventTypeHeader)
		gotBody, _ = readAll(r)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	deliveries := &mockDeliveryRepository{}
	repo := &mockWebhookRepo{webhooks: []Webhook{{
		ID: "wh-1", ProjectID: "proj-1", URL: server.URL,
		Events: []string{"conversation.created"}, Secret: "topsecret", Active: true,
	}}}
	d := NewDispatcher(repo, deliveries)

	d.HandleDomainEvent(context.Background(), "proj-1", "conversation.created", map[string]any{"conversation_id": "c-1"})

	waitFor(t, func() bool { return deliveries.count() == 1 })

	want := "sha256=" + referenceSignature("topsecret", gotBody)
	if gotSignature != want {
		t.Errorf("signature header = %q, want %q", gotSignature, want)
	}
	if gotEventType != "conversation.created" {
		t.Errorf("event type header = %q", gotEventType)
	}

	logged := deliveries.snapshotCreated()[0]
	if logged.Status != DeliveryStatusSuccess {
		t.Errorf("status = %s, want success", logged.Status)
	}
	if logged.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", logged.Attempts)
	}
	if logged.ResponseCode == nil || *logged.ResponseCode != http.StatusOK {
		t.Errorf("response code = %v, want 200", logged.ResponseCode)
	}
}

func readAll(r *http.Request) ([]byte, error) {
	defer func() { _ = r.Body.Close() }()
	return io.ReadAll(r.Body)
}

/* ── retries ──────────────────────────────────────────────────────────── */

func TestDispatcherRetriesAndSucceedsOnTheFinalAttempt(t *testing.T) {
	var attempts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	deliveries := &mockDeliveryRepository{}
	repo := &mockWebhookRepo{webhooks: []Webhook{{
		ID: "wh-1", ProjectID: "proj-1", URL: server.URL,
		Events: []string{"conversation.created"}, Secret: "s", Active: true,
	}}}
	d := NewDispatcher(repo, deliveries)

	d.HandleDomainEvent(context.Background(), "proj-1", "conversation.created", map[string]any{})
	waitFor(t, func() bool { return deliveries.count() == 1 })

	logged := deliveries.snapshotCreated()[0]
	if logged.Status != DeliveryStatusSuccess {
		t.Errorf("status = %s, want success", logged.Status)
	}
	if logged.Attempts != 3 {
		t.Errorf("attempts = %d, want 3", logged.Attempts)
	}
	if got := atomic.LoadInt32(&attempts); got != 3 {
		t.Errorf("server saw %d requests, want 3", got)
	}
}

func TestDispatcherLogsFailureAfterExhaustingAttempts(t *testing.T) {
	var attempts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	deliveries := &mockDeliveryRepository{}
	repo := &mockWebhookRepo{webhooks: []Webhook{{
		ID: "wh-1", ProjectID: "proj-1", URL: server.URL,
		Events: []string{"conversation.created"}, Secret: "s", Active: true,
	}}}
	d := NewDispatcher(repo, deliveries)

	d.HandleDomainEvent(context.Background(), "proj-1", "conversation.created", map[string]any{})
	waitFor(t, func() bool { return deliveries.count() == 1 })

	logged := deliveries.snapshotCreated()[0]
	if logged.Status != DeliveryStatusFailed {
		t.Errorf("status = %s, want failed", logged.Status)
	}
	if logged.Attempts != maxDeliveryAttempts {
		t.Errorf("attempts = %d, want %d", logged.Attempts, maxDeliveryAttempts)
	}
	if logged.ResponseCode == nil || *logged.ResponseCode != http.StatusInternalServerError {
		t.Errorf("response code = %v, want 500", logged.ResponseCode)
	}
	if logged.LastError == "" {
		t.Error("last_error is empty on a failed delivery")
	}
	if got := atomic.LoadInt32(&attempts); int(got) != maxDeliveryAttempts {
		t.Errorf("server saw %d requests, want %d", got, maxDeliveryAttempts)
	}
}

/* ── SSRF guard ───────────────────────────────────────────────────────── */

// TestDispatcherBlocksAndDoesNotRetryASSRFRefusedDestination proves attempt()
// treats a guard refusal as terminal, not retryable: the destination refuses
// identically every time, so retrying it would only spend the backoff budget
// to reach the same answer three times. It also proves no network attempt is
// even made — the guard check runs BEFORE any dial.
func TestDispatcherBlocksAndDoesNotRetryASSRFRefusedDestination(t *testing.T) {
	deliveries := &mockDeliveryRepository{}
	repo := &mockWebhookRepo{webhooks: []Webhook{{
		ID: "wh-1", ProjectID: "proj-1", URL: "http://169.254.169.254/latest/meta-data/",
		Events: []string{"conversation.created"}, Secret: "s", Active: true,
	}}}
	guard := NewDestinationGuardWithResolver(nil, fakeIPResolver{})
	d := NewDispatcher(repo, deliveries, WithGuard(guard))

	d.HandleDomainEvent(context.Background(), "proj-1", "conversation.created", map[string]any{})
	waitFor(t, func() bool { return deliveries.count() == 1 })

	logged := deliveries.snapshotCreated()[0]
	if logged.Status != DeliveryStatusBlocked {
		t.Errorf("status = %s, want %s", logged.Status, DeliveryStatusBlocked)
	}
	if logged.Attempts != 1 {
		t.Errorf("attempts = %d, want 1 (a blocked destination is not retried)", logged.Attempts)
	}
	if logged.LastError == "" {
		t.Error("last_error is empty on a blocked delivery")
	}
	if logged.ResponseCode != nil {
		t.Errorf("response code = %v, want nil (no dial was attempted)", logged.ResponseCode)
	}
}

// TestDispatcherWithoutAGuardDialsUnchecked documents the deliberate degrade:
// a Dispatcher built with no WithGuard option performs no SSRF check at all.
// Production always wires one (cmd/elitea-main/main.go); this pins the
// no-guard behaviour so a future change cannot silently make it fail closed
// (which would need its own migration/test update, not a surprise).
func TestDispatcherWithoutAGuardDialsUnchecked(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	deliveries := &mockDeliveryRepository{}
	repo := &mockWebhookRepo{webhooks: []Webhook{{
		ID: "wh-1", ProjectID: "proj-1", URL: server.URL, // a loopback address
		Events: []string{"conversation.created"}, Secret: "s", Active: true,
	}}}
	d := NewDispatcher(repo, deliveries) // no WithGuard

	d.HandleDomainEvent(context.Background(), "proj-1", "conversation.created", map[string]any{})
	waitFor(t, func() bool { return deliveries.count() == 1 })

	snap := deliveries.snapshotCreated()
	if snap[0].Status != DeliveryStatusSuccess {
		t.Errorf("status = %s, want success (an unguarded Dispatcher does not block loopback)", snap[0].Status)
	}
}

/* ── inactive / unsubscribed webhooks are skipped ────────────────────── */

func TestDispatcherSkipsInactiveAndUnsubscribedWebhooks(t *testing.T) {
	deliveries := &mockDeliveryRepository{}
	// ListByEvent is what the real repository filters with (active AND
	// event membership); the mock's ListByEvent already returns the whole
	// list, so this test's repo pre-filters instead, proving the
	// dispatcher's OWN active check (belt-and-suspenders against a
	// repository that returned an inactive row).
	repo := &mockWebhookRepo{webhooks: []Webhook{{
		ID: "wh-inactive", ProjectID: "proj-1", URL: "http://example.invalid",
		Events: []string{"conversation.created"}, Secret: "s", Active: false,
	}}}
	d := NewDispatcher(repo, deliveries)

	d.HandleDomainEvent(context.Background(), "proj-1", "conversation.created", map[string]any{})

	// Give the (should-be-absent) goroutine a moment to have run if the
	// active check were missing, then assert nothing was logged.
	time.Sleep(100 * time.Millisecond)
	if got := deliveries.snapshotCreated(); len(got) != 0 {
		t.Errorf("an inactive webhook was delivered to: %+v", got)
	}
}

/* ── redeliver ────────────────────────────────────────────────────────── */

func TestRedeliverResendsTheExactStoredPayloadAsANewRow(t *testing.T) {
	var bodies [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := readAll(r)
		bodies = append(bodies, body)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	deliveries := &mockDeliveryRepository{}
	repo := &mockWebhookRepo{webhooks: []Webhook{{
		ID: "wh-1", ProjectID: "proj-1", URL: server.URL,
		Events: []string{"conversation.created"}, Secret: "s", Active: true,
	}}}
	d := NewDispatcher(repo, deliveries)

	d.HandleDomainEvent(context.Background(), "proj-1", "conversation.created", map[string]any{"conversation_id": "c-1"})
	waitFor(t, func() bool { return deliveries.count() == 1 })
	original := deliveries.snapshotCreated()[0]

	redelivered, err := d.Redeliver(context.Background(), "proj-1", "wh-1", original.ID)
	if err != nil {
		t.Fatalf("Redeliver: %v", err)
	}
	if redelivered.RedeliveryOf != original.ID {
		t.Errorf("redelivery_of = %q, want %q", redelivered.RedeliveryOf, original.ID)
	}
	if redelivered.ID == original.ID {
		t.Error("Redeliver mutated the original row instead of logging a new one")
	}
	if len(bodies) != 2 {
		t.Fatalf("server saw %d requests, want 2 (original + redelivery)", len(bodies))
	}
	if string(bodies[0]) != string(bodies[1]) {
		t.Errorf("redelivery body differs from the original:\n%s\nvs\n%s", bodies[0], bodies[1])
	}
}

func TestRedeliverRefusesAnUnknownDeliveryID(t *testing.T) {
	deliveries := &mockDeliveryRepository{}
	repo := &mockWebhookRepo{webhooks: []Webhook{{ID: "wh-1", ProjectID: "proj-1", URL: "http://example.invalid"}}}
	d := NewDispatcher(repo, deliveries)

	_, err := d.Redeliver(context.Background(), "proj-1", "wh-1", "does-not-exist")
	if err != ErrDeliveryNotFound {
		t.Errorf("err = %v, want ErrDeliveryNotFound", err)
	}
}

// TestDispatcherDoesNotFollowRedirects proves a delivery never follows a 3xx:
// the signed body is not re-sent to the Location, and the 3xx is logged as a
// failed delivery after one attempt, since retrying cannot change it.
func TestDispatcherDoesNotFollowRedirects(t *testing.T) {
	var followed atomic.Int32
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		followed.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer elsewhere.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/collect", http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	host, port, err := net.SplitHostPort(redirector.Listener.Addr().String())
	if err != nil {
		t.Fatalf("split test server address: %v", err)
	}
	private, err := ParseDestinationAllowlist([]string{"127.0.0.0/8"})
	if err != nil {
		t.Fatalf("parse allowlist: %v", err)
	}
	guard := NewDestinationGuardWithResolver(private, fakeIPResolver{ips: map[string][]net.IPAddr{
		"receiver.example": {{IP: net.ParseIP(host)}},
	}})
	deliveries := &mockDeliveryRepository{}
	repo := &mockWebhookRepo{webhooks: []Webhook{{
		ID: "wh-1", ProjectID: "proj-1", URL: "http://receiver.example:" + port + "/hook",
		Events: []string{"conversation.created"}, Secret: "s", Active: true,
	}}}
	d := NewDispatcher(repo, deliveries, WithGuard(guard))

	d.HandleDomainEvent(context.Background(), "proj-1", "conversation.created", map[string]any{})
	waitFor(t, func() bool { return deliveries.count() == 1 })

	logged := deliveries.snapshotCreated()[0]
	if followed.Load() != 0 {
		t.Fatalf("the redirect target received %d deliveries, want 0", followed.Load())
	}
	if logged.Status != DeliveryStatusFailed {
		t.Errorf("status = %s, want failed", logged.Status)
	}
	if logged.ResponseCode == nil || *logged.ResponseCode != http.StatusTemporaryRedirect {
		t.Errorf("response code = %v, want 307", logged.ResponseCode)
	}
	if logged.Attempts != 1 {
		t.Errorf("attempts = %d, want 1 (a redirect answers the same way every time)", logged.Attempts)
	}
}

/* ── the signing secret comes from the vault (issue 2963) ─────────────── */

// fakeSecretResolver stands in for the project vault.
type fakeSecretResolver struct {
	mu     sync.Mutex
	values map[string]string // keyed by projectID + "|" + reference
	calls  int
}

func (f *fakeSecretResolver) ResolveSecretValue(_ context.Context, projectID, secretRef string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	value, ok := f.values[projectID+"|"+secretRef]
	if !ok {
		return "", errors.New("secret not found")
	}
	return value, nil
}

func (f *fakeSecretResolver) set(key, value string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.values[key] = value
}

func (f *fakeSecretResolver) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// The signature header is the HMAC of the body keyed by the VAULT value the
// row's reference names; the row carries no plaintext at all.
func TestDeliverySignsWithTheVaultValue(t *testing.T) {
	var mu sync.Mutex
	var signatures []string
	var bodies [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := readAll(r)
		mu.Lock()
		signatures = append(signatures, r.Header.Get(SignatureHeader))
		bodies = append(bodies, body)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	const ref = "{{secret.0123456789abcdef0123456789abcdef}}"
	resolver := &fakeSecretResolver{values: map[string]string{"7|" + ref: "value-held-in-the-vault"}}
	deliveries := &mockDeliveryRepository{}
	repo := &mockWebhookRepo{webhooks: []Webhook{{
		ID: "wh-1", ProjectID: "7", URL: server.URL,
		Events: []string{"conversation.created"}, SecretRef: ref, Active: true,
	}}}
	d := NewDispatcher(repo, deliveries, WithSecretResolver(resolver))

	d.HandleDomainEvent(context.Background(), "7", "conversation.created", map[string]any{"conversation_id": "c-1"})
	waitFor(t, func() bool { return deliveries.count() == 1 })

	mu.Lock()
	gotSignature, gotBody := signatures[0], bodies[0]
	mu.Unlock()
	if want := "sha256=" + referenceSignature("value-held-in-the-vault", gotBody); gotSignature != want {
		t.Fatalf("signature header = %q, want %q", gotSignature, want)
	}
	if got := deliveries.snapshotCreated()[0].Status; got != DeliveryStatusSuccess {
		t.Fatalf("status = %s, want success", got)
	}

	// A redelivery re-reads the vault, so a rotated value signs it.
	resolver.set("7|"+ref, "rotated-vault-value")
	if _, err := d.Redeliver(context.Background(), "7", "wh-1", deliveries.snapshotCreated()[0].ID); err != nil {
		t.Fatalf("Redeliver: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := "sha256=" + referenceSignature("rotated-vault-value", bodies[1]); signatures[1] != want {
		t.Fatalf("redelivery signature = %q, want %q", signatures[1], want)
	}
}

// The vault is read once per webhook per dispatch, not once per attempt.
func TestTheSecretIsResolvedOncePerDispatch(t *testing.T) {
	var attempts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&attempts, 1) < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	const ref = "{{secret.once}}"
	resolver := &fakeSecretResolver{values: map[string]string{"7|" + ref: "v"}}
	deliveries := &mockDeliveryRepository{}
	repo := &mockWebhookRepo{webhooks: []Webhook{{
		ID: "wh-1", ProjectID: "7", URL: server.URL,
		Events: []string{"conversation.created"}, SecretRef: ref, Active: true,
	}}}
	d := NewDispatcher(repo, deliveries, WithSecretResolver(resolver))

	d.HandleDomainEvent(context.Background(), "7", "conversation.created", map[string]any{})
	waitFor(t, func() bool { return deliveries.count() == 1 })
	if got := atomic.LoadInt32(&attempts); got != 3 {
		t.Fatalf("attempts = %d, want 3", got)
	}
	if got := resolver.callCount(); got != 1 {
		t.Fatalf("vault reads = %d, want 1 for one dispatch", got)
	}
}

// A webhook with no signing secret at all is not delivered (#2959): nothing
// goes out, and the delivery is logged FAILED with the reason, both on the
// event path and on Redeliver.
func TestAWebhookWithoutASecretIsNotDelivered(t *testing.T) {
	var received int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&received, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	deliveries := &mockDeliveryRepository{}
	repo := &mockWebhookRepo{webhooks: []Webhook{{
		ID: "wh-1", ProjectID: "7", URL: server.URL,
		Events: []string{"conversation.created"}, Active: true,
	}}}
	d := NewDispatcher(repo, deliveries, WithSecretResolver(&fakeSecretResolver{values: map[string]string{}}))

	d.HandleDomainEvent(context.Background(), "7", "conversation.created", map[string]any{})
	waitFor(t, func() bool { return deliveries.count() == 1 })

	if got := atomic.LoadInt32(&received); got != 0 {
		t.Fatalf("receiver saw %d requests, want none: an unsigned delivery went out", got)
	}
	logged := deliveries.snapshotCreated()[0]
	if logged.Status != DeliveryStatusFailed || logged.LastError != errNoSigningSecret.Error() {
		t.Fatalf("logged %s %q, want failed with the no-secret reason", logged.Status, logged.LastError)
	}
	if logged.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1 (the refusal is not retried)", logged.Attempts)
	}
}

// send never builds an unsigned request, even if a caller skips attempt().
func TestSendRefusesAnEmptySecret(t *testing.T) {
	var received int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&received, 1)
	}))
	defer server.Close()
	d := NewDispatcher(&mockWebhookRepo{}, nil)
	if _, err := d.send(context.Background(), Webhook{ID: "wh-1", URL: server.URL}, "", "conversation.created", []byte(`{}`)); err == nil {
		t.Fatal("send accepted an empty secret")
	}
	if atomic.LoadInt32(&received) != 0 {
		t.Fatal("an unsigned request reached the receiver")
	}
}

// A webhook whose secret cannot be read is never sent unsigned: nothing goes
// out, and the delivery is logged FAILED with a reason that names no secret.
func TestAnUnreadableSecretSendsNothing(t *testing.T) {
	for name, resolver := range map[string]SecretResolver{
		"no resolver composed": nil,
		"vault has no value":   &fakeSecretResolver{values: map[string]string{}},
	} {
		t.Run(name, func(t *testing.T) {
			var received int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&received, 1)
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()

			deliveries := &mockDeliveryRepository{}
			repo := &mockWebhookRepo{webhooks: []Webhook{{
				ID: "wh-1", ProjectID: "7", URL: server.URL,
				Events: []string{"conversation.created"}, SecretRef: "{{secret.missing}}", Active: true,
			}}}
			opts := []DispatcherOption{}
			if resolver != nil {
				opts = append(opts, WithSecretResolver(resolver))
			}
			d := NewDispatcher(repo, deliveries, opts...)

			d.HandleDomainEvent(context.Background(), "7", "conversation.created", map[string]any{})
			waitFor(t, func() bool { return deliveries.count() == 1 })

			if got := atomic.LoadInt32(&received); got != 0 {
				t.Fatalf("receiver saw %d requests, want none", got)
			}
			logged := deliveries.snapshotCreated()[0]
			if logged.Status != DeliveryStatusFailed || logged.LastError != errSigningSecretUnavailable.Error() {
				t.Fatalf("logged %s %q, want failed with the unavailable-secret reason", logged.Status, logged.LastError)
			}
		})
	}
}
