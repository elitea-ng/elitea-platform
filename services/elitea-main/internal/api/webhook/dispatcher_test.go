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
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// mockDeliveryRepository is an in-memory DeliveryRepository — the delivery-
// log equivalent of handler_test.go's mockWebhookRepo.
type mockDeliveryRepository struct {
	created []Delivery
	nextID  int
}

func (m *mockDeliveryRepository) Create(_ context.Context, d Delivery) (Delivery, error) {
	m.nextID++
	d.ID = "del-" + strconv.Itoa(m.nextID)
	d.CreatedAt = time.Now()
	d.UpdatedAt = d.CreatedAt
	m.created = append(m.created, d)
	return d, nil
}

func (m *mockDeliveryRepository) ListRecent(_ context.Context, projectID, webhookID string, limit int) ([]Delivery, error) {
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
	for _, d := range m.created {
		if d.ID == deliveryID && d.ProjectID == projectID && d.WebhookID == webhookID {
			return d, nil
		}
	}
	return Delivery{}, ErrDeliveryNotFound
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

	waitFor(t, func() bool { return len(deliveries.created) == 1 })

	want := "sha256=" + referenceSignature("topsecret", gotBody)
	if gotSignature != want {
		t.Errorf("signature header = %q, want %q", gotSignature, want)
	}
	if gotEventType != "conversation.created" {
		t.Errorf("event type header = %q", gotEventType)
	}

	logged := deliveries.created[0]
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
	waitFor(t, func() bool { return len(deliveries.created) == 1 })

	logged := deliveries.created[0]
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
	waitFor(t, func() bool { return len(deliveries.created) == 1 })

	logged := deliveries.created[0]
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
	if len(deliveries.created) != 0 {
		t.Errorf("an inactive webhook was delivered to: %+v", deliveries.created)
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
	waitFor(t, func() bool { return len(deliveries.created) == 1 })
	original := deliveries.created[0]

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
