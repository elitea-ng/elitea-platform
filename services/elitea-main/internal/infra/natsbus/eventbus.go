// Package natsbus is elitea-main's live-update bus: NATS core pub/sub.
//
// It carries the project event stream — canvas presence rosters and the LLM
// gateway's budget.soft_alert — to the project SSE route
// (internal/api/v2/events), on every replica. elitea-main's domain events
// (conversation create, artifact upload, pipeline runs, …) are deliberately
// NOT published here: they go to webhook sinks only (cmd/elitea-main,
// newDomainEventsPublisher), and the SSE route forwards one type per subject
// family. It replaced the plain Redis (Valkey) pub/sub client that used to sit
// at REDIS_URL.
//
// # Subject scheme: one family per producer
//
// Callers name a LOGICAL channel and subjectFor maps it to a subject by
// replacing ':' with '.' under the family's root:
//
//	project:123:presence → elitea.events.project.123.presence  (PresenceSubjectRoot)
//	project:123:events   → gateway.events.project.123.events   (SubjectRoot)
//
// The families are separate on purpose (#1076). elitea.events.> is
// elitea-main's own subject space in its own NATS account (MAIN), and only
// elitea-main publishes canvas presence there. gateway.events.> is the LLM
// gateway's, in the GATEWAY account; its per-project soft-alert subject is
// exported to MAIN and imported here, and elitea-main's identity may not
// publish on it. So neither producer can put a frame into the other's family,
// and the SSE route accepts each event type from its own family only.
//
// A channel ending in ":presence" maps under PresenceSubjectRoot; every other
// channel under SubjectRoot. A Redis-style "prefix:*" catch-all maps to the
// NATS multi-token wildcard ("gateway.events.prefix.>").
//
// Project ids are positive integers by the time a channel is built
// (legacyrbac refuses anything else before the SSE handler runs), so no
// caller-controlled '.', '*' or '>' reaches a subject.
//
// # Delivery
//
// Core NATS, not JetStream: at-most-once and live only, which is exactly the
// contract of an SSE stream with no replay. A subscriber that is not connected
// when an event is published does not get it, as with Redis PUBLISH.
//
// The envelope on the wire is Event (type, source, payload, timestamp) — the
// shape the Redis bus used — so a consumer decodes it unchanged.
package natsbus

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
)

// Event is the envelope every message on the bus carries.
type Event struct {
	Type      string          `json:"type"`
	Source    string          `json:"source"`
	Payload   json.RawMessage `json:"payload"`
	Timestamp time.Time       `json:"timestamp"`
}

// EventHandler consumes one decoded Event (Subscribe).
type EventHandler func(ctx context.Context, event Event) error

// SubjectRoot is the LLM gateway's event family (design/ADR: gateway.events.*),
// in the gateway's NATS account; elitea-main reads the per-project soft-alert
// subject it imports. Every channel not in another family maps under it.
const SubjectRoot = "gateway.events"

// PresenceSubjectRoot is elitea-main's own event family, in its own NATS
// account (#1076): canvas presence rosters, project:<id>:presence →
// elitea.events.project.<id>.presence.
const PresenceSubjectRoot = "elitea.events"

// presenceSuffix marks a channel of the presence family.
const presenceSuffix = ":presence"

// ConnectTimeout bounds the initial dial and every request the client makes so
// a NATS partition fails fast rather than hanging (design §8.5, matches the
// gateway NATS client's 1s Timeout).
const ConnectTimeout = 1 * time.Second

// natsConn is the minimal surface of *nats.Conn the bus uses. It lets tests
// substitute a fake without a live server (same narrow-interface pattern as the
// gateway's internal/infra/nats client).
type natsConn interface {
	Publish(subj string, data []byte) error
	ChanSubscribe(subj string, ch chan *nats.Msg) (subscription, error)
	FlushTimeout(timeout time.Duration) error
	RTT() (time.Duration, error)
	Drain() error
	IsClosed() bool
	Close()
}

// subscription is the minimal surface of *nats.Subscription the bus needs
// (drain on teardown). Abstracting it makes subscription cleanup observable in
// tests without a live server.
type subscription interface {
	Drain() error
}

// realConn adapts a *nats.Conn to natsConn. Publish/FlushTimeout/RTT/Drain/
// IsClosed/Close are promoted from the embedded connection unchanged; only ChanSubscribe is
// overridden to return the narrower subscription interface (the concrete
// *nats.Subscription already satisfies it).
type realConn struct{ *nats.Conn }

func (c realConn) ChanSubscribe(subj string, ch chan *nats.Msg) (subscription, error) {
	return c.Conn.ChanSubscribe(subj, ch)
}

// EventBus publishes and subscribes to platform events over NATS core pub/sub.
type EventBus struct {
	conn   natsConn
	source string
	// flush makes Publish wait for the server to acknowledge the write
	// (FlushTimeout). On by default; see WithBufferedPublish.
	flush bool
}

// Option configures an EventBus.
type Option func(*EventBus)

// WithBufferedPublish makes Publish return as soon as the message is in the
// client's outbound buffer, without a FlushTimeout round trip.
//
// elitea-main's composition root uses it, because Publish runs on request
// paths (conversation create, artifact upload, the canvas heartbeat) and the
// stream it feeds is best-effort live UI. With a flush per publish every one
// of those requests pays a NATS round trip, and while NATS is reconnecting
// each of them waits the full ConnectTimeout before failing. Buffered, a
// publish during a reconnect is held in nats.go's reconnect buffer and sent
// when the connection returns; Publish still errors once the connection is
// closed for good or that buffer is full.
func WithBufferedPublish() Option {
	return func(eb *EventBus) { eb.flush = false }
}

// Connect dials NATS and returns an EventBus. url is the NATS server URL
// (nats://host:4222); name identifies the client in NATS monitoring; source is
// stamped into every published Event. A dial failure is returned to the
// caller. After the first successful dial the client reconnects forever.
func Connect(url, name, source string, opts ...Option) (*EventBus, error) {
	nc, err := nats.Connect(url,
		nats.Name(name),
		nats.Timeout(ConnectTimeout),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(500*time.Millisecond),
	)
	if err != nil {
		return nil, fmt.Errorf("natsbus: connect: %w", err)
	}
	return New(realConn{Conn: nc}, source, opts...), nil
}

// NewFromConn wraps a connection the caller dialled itself — the composition
// root does, because the same connection also backs JetStream KV.
func NewFromConn(nc *nats.Conn, source string, opts ...Option) *EventBus {
	return New(realConn{Conn: nc}, source, opts...)
}

// New wraps an already-connected NATS connection. Exposed for wiring and tests.
func New(conn natsConn, source string, opts ...Option) *EventBus {
	eb := &EventBus{conn: conn, source: source, flush: true}
	for _, opt := range opts {
		opt(eb)
	}
	return eb
}

// subjectFor maps a logical channel name to a NATS subject under SubjectRoot.
// ':' → '.'; a trailing ':*' (Redis catch-all) becomes the NATS multi-token
// wildcard '>' so an "elitea:*" subscription still receives every event.
func subjectFor(channel string) string {
	if channel == "" {
		return SubjectRoot
	}
	if strings.HasSuffix(channel, presenceSuffix) && !strings.Contains(channel, "*") {
		return PresenceSubjectRoot + "." + strings.ReplaceAll(channel, ":", ".")
	}
	// Bare "*" Redis catch-all matches every channel → NATS "root.>".
	if channel == "*" {
		return SubjectRoot + ".>"
	}
	// Redis "prefix:*" catch-all → NATS "root.prefix.>" multi-token wildcard.
	if strings.HasSuffix(channel, ":*") {
		prefix := strings.TrimSuffix(channel, ":*")
		prefix = strings.ReplaceAll(prefix, ":", ".")
		if prefix == "" {
			return SubjectRoot + ".>"
		}
		return SubjectRoot + "." + prefix + ".>"
	}
	return SubjectRoot + "." + strings.ReplaceAll(channel, ":", ".")
}

// Publish marshals payload into an Event envelope and publishes it to the
// NATS subject derived from channel. Unless WithBufferedPublish was given it
// flushes, so a transport error surfaces synchronously (bounded by
// ConnectTimeout) rather than being silently buffered.
func (eb *EventBus) Publish(_ context.Context, channel string, eventType string, payload interface{}) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("natsbus: marshal payload: %w", err)
	}

	evt := Event{
		Type:      eventType,
		Source:    eb.source,
		Payload:   data,
		Timestamp: time.Now().UTC(),
	}

	msg, err := json.Marshal(evt)
	if err != nil {
		return fmt.Errorf("natsbus: marshal event: %w", err)
	}

	if err := eb.conn.Publish(subjectFor(channel), msg); err != nil {
		return fmt.Errorf("natsbus: publish: %w", err)
	}
	if !eb.flush {
		return nil
	}
	if err := eb.conn.FlushTimeout(ConnectTimeout); err != nil {
		return fmt.Errorf("natsbus: flush: %w", err)
	}
	return nil
}

// Subscribe asynchronously consumes events on the subject derived from channel,
// decoding each into an Event and invoking handler. The goroutine exits (and
// drains the subscription) when ctx is cancelled. A malformed message is
// logged and skipped; a handler error is logged but does not stop the loop.
func (eb *EventBus) Subscribe(ctx context.Context, channel string, handler EventHandler) {
	subject := subjectFor(channel)
	msgCh := make(chan *nats.Msg, 64)
	sub, err := eb.conn.ChanSubscribe(subject, msgCh)
	if err != nil {
		slog.Error("natsbus: subscribe failed", "err", err, "subject", subject)
		return
	}

	go func() {
		defer func() { _ = sub.Drain() }()
		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-msgCh:
				if !ok {
					return
				}
				var evt Event
				if err := json.Unmarshal(msg.Data, &evt); err != nil {
					slog.Error("natsbus: unmarshal event", "err", err, "subject", subject)
					continue
				}
				if err := handler(ctx, evt); err != nil {
					slog.Error("natsbus: handler error", "err", err, "subject", subject, "type", evt.Type)
				}
			}
		}
	}()
}

// Raw returns a receive-only channel of decoded events for a channel, plus a
// cancel func that drains the underlying subscription. The project SSE handler
// (internal/api/v2/events) uses this so it can multiplex events with its own
// heartbeat ticker instead of supplying a callback. The channel closes when the
// caller invokes cancel or ctx is cancelled.
func (eb *EventBus) Raw(ctx context.Context, channel string) (<-chan Event, func(), error) {
	subject := subjectFor(channel)
	msgCh := make(chan *nats.Msg, 64)
	sub, err := eb.conn.ChanSubscribe(subject, msgCh)
	if err != nil {
		return nil, nil, fmt.Errorf("natsbus: subscribe: %w", err)
	}

	out := make(chan Event, 64)
	done := make(chan struct{})
	cancel := func() {
		select {
		case <-done:
		default:
			close(done)
		}
	}

	go func() {
		defer close(out)
		defer func() { _ = sub.Drain() }()
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case msg, ok := <-msgCh:
				if !ok {
					return
				}
				var evt Event
				if err := json.Unmarshal(msg.Data, &evt); err != nil {
					slog.Error("natsbus: unmarshal event", "err", err, "subject", subject)
					continue
				}
				select {
				case out <- evt:
				case <-ctx.Done():
					return
				case <-done:
					return
				}
			}
		}
	}()

	return out, cancel, nil
}

// Ping verifies connectivity via the connection round-trip time. It satisfies
// the health.Checker interface (internal/api/health). elitea-main does NOT put
// it on /readyz: the stream is best-effort live UI, and a NATS blip must not
// take every API replica out of the load balancer.
func (eb *EventBus) Ping(_ context.Context) error {
	if _, err := eb.conn.RTT(); err != nil {
		return fmt.Errorf("natsbus: ping: %w", err)
	}
	return nil
}

// CloseTimeout bounds shutdown: half for the flush of buffered publishes, the
// rest for the drain to finish. Past it the connection is closed regardless,
// so a dead server cannot hold a terminating pod.
const CloseTimeout = 4 * time.Second

// closePollInterval is how often Close checks that the drain has finished.
const closePollInterval = 10 * time.Millisecond

// Close flushes, drains and closes the connection, each step bounded by
// CloseTimeout, so publishes still in the client's buffer — every publish is
// buffered under WithBufferedPublish — reach the server before exit.
//
// Drain alone does not guarantee that: in nats.go it is ASYNC (it starts a
// goroutine and returns), and while the client is reconnecting it calls
// Close() at once, discarding the reconnect buffer. So:
//
//  1. FlushTimeout first. A PING/PONG round trip proves everything written
//     before it is on the server; during a reconnect it waits (bounded) for
//     the connection to come back, which is when the reconnect buffer is sent.
//  2. Drain, then wait (bounded) for the connection to report closed, which
//     is when the drain has delivered pending subscription messages and
//     flushed again. Polling IsClosed rather than installing a closed
//     callback keeps any callback the dialler set.
//  3. Anything that fails or overruns falls back to Close().
func (eb *EventBus) Close() {
	eb.closeWithin(CloseTimeout)
}

func (eb *EventBus) closeWithin(timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	if eb.conn.IsClosed() {
		return
	}
	if err := eb.conn.FlushTimeout(timeout / 2); err != nil {
		slog.Warn("natsbus: shutdown flush did not complete; buffered publishes may be lost", "err", err)
	}
	if err := eb.conn.Drain(); err != nil {
		eb.conn.Close()
		return
	}
	for !eb.conn.IsClosed() {
		if !time.Now().Before(deadline) {
			slog.Warn("natsbus: drain did not finish before the shutdown deadline; closing", "timeout", timeout)
			eb.conn.Close()
			return
		}
		time.Sleep(closePollInterval)
	}
}
