// Package exporttest is the shared test-support package for the signal and
// audit export stories of ADR-0058: in-process OTLP/HTTP logs and signed
// webhook receivers that record what a sink sends, so the export, the
// no-content proof and the kill-and-restore tests can assert without a real
// SIEM.
//
// The receivers model an external SIEM: they are in-process, in the same
// address space as the test, and they speak the same OTLP/HTTP and
// signed-webhook protocols the sinks use. They are not a real collector or a
// real receiver; a deployment that needs mTLS and a real endpoint uses the
// real sinks.
package exporttest

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	collogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	"google.golang.org/protobuf/proto"
)

// Event is one recorded log record or webhook event.
type Event struct {
	// EventID is the event id of the envelope.
	EventID string
	// Body is the raw envelope body of an OTLP record or of one webhook event,
	// as a string.
	Body string
	// Attributes are the flat attributes of an OTLP record.
	Attributes map[string]string
	// BatchID is the batch id of a webhook delivery.
	BatchID string
	// Headers are the webhook delivery headers.
	Headers http.Header
}

// WebhookBatch is one webhook delivery, recorded.
type WebhookBatch struct {
	BatchID string
	Body    []byte
	Events  []Event
	Headers http.Header
}

// server is the shared listener, status switch, down switch and body record.
// Its serveHTTP is overridden by each receiver type through ServeHTTP.
type server struct {
	listener net.Listener
	server   *http.Server

	mu       sync.Mutex
	down     bool
	status   []int
	got      map[string]int
	bodies   [][]byte
	batches  []WebhookBatch
	now      func() time.Time
	requests int
}

// start opens a loopback listener on an OS-chosen port and serves h until
// Stop is called.
func start(t *testing.T, h http.Handler) *server {
	t.Helper()
	s := &server{got: map[string]int{}, now: time.Now}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.listener = ln
	s.server = &http.Server{Handler: h}
	go func() { _ = s.server.Serve(ln) }()
	t.Cleanup(func() { _ = s.Stop(t) })
	return s
}

// ServeHTTP is the shared request lifecycle: the down switch, the status
// switch and the body read. It calls decode, which each receiver implements.
func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Read the body before taking the lock: a slow client body must not block
	// Delivered, Received and URL for every other caller.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.down {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	if len(s.status) > 0 {
		code := s.status[0]
		s.status = s.status[1:]
		if code != http.StatusOK {
			w.WriteHeader(code)
			return
		}
	}
	s.bodies = append(s.bodies, body)
	s.decode(w, r, body)
}

// decode is the part of ServeHTTP each receiver implements.
func (s *server) decode(w http.ResponseWriter, r *http.Request, body []byte) {
	w.WriteHeader(http.StatusOK)
}

// Stop closes the listener; the received set is kept.
func (s *server) Stop(t *testing.T) error {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.down {
		return nil
	}
	s.down = true
	if s.server != nil {
		_ = s.server.Close()
	}
	if s.listener != nil {
		return s.listener.Close()
	}
	return nil
}

// Restart brings the receiver back on a fresh loopback listener; the received
// set is kept. The URL changes, so the caller reads the new URL.
func (s *server) Restart(t *testing.T, h http.Handler) {
	t.Helper()
	s.mu.Lock()
	s.down = false
	s.mu.Unlock()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.listener = ln
	s.server = &http.Server{Handler: h}
	go func() { _ = s.server.Serve(ln) }()
}

// URL is the receiver's base URL.
func (s *server) URL() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener == nil {
		return ""
	}
	return "http://" + s.listener.Addr().String()
}

// SetStatus scripts answers for the next N requests: the given codes first,
// then 200.
func (s *server) SetStatus(codes ...int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = append([]int(nil), codes...)
}

// Received returns the distinct event ids received, mapped to the number of
// times each was received.
func (s *server) Received() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]int{}
	for id, n := range s.got {
		out[id] = n
	}
	return out
}

// Delivered returns the number of times one event id was received.
func (s *server) Delivered(eventID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.got[eventID]
}

// AllBodies returns the raw bodies the receiver read, in order.
func (s *server) AllBodies() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][]byte, len(s.bodies))
	copy(out, s.bodies)
	return out
}

// WaitForEvent waits until the event id is recorded, or the timeout elapses.
func (s *server) WaitForEvent(eventID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	tick := time.NewTicker(2 * time.Millisecond)
	defer tick.Stop()
	for time.Now().Before(deadline) {
		<-tick.C
		if s.Delivered(eventID) > 0 {
			return nil
		}
	}
	return fmt.Errorf("event %s was not received within %s", eventID, timeout)
}

// AssertNoEvent fails if the receiver has recorded any event in the window.
func (s *server) AssertNoEvent(t *testing.T, window time.Duration) {
	t.Helper()
	time.Sleep(window)
	if got := s.Received(); len(got) != 0 {
		t.Errorf("expected no events in the window, got %v", got)
	}
}

// chainSequences parses the decoded envelopes and returns the chain.sequence
// values of the events that carry a chain, in the order they were recorded.
// A body that does not parse is a test failure, not a silent skip.
func chainSequences(t *testing.T, bodies []string) []int64 {
	t.Helper()
	var out []int64
	for _, body := range bodies {
		var envelope struct {
			Chain *struct {
				Sequence int64 `json:"sequence"`
			} `json:"chain"`
		}
		if err := json.Unmarshal([]byte(body), &envelope); err != nil {
			t.Fatalf("decode event body %q: %v", body, err)
		}
		if envelope.Chain != nil {
			out = append(out, envelope.Chain.Sequence)
		}
	}
	return out
}

// deliverySeqs parses the decoded envelopes and returns the delivery_seq values
// of the events that carry one, in the order they were recorded. A body that
// does not parse is a test failure, not a silent skip.
func deliverySeqs(t *testing.T, bodies []string) []int64 {
	t.Helper()
	var out []int64
	for _, body := range bodies {
		var envelope struct {
			DeliverySeq *int64 `json:"delivery_seq"`
		}
		if err := json.Unmarshal([]byte(body), &envelope); err != nil {
			t.Fatalf("decode event body %q: %v", body, err)
		}
		if envelope.DeliverySeq != nil {
			out = append(out, *envelope.DeliverySeq)
		}
	}
	return out
}

// SigningKey is one webhook HMAC key. Kept local so the package is import-free
// of the audit receiver it supports (that import would cycle).
type SigningKey struct {
	ID      string
	Secret  []byte
	Created time.Time
}

// HeaderSignature is the webhook signature header.
const HeaderSignature = "X-Elitea-Signature"

// HeaderKeyID names the signing key.
const HeaderKeyID = "X-Elitea-Key-Id"

// HeaderDelivery is the batch id.
const HeaderDelivery = "X-Elitea-Delivery"

// HeaderSchema is the envelope version.
const HeaderSchema = "X-Elitea-Schema"

// Sign computes the v1 signature for a body at time t.
func Sign(secret []byte, t time.Time, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(strconv.FormatInt(t.Unix(), 10)))
	_, _ = mac.Write([]byte{'.'})
	_, _ = mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// SignatureHeader renders the HeaderSignature value.
func SignatureHeader(secret []byte, t time.Time, body []byte) string {
	return "t=" + strconv.FormatInt(t.Unix(), 10) + ",v1=" + Sign(secret, t, body)
}

// WebhookReceiver is a signed-webhook receiver.
type WebhookReceiver struct {
	s    *server
	keys map[string][]byte
	now  func() time.Time
	// refuse answers this code to any batch holding the event id.
	refuse map[string]int
}

// NewWebhookReceiver starts a signed-webhook receiver on an OS-chosen
// loopback port, accepting bodies signed with any of the given keys. The
// receiver is always running until Stop is called; Restart rebinds.
func NewWebhookReceiver(t *testing.T, keys ...SigningKey) *WebhookReceiver {
	t.Helper()
	w := &WebhookReceiver{keys: map[string][]byte{}, now: time.Now, refuse: map[string]int{}}
	for _, k := range keys {
		w.keys[k.ID] = k.Secret
	}
	w.s = start(t, http.HandlerFunc(w.ServeHTTP))
	return w
}

// Refusals is the per-event refusal map the caller fills: event id -> code.
// A batch holding the event answers the code.
func (w *WebhookReceiver) Refusals() map[string]int { return w.refuse }

// ServeHTTP verifies the signature before decoding the body and records the
// events of each delivery.
func (w *WebhookReceiver) ServeHTTP(resp http.ResponseWriter, r *http.Request) {
	// Read the body before taking the lock: a slow client body must not block
	// Delivered, Received and URL for every other caller.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		resp.WriteHeader(http.StatusBadRequest)
		return
	}
	w.s.mu.Lock()
	defer w.s.mu.Unlock()
	w.s.requests++
	if w.s.down {
		resp.WriteHeader(http.StatusBadGateway)
		return
	}
	if len(w.s.status) > 0 {
		code := w.s.status[0]
		w.s.status = w.s.status[1:]
		if code != http.StatusOK {
			resp.WriteHeader(code)
			return
		}
	}
	// Verify the signature before decoding the body.
	secret, ok := w.keys[r.Header.Get(HeaderKeyID)]
	if !ok {
		resp.WriteHeader(http.StatusUnauthorized)
		return
	}
	parts := strings.Split(r.Header.Get(HeaderSignature), ",")
	if len(parts) != 2 || !strings.HasPrefix(parts[0], "t=") || !strings.HasPrefix(parts[1], "v1=") {
		resp.WriteHeader(http.StatusUnauthorized)
		return
	}
	ts, err := strconv.ParseInt(strings.TrimPrefix(parts[0], "t="), 10, 64)
	if err != nil || w.now().Sub(time.Unix(ts, 0)).Abs() > 5*time.Minute {
		resp.WriteHeader(http.StatusUnauthorized)
		return
	}
	want := Sign(secret, time.Unix(ts, 0), body)
	gotSig, _ := hex.DecodeString(strings.TrimPrefix(parts[1], "v1="))
	wantSig, _ := hex.DecodeString(want)
	if !hmac.Equal(gotSig, wantSig) {
		resp.WriteHeader(http.StatusUnauthorized)
		return
	}
	var batch webhookBody
	if err := json.Unmarshal(body, &batch); err != nil {
		resp.WriteHeader(http.StatusBadRequest)
		return
	}
	// The refusal check answers the code to any batch holding the event.
	type event struct {
		EventID string `json:"event_id"`
	}
	events := make([]struct {
		EventID string
		Raw     []byte
	}, 0, len(batch.Events))
	for _, raw := range batch.Events {
		var e event
		if err := json.Unmarshal(raw, &e); err != nil {
			resp.WriteHeader(http.StatusBadRequest)
			return
		}
		events = append(events, struct {
			EventID string
			Raw     []byte
		}{e.EventID, raw})
	}
	for _, e := range events {
		if code, ok := w.refuse[e.EventID]; ok {
			resp.WriteHeader(code)
			return
		}
	}
	w.s.bodies = append(w.s.bodies, body)
	b := WebhookBatch{BatchID: batch.BatchID, Body: body, Headers: r.Header.Clone()}
	for _, e := range events {
		w.s.got[e.EventID]++
		b.Events = append(b.Events, Event{EventID: e.EventID, Body: string(e.Raw), BatchID: batch.BatchID, Headers: r.Header.Clone()})
	}
	w.s.batches = append(w.s.batches, b)
	resp.WriteHeader(http.StatusOK)
}

// URL is the receiver's base URL.
func (w *WebhookReceiver) URL() string { return w.s.URL() }

// Received returns the distinct event ids received, mapped to the number of
// times each was received.
func (w *WebhookReceiver) Received() map[string]int { return w.s.Received() }

// Delivered returns the number of times one event id was received.
func (w *WebhookReceiver) Delivered(eventID string) int { return w.s.Delivered(eventID) }

// AllBodies returns the raw bodies the receiver read, in order.
func (w *WebhookReceiver) AllBodies() [][]byte { return w.s.AllBodies() }

// Requests returns the number of requests the receiver has seen.
func (w *WebhookReceiver) Requests() int {
	w.s.mu.Lock()
	defer w.s.mu.Unlock()
	return w.s.requests
}

// KeyIDs returns the key ids seen on successful deliveries.
func (w *WebhookReceiver) KeyIDs() []string {
	w.s.mu.Lock()
	defer w.s.mu.Unlock()
	out := []string{}
	for i := range w.s.batches {
		out = append(out, w.s.batches[i].Headers.Get(HeaderKeyID))
	}
	return out
}

// BatchIDs returns the batch ids received, in order.
func (w *WebhookReceiver) BatchIDs() []string {
	w.s.mu.Lock()
	defer w.s.mu.Unlock()
	out := make([]string, 0, len(w.s.batches))
	for _, b := range w.s.batches {
		out = append(out, b.BatchID)
	}
	return out
}

// Headers returns the request headers of every delivery.
func (w *WebhookReceiver) Headers() []http.Header {
	w.s.mu.Lock()
	defer w.s.mu.Unlock()
	out := make([]http.Header, 0, len(w.s.batches))
	for _, b := range w.s.batches {
		out = append(out, b.Headers)
	}
	return out
}

// Stop closes the listener; the received set is kept.
func (w *WebhookReceiver) Stop(t *testing.T) error { return w.s.Stop(t) }

// Restart brings the receiver back on a fresh loopback listener; the received
// set is kept. The URL changes.
func (w *WebhookReceiver) Restart(t *testing.T) { w.s.Restart(t, http.HandlerFunc(w.ServeHTTP)) }

// SetStatus scripts answers for the next N requests.
func (w *WebhookReceiver) SetStatus(codes ...int) { w.s.SetStatus(codes...) }

// WaitForEvent waits until the event id is recorded, or the timeout elapses.
func (w *WebhookReceiver) WaitForEvent(eventID string, timeout time.Duration) error {
	return w.s.WaitForEvent(eventID, timeout)
}

// AssertNoEvent fails if the receiver has recorded any event in the window.
func (w *WebhookReceiver) AssertNoEvent(t *testing.T, window time.Duration) {
	w.s.AssertNoEvent(t, window)
}

// ChainSequences returns the chain.sequence values of the events the receiver
// recorded, in the order it recorded them.
func (w *WebhookReceiver) ChainSequences(t *testing.T) []int64 {
	t.Helper()
	w.s.mu.Lock()
	defer w.s.mu.Unlock()
	bodies := make([]string, 0, len(w.s.batches))
	for _, b := range w.s.batches {
		for _, e := range b.Events {
			bodies = append(bodies, e.Body)
		}
	}
	return chainSequences(t, bodies)
}

// DeliverySeqs returns the delivery_seq values of the events the receiver
// recorded, in the order it recorded them.
func (w *WebhookReceiver) DeliverySeqs(t *testing.T) []int64 {
	t.Helper()
	w.s.mu.Lock()
	defer w.s.mu.Unlock()
	bodies := make([]string, 0, len(w.s.batches))
	for _, b := range w.s.batches {
		for _, e := range b.Events {
			bodies = append(bodies, e.Body)
		}
	}
	return deliverySeqs(t, bodies)
}

type webhookBody struct {
	Schema  string              `json:"schema"`
	BatchID string              `json:"batch_id"`
	Events  []json.RawMessage   `json:"events"`
}

// OTLPReceiver is an OTLP/HTTP logs receiver.
type OTLPReceiver struct {
	s *server
}

// NewOTLPReceiver starts a receiver on an OS-chosen loopback port. It decodes
// ExportLogsServiceRequest bodies and records each log record's body and
// attributes.
func NewOTLPReceiver(t *testing.T) *OTLPReceiver {
	t.Helper()
	o := &OTLPReceiver{}
	o.s = start(t, http.HandlerFunc(o.ServeHTTP))
	return o
}

// ServeHTTP decodes the OTLP request and records each log record.
func (o *OTLPReceiver) ServeHTTP(resp http.ResponseWriter, r *http.Request) {
	// Read the body before taking the lock: a slow client body must not block
	// Delivered, Received and URL for every other caller.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		resp.WriteHeader(http.StatusBadRequest)
		return
	}
	o.s.mu.Lock()
	defer o.s.mu.Unlock()
	o.s.requests++
	if o.s.down {
		resp.WriteHeader(http.StatusBadGateway)
		return
	}
	if len(o.s.status) > 0 {
		code := o.s.status[0]
		o.s.status = o.s.status[1:]
		if code != http.StatusOK {
			resp.WriteHeader(code)
			return
		}
	}
	req := &collogs.ExportLogsServiceRequest{}
	if err := proto.Unmarshal(body, req); err != nil {
		resp.WriteHeader(http.StatusBadRequest)
		return
	}
	o.s.bodies = append(o.s.bodies, body)
	for _, rl := range req.GetResourceLogs() {
		for _, sl := range rl.GetScopeLogs() {
			for _, rec := range sl.GetLogRecords() {
				attrs := map[string]string{}
				for _, attr := range rec.GetAttributes() {
					attrs[attr.GetKey()] = attr.GetValue().GetStringValue()
				}
				eventID := attrs["elitea.audit.event_id"]
				if eventID == "" {
					eventID = "unknown"
				}
				o.s.got[eventID]++
			}
		}
	}
	resp.WriteHeader(http.StatusOK)
}

// URL is the receiver's base URL.
func (o *OTLPReceiver) URL() string { return o.s.URL() }

// Received returns the distinct event ids received, mapped to the number of
// times each was received.
func (o *OTLPReceiver) Received() map[string]int { return o.s.Received() }

// Delivered returns the number of times one event id was received.
func (o *OTLPReceiver) Delivered(eventID string) int { return o.s.Delivered(eventID) }

// AllBodies returns the raw bodies the receiver read, in order.
func (o *OTLPReceiver) AllBodies() [][]byte { return o.s.AllBodies() }

// AllEvents returns every log record the receiver decoded, in order.
func (o *OTLPReceiver) AllEvents() []Event {
	o.s.mu.Lock()
	defer o.s.mu.Unlock()
	var out []Event
	for _, body := range o.s.bodies {
		req := &collogs.ExportLogsServiceRequest{}
		if err := proto.Unmarshal(body, req); err != nil {
			continue
		}
		for _, rl := range req.GetResourceLogs() {
			for _, sl := range rl.GetScopeLogs() {
				for _, rec := range sl.GetLogRecords() {
					attrs := map[string]string{}
					eventID := ""
					for _, attr := range rec.GetAttributes() {
						attrs[attr.GetKey()] = attr.GetValue().GetStringValue()
						if attr.GetKey() == "elitea.audit.event_id" {
							eventID = attr.GetValue().GetStringValue()
						}
					}
					out = append(out, Event{EventID: eventID, Body: rec.GetBody().GetStringValue(), Attributes: attrs})
				}
			}
		}
	}
	return out
}

// Stop closes the listener; the received set is kept.
func (o *OTLPReceiver) Stop(t *testing.T) error { return o.s.Stop(t) }

// Restart brings the receiver back on a fresh loopback listener; the received
// set is kept. The URL changes.
func (o *OTLPReceiver) Restart(t *testing.T) { o.s.Restart(t, http.HandlerFunc(o.ServeHTTP)) }

// SetStatus scripts answers for the next N requests.
func (o *OTLPReceiver) SetStatus(codes ...int) { o.s.SetStatus(codes...) }

// WaitForEvent waits until the event id is recorded, or the timeout elapses.
func (o *OTLPReceiver) WaitForEvent(eventID string, timeout time.Duration) error {
	return o.s.WaitForEvent(eventID, timeout)
}

// AssertNoEvent fails if the receiver has recorded any event in the window.
func (o *OTLPReceiver) AssertNoEvent(t *testing.T, window time.Duration) {
	o.s.AssertNoEvent(t, window)
}

// ChainSequences returns the chain.sequence values of the events the receiver
// decoded, in the order it recorded them.
func (o *OTLPReceiver) ChainSequences(t *testing.T) []int64 {
	t.Helper()
	ents := o.AllEvents()
	bodies := make([]string, 0, len(ents))
	for _, e := range ents {
		bodies = append(bodies, e.Body)
	}
	return chainSequences(t, bodies)
}

// DeliverySeqs returns the delivery_seq values of the events the receiver
// decoded, in the order it recorded them.
func (o *OTLPReceiver) DeliverySeqs(t *testing.T) []int64 {
	t.Helper()
	ents := o.AllEvents()
	bodies := make([]string, 0, len(ents))
	for _, e := range ents {
		bodies = append(bodies, e.Body)
	}
	return deliverySeqs(t, bodies)
}
