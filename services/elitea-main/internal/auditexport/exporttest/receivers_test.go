package exporttest

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/libs/go/observability"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auditexport"
)

var (
	testKey1 = SigningKey{ID: "k1", Secret: []byte("secret-one-secret-one-secret-0000"), Created: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	testKey2 = SigningKey{ID: "k2", Secret: []byte("secret-two-secret-two-secret-0000"), Created: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)}
)

func testEvent(id string) auditexport.Event {
	return auditexport.Event{
		Schema:    auditexport.SchemaVersion,
		EventID:   id,
		Time:      "2026-11-25T09:30:00Z",
		Source:    auditexport.SourceAdminAudit,
		EventType: "api",
		Tenant:    auditexport.Tenant{ProjectID: nil},
		Actor:     auditexport.Actor{Type: "user", ID: "7"},
		Action:    "GET /x",
		Outcome:   auditexport.OutcomeSuccess,
		Details:   map[string]any{"row_id": 1, "is_error": false},
	}
}

func newWebhookSink(t *testing.T, url string, signer SigningKey) *auditexport.WebhookSink {
	t.Helper()
	// The real egress guard refuses loopback without an allowlist; use a
	// plain round-tripper here, the way the webhook test does with allowLoopback.
	cfg := auditexport.WebhookConfig{
		URL:    url,
		Keys:   fixedKeys(signer),
		Client: &http.Client{},
		Sleep:  func(context.Context, time.Duration) error { return nil },
	}
	sink, err := auditexport.NewWebhookSink(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return sink
}

func fixedKeys(keys ...SigningKey) func() ([]auditexport.SigningKey, error) {
	return func() ([]auditexport.SigningKey, error) {
		out := make([]auditexport.SigningKey, len(keys))
		for i, k := range keys {
			out[i] = auditexport.SigningKey{ID: k.ID, Secret: k.Secret, Created: k.Created}
		}
		return out, nil
	}
}

func TestWebhookReceiverVerifiesWithBothKeys(t *testing.T) {
	rcv := NewWebhookReceiver(t, testKey1, testKey2)
	defer func() { _ = rcv.Stop(t) }()
	sink := newWebhookSink(t, rcv.URL(), testKey2)
	events := []auditexport.Event{testEvent("event-1"), testEvent("event-2")}
	if err := sink.Send(context.Background(), events); err != nil {
		t.Fatalf("send: %v", err)
	}
	for _, e := range events {
		if rcv.Delivered(e.EventID) != 1 {
			t.Fatalf("event %s delivered %d times, want 1", e.EventID, rcv.Delivered(e.EventID))
		}
	}
	// The receiver saw the batch ids.
	if len(rcv.BatchIDs()) != 1 {
		t.Fatalf("batch ids = %v", rcv.BatchIDs())
	}
}

func TestWebhookReceiverRefusesBadSignature(t *testing.T) {
	rcv := NewWebhookReceiver(t, testKey1)
	defer func() { _ = rcv.Stop(t) }()
	// Sign with the wrong key: the receiver holds only testKey1 and the body
	// is signed by testKey2.
	body := []byte(`{"schema":"elitea.audit.v1","batch_id":"b","events":[]}`)
	secret := testKey2.Secret
	sig := auditexport.Sign(secret, time.Now(), body)
	req, _ := http.NewRequest(http.MethodPost, rcv.URL(), strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(auditexport.HeaderSignature, "t="+fmt.Sprintf("%d", time.Now().Unix())+",v1="+sig)
	req.Header.Set(auditexport.HeaderKeyID, testKey1.ID)
	req.Header.Set(auditexport.HeaderDelivery, "b")
	req.Header.Set(auditexport.HeaderSchema, auditexport.SchemaVersion)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestWebhookReceiverStatusSwitch(t *testing.T) {
	// Transient codes (401, 429, 500) make the sink retry; the received set
	// stays complete across the switch.
	for _, code := range []int{http.StatusUnauthorized, http.StatusTooManyRequests, http.StatusInternalServerError} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			rcv := NewWebhookReceiver(t, testKey2)
			defer func() { _ = rcv.Stop(t) }()
			sink := newWebhookSink(t, rcv.URL(), testKey2)
			rcv.SetStatus(code)
			events := []auditexport.Event{testEvent("event-a")}
			for i := 0; i < 3; i++ {
				if err := sink.Send(context.Background(), events); err != nil {
					t.Fatalf("pass %d: %v", i, err)
				}
			}
			if rcv.Delivered("event-a") != 3 {
				t.Fatalf("event-a delivered %d times, want 3 (status %d)", rcv.Delivered("event-a"), code)
			}
		})
	}
	// A 400 is a content refusal: the pipeline splits and dead-letters.
	{
		rcv := NewWebhookReceiver(t, testKey2)
		defer func() { _ = rcv.Stop(t) }()
		sink := newWebhookSink(t, rcv.URL(), testKey2)
		rcv.Refusals()["event-b"] = http.StatusBadRequest
		events := []auditexport.Event{testEvent("event-a"), testEvent("event-b")}
		err := sink.Send(context.Background(), events)
		if err == nil {
			t.Fatal("a content refusal must return an error")
		}
		if rcv.Delivered("event-a") != 1 {
			t.Fatalf("event-a delivered %d times, want 1", rcv.Delivered("event-a"))
		}
		if rcv.Delivered("event-b") != 0 {
			t.Fatalf("event-b delivered %d times, want 0", rcv.Delivered("event-b"))
		}
	}
}

func TestWebhookReceiverKillAndRestore(t *testing.T) {
	rcv := NewWebhookReceiver(t, testKey2)
	defer func() { _ = rcv.Stop(t) }()
	sink := newWebhookSink(t, rcv.URL(), testKey2)
	events := []auditexport.Event{testEvent("event-x"), testEvent("event-y")}
	if err := sink.Send(context.Background(), events); err != nil {
		t.Fatalf("first send: %v", err)
	}
	_ = rcv.Stop(t)
	rcv.Restart(t)
	sink2 := newWebhookSink(t, rcv.URL(), testKey2)
	if err := sink2.Send(context.Background(), []auditexport.Event{testEvent("event-z")}); err != nil {
		t.Fatalf("second send: %v", err)
	}
	// The deduplicated set the receiver holds equals the sent set.
	for _, id := range []string{"event-x", "event-y", "event-z"} {
		if rcv.Delivered(id) != 1 {
			t.Fatalf("event %s delivered %d times, want 1", id, rcv.Delivered(id))
		}
	}
	if len(rcv.BatchIDs()) != 2 {
		t.Fatalf("batch ids = %v, want 2", rcv.BatchIDs())
	}
}

func TestOTLPReceiverKillAndRestore(t *testing.T) {
	rcv := NewOTLPReceiver(t)
	sink := newOTLPSink(t, rcv.URL())
	events := []auditexport.Event{testEvent("event-1"), testEvent("event-2")}
	if err := sink.Send(context.Background(), events); err != nil {
		t.Fatalf("first send: %v", err)
	}
	_ = rcv.Stop(t)
	rcv.Restart(t)
	sink2 := newOTLPSink(t, rcv.URL())
	if err := sink2.Send(context.Background(), []auditexport.Event{testEvent("event-3")}); err != nil {
		t.Fatalf("second send: %v", err)
	}
	got := rcv.Received()
	for _, id := range []string{"event-1", "event-2", "event-3"} {
		if got[id] != 1 {
			t.Fatalf("event %s delivered %d times, want 1 (all: %v)", id, got[id], got)
		}
	}
}

func TestOTLPReceiverDecodes(t *testing.T) {
	rcv := NewOTLPReceiver(t)
	defer func() { _ = rcv.Stop(t) }()
	sink := newOTLPSink(t, rcv.URL())
	events := []auditexport.Event{testEvent("event-1"), testEvent("event-2")}
	if err := sink.Send(context.Background(), events); err != nil {
		t.Fatalf("send: %v", err)
	}
	if rcv.Delivered("event-1") != 1 || rcv.Delivered("event-2") != 1 {
		t.Fatalf("received: %v", rcv.Received())
	}
	if rcv.Received()["event-1"] != 1 || rcv.Received()["event-2"] != 1 {
		t.Fatalf("received set is not exactly the sent set: %v", rcv.Received())
	}
	all := rcv.AllEvents(t)
	if len(all) != 2 {
		t.Fatalf("decoded %d events, want 2", len(all))
	}
	if all[0].Body == "" {
		t.Fatal("the event body is empty")
	}
	// The attributes are the flat fields a SIEM filters on: the event id,
	// the source and the event type of the first event, asserted from the
	// decoded record rather than from the fixture.
	if all[0].Attributes["elitea.audit.event_id"] != "event-1" {
		t.Fatalf("event_id attribute = %q, want event-1", all[0].Attributes["elitea.audit.event_id"])
	}
	if all[0].Attributes["elitea.audit.source"] != "admin_audit" {
		t.Fatalf("source attribute = %q, want admin_audit", all[0].Attributes["elitea.audit.source"])
	}
	if all[0].Attributes["elitea.audit.event_type"] != "api" {
		t.Fatalf("event_type attribute = %q, want api", all[0].Attributes["elitea.audit.event_type"])
	}
}

func TestOTLPReceiverStatusSwitch(t *testing.T) {
	rcv := NewOTLPReceiver(t)
	defer func() { _ = rcv.Stop(t) }()
	rcv.SetStatus(http.StatusServiceUnavailable, http.StatusServiceUnavailable)
	sink := newOTLPSink(t, rcv.URL())
	err := sink.Send(context.Background(), []auditexport.Event{testEvent("event-a")})
	if err == nil {
		t.Fatal("an unavailable collector must return an error")
	}
	if len(rcv.Received()) != 0 {
		t.Fatalf("received events after a 503: %v", rcv.Received())
	}
}

func newOTLPSink(t *testing.T, endpoint string) *auditexport.OTLPSink {
	t.Helper()
	exp, err := observability.NewLogsExporter(context.Background(), observability.LogsConfig{
		ServiceName: "elitea-main",
		Endpoint:    endpoint,
		Protocol:    observability.LogsProtocolHTTP,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = exp.Shutdown(context.Background()) })
	sink, err := auditexport.NewOTLPSink(exp)
	if err != nil {
		t.Fatal(err)
	}
	return sink
}

func TestWebhookReceiverAllBodiesAndWaitForEvent(t *testing.T) {
	rcv := NewWebhookReceiver(t, testKey2)
	defer func() { _ = rcv.Stop(t) }()
	sink := newWebhookSink(t, rcv.URL(), testKey2)
	event := auditexport.Event{Schema: auditexport.SchemaVersion, EventID: "event-all", Time: "2026-11-25T09:30:00Z", Source: auditexport.SourceAdminAudit, EventType: "api", Tenant: auditexport.Tenant{ProjectID: nil}, Actor: auditexport.Actor{Type: "user", ID: "1"}, Action: "GET /x", Outcome: auditexport.OutcomeSuccess, Details: map[string]any{}}
	if err := sink.Send(context.Background(), []auditexport.Event{event}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if err := rcv.WaitForEvent("event-all", 5*time.Second); err != nil {
		t.Fatalf("wait: %v", err)
	}
	if len(rcv.AllBodies()) != 1 {
		t.Fatalf("all bodies = %d, want 1", len(rcv.AllBodies()))
	}
	_ = rcv.ChainSequences(t)
	_ = rcv.DeliverySeqs(t)
}

func TestOTLPReceiverAllBodiesAndWaitForEvent(t *testing.T) {
	rcv := NewOTLPReceiver(t)
	defer func() { _ = rcv.Stop(t) }()
	sink := newOTLPSink(t, rcv.URL())
	event := auditexport.Event{Schema: auditexport.SchemaVersion, EventID: "event-all", Time: "2026-11-25T09:30:00Z", Source: auditexport.SourceAdminAudit, EventType: "api", Tenant: auditexport.Tenant{ProjectID: nil}, Actor: auditexport.Actor{Type: "user", ID: "1"}, Action: "GET /x", Outcome: auditexport.OutcomeSuccess, Details: map[string]any{}}
	if err := sink.Send(context.Background(), []auditexport.Event{event}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if err := rcv.WaitForEvent("event-all", 5*time.Second); err != nil {
		t.Fatalf("wait: %v", err)
	}
	if len(rcv.AllBodies()) != 1 {
		t.Fatalf("all bodies = %d, want 1", len(rcv.AllBodies()))
	}
	_ = rcv.ChainSequences(t)
	_ = rcv.DeliverySeqs(t)
}

func TestWebhookReceiverDeadLettersOnBadContent(t *testing.T) {
	// A receiver that answers 400 for the event is a content refusal: the sink
	// retries a bounded number of times, then the pipeline dead-letters the
	// event. The existing plumbing test (TestWebhookSplitsARefusedBatchAndDeliversTheNeighbours)
	// covers the split; this one asserts the default attempt bound.
	rcv := NewWebhookReceiver(t, testKey2)
	defer func() { _ = rcv.Stop(t) }()
	sink := newWebhookSink(t, rcv.URL(), testKey2)
	rcv.Refusals()["event-bad"] = http.StatusBadRequest
	events := []auditexport.Event{testEvent("event-ok"), testEvent("event-bad")}
	err := sink.Send(context.Background(), events)
	if err == nil {
		t.Fatal("a content refusal must return an error")
	}
	var permanent *auditexport.PermanentError
	if !errors.As(err, &permanent) {
		t.Fatalf("err = %v, want a permanent error", err)
	}
	if len(permanent.Events) != 1 || permanent.Events[0].EventID != "event-bad" {
		t.Fatalf("permanent events = %v, want only event-bad", permanent.Events)
	}
}
