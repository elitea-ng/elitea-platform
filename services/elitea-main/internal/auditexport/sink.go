package auditexport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// Sink delivers events, in order, to one receiver. Send returns nil only when
// every event was settled: accepted by the receiver, or (in the pipeline Wrap
// builds) dead-lettered.
//
// A sink that settled only the first events of the slice says so with a
// *PartialError, so the caller does not offer those again. Any other error
// settled nothing.
//
// An error is transient unless it wraps a *PermanentError: a transient error
// is retried by the relay forever, and only an outage alert tells an operator;
// a permanent error means the receiver refused the content.
type Sink interface {
	Name() string
	Send(ctx context.Context, events []Event) error
}

// PermanentError marks content the receiver refused (HTTP 400, 413 or 422).
// Retrying the same bytes cannot succeed.
type PermanentError struct {
	Err error
	// Events are the refused events. They start at the first unsettled event
	// of the Send. The webhook splits a refused batch until one event is
	// isolated, so it always names exactly one. Empty means every unsettled
	// event of the Send.
	Events []Event
}

func (e *PermanentError) Error() string { return "permanent rejection: " + e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// PartialError reports that a Send settled the first Settled events of its
// slice, in order, and then failed with Err. The settled ones must not be
// offered to this sink again.
type PartialError struct {
	Settled int
	Err     error
}

func (e *PartialError) Error() string {
	return fmt.Sprintf("%d events settled, then: %v", e.Settled, e.Err)
}
func (e *PartialError) Unwrap() error { return e.Err }

// settledCount is how many leading events of a Send of n events err leaves
// settled.
func settledCount(err error, n int) int {
	if err == nil {
		return n
	}
	var partial *PartialError
	if errors.As(err, &partial) {
		return max(0, min(partial.Settled, n))
	}
	return 0
}

// withSettled wraps err so it reports settled leading events. Zero settled
// returns err as it is.
func withSettled(settled int, err error) error {
	if err == nil || settled <= 0 {
		return err
	}
	var inner *PartialError
	if errors.As(err, &inner) {
		err = inner.Err
	}
	return &PartialError{Settled: settled, Err: err}
}

// batchKey identifies a batch by its event ids, for the webhook's delivery id.
func batchKey(events []Event) string {
	h := sha256.New()
	for _, e := range events {
		_, _ = h.Write([]byte(e.EventID))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// DeadLetterStore records events given up on, so an operator can replay them.
type DeadLetterStore interface {
	RecordDeadLetters(ctx context.Context, sink string, events []Event, reason string, attempts int) error
}

// PipelineConfig is what Wrap adds around a raw sink.
type PipelineConfig struct {
	Metrics *Metrics
	// DeadLetters is where dead letters are recorded. Nil means a permanent
	// rejection is never dead-lettered: it blocks the stream like an outage.
	DeadLetters DeadLetterStore
	// MaxPermanentAttempts is how many passes may refuse the same event
	// permanently before it is dead-lettered. Default 5.
	MaxPermanentAttempts int
	Logger               *slog.Logger
	// MemoSize bounds how many recently settled event refs the pipeline
	// remembers. Default 65536.
	MemoSize int
}

// DefaultMaxPermanentAttempts is PipelineConfig.MaxPermanentAttempts' default.
const DefaultMaxPermanentAttempts = 5

const defaultMemoSize = 1 << 16

// Wrap adds the pipeline every sink runs through:
//
//   - memo: an event this sink already settled is not sent to it again. The
//     security audit relay acknowledges an outbox row only when every sink
//     accepted it, so while one sink is down the relay re-offers the same and
//     a growing set of rows; the memo is per event, not per batch, so the
//     healthy sink receives each one once. The audit_events relay keeps a
//     persisted position per sink and never re-offers in the first place.
//     The memo is in memory: a restart may resend a batch (at least once).
//   - metrics: delivered and failed counters;
//   - dead letter: ONLY an event the receiver refused alone, permanently, on
//     MaxPermanentAttempts passes is dead-lettered, by itself; its neighbours
//     are delivered. A transient failure (receiver down, 5xx, 429, wrong
//     credentials, egress refused) is never dead-lettered: those events wait,
//     the backlog and lag alerts fire, and nothing is lost when the receiver
//     comes back.
func Wrap(sink Sink, cfg PipelineConfig) Sink {
	if cfg.MaxPermanentAttempts <= 0 {
		cfg.MaxPermanentAttempts = DefaultMaxPermanentAttempts
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Metrics == nil {
		cfg.Metrics = NewMetrics()
	}
	if cfg.MemoSize <= 0 {
		cfg.MemoSize = defaultMemoSize
	}
	cfg.Metrics.Register(sink.Name())
	return &pipeline{inner: sink, cfg: cfg, memo: newRefMemo(cfg.MemoSize)}
}

type pipeline struct {
	inner Sink
	cfg   PipelineConfig

	mu   sync.Mutex
	memo *refMemo
	// refusedRef is the event the receiver last refused permanently, and
	// refusedCount the passes that refused it. Events are offered in order,
	// so at most one event is being counted at a time.
	refusedRef   string
	refusedCount int
}

func (p *pipeline) Name() string { return p.inner.Name() }

func (p *pipeline) Send(ctx context.Context, events []Event) error {
	settled := 0 // leading events of events that are settled
	for settled < len(events) {
		// pending are the unsettled events not in the memo; index maps each
		// back to its position in events.
		p.mu.Lock()
		var (
			pending []Event
			index   []int
		)
		for i := settled; i < len(events); i++ {
			if !p.memo.has(events[i].Ref()) {
				pending = append(pending, events[i])
				index = append(index, i)
			}
		}
		p.mu.Unlock()
		if len(pending) == 0 {
			return nil
		}

		err := p.inner.Send(ctx, pending)
		n := settledCount(err, len(pending))
		if n > 0 {
			p.remember(pending[:n])
			p.cfg.Metrics.addDelivered(p.Name(), pending[:n])
		}
		if err == nil || n == len(pending) {
			return nil
		}
		// Everything before the first unsettled pending event is settled.
		settled = index[n]

		var permanent *PermanentError
		if !errors.As(err, &permanent) {
			p.cfg.Metrics.addFailed(p.Name(), "transient")
			return withSettled(settled, err)
		}
		p.cfg.Metrics.addFailed(p.Name(), "permanent")
		refused := permanent.Events
		if len(refused) == 0 {
			refused = pending[n:]
		}
		refused = refused[:min(len(refused), len(pending)-n)]
		ref := refused[0].Ref()

		p.mu.Lock()
		if p.refusedRef != ref {
			p.refusedRef, p.refusedCount = ref, 0
		}
		p.refusedCount++
		attempts := p.refusedCount
		p.mu.Unlock()

		if p.cfg.DeadLetters == nil || attempts < p.cfg.MaxPermanentAttempts {
			return withSettled(settled, err)
		}
		if storeErr := p.cfg.DeadLetters.RecordDeadLetters(ctx, p.Name(), refused, permanent.Error(), attempts); storeErr != nil {
			// Recording failed: keep the event pending rather than lose the
			// only copy of the decision.
			return withSettled(settled, errors.Join(err, fmt.Errorf("record dead letters: %w", storeErr)))
		}
		p.cfg.Metrics.addDeadLettered(p.Name(), refused)
		refs := make([]string, len(refused))
		for i, e := range refused {
			refs[i] = e.Ref()
		}
		p.cfg.Logger.Error("auditexport: event dead-lettered after repeated permanent rejection; it remains in its source table",
			"sink", p.Name(), "attempts", attempts, "refs", refs, "reason", permanent.Error())
		p.remember(refused)
		p.mu.Lock()
		p.refusedRef, p.refusedCount = "", 0
		p.mu.Unlock()
		// The dead-lettered events are settled; carry on with the rest.
		last := index[n+len(refused)-1]
		settled = last + 1
	}
	return nil
}

func (p *pipeline) remember(events []Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range events {
		if ref := e.Ref(); ref != "" {
			p.memo.add(ref)
		}
	}
}

// refMemo is a bounded set: the oldest ref is forgotten first.
type refMemo struct {
	set  map[string]struct{}
	ring []string
	next int
}

func newRefMemo(size int) *refMemo {
	return &refMemo{set: make(map[string]struct{}, size), ring: make([]string, size)}
}

func (m *refMemo) has(ref string) bool {
	if ref == "" {
		return false
	}
	_, ok := m.set[ref]
	return ok
}

func (m *refMemo) add(ref string) {
	if _, ok := m.set[ref]; ok {
		return
	}
	if old := m.ring[m.next]; old != "" {
		delete(m.set, old)
	}
	m.ring[m.next] = ref
	m.set[ref] = struct{}{}
	m.next = (m.next + 1) % len(m.ring)
}

// Fanout sends to every sink and returns an error when any failed. It serves
// the security audit relay, which has one acknowledgement per outbox row: a
// sink that failed keeps the rows pending for all of them, and the per-event
// memo in Wrap stops healthy sinks receiving them again. The audit_events
// relay does not use it; it drives each sink from its own position.
type Fanout []Sink

func (f Fanout) Name() string {
	names := ""
	for i, s := range f {
		if i > 0 {
			names += "+"
		}
		names += s.Name()
	}
	return names
}

func (f Fanout) Send(ctx context.Context, events []Event) error {
	var errs []error
	for _, s := range f {
		if err := s.Send(ctx, events); err != nil {
			// A partial delivery by one sink is not a partial acknowledgement
			// of the fan-out: drop the settled count.
			var partial *PartialError
			if errors.As(err, &partial) {
				err = partial.Err
			}
			errs = append(errs, fmt.Errorf("sink %s: %w", s.Name(), err))
		}
	}
	return errors.Join(errs...)
}

// DecodeSinkName labels the dead letters and metrics of security outbox
// records that cannot be decoded into an event.
const DecodeSinkName = "decode"

// SecurityExporter adapts a Sink to securityaudit.Exporter, the interface the
// security audit relay offers batches to. The relay marks rows exported only
// when Export returns nil.
type SecurityExporter struct {
	Sink Sink
	// DeadLetters records an outbox record that cannot be decoded, so that one
	// record does not block the stream. Nil keeps the old behaviour: the
	// decode error is returned and the batch stays pending.
	DeadLetters DeadLetterStore
	Metrics     *Metrics
	Logger      *slog.Logger
}

// Name is the exporter's configuration name.
func (e SecurityExporter) Name() string { return e.Sink.Name() }

// Export maps the records and sends them. A record that cannot be decoded
// will never decode, however often it is retried: it is dead-lettered alone
// (it stays in the outbox and the durable store) and the rest are sent.
func (e SecurityExporter) Export(ctx context.Context, records []securityaudit.Record) error {
	events := make([]Event, 0, len(records))
	var broken []Event
	var reasons []string
	for _, record := range records {
		event, err := FromSecurityRecord(record)
		if err != nil {
			if e.DeadLetters == nil {
				return err
			}
			broken = append(broken, undecodableEvent(record))
			reasons = append(reasons, err.Error())
			continue
		}
		events = append(events, event)
	}
	for i, event := range broken {
		if err := e.DeadLetters.RecordDeadLetters(ctx, DecodeSinkName, []Event{event}, reasons[i], 1); err != nil {
			return fmt.Errorf("record undecodable security audit record %s: %w", event.Ref(), err)
		}
		if e.Metrics != nil {
			e.Metrics.addDeadLettered(DecodeSinkName, []Event{event})
		}
		logger := e.Logger
		if logger == nil {
			logger = slog.Default()
		}
		logger.Error("auditexport: security audit record cannot be decoded; dead-lettered, the rest of the batch is exported",
			"ref", event.Ref(), "event_id", event.EventID, "reason", reasons[i])
	}
	if len(events) == 0 {
		return nil
	}
	return e.Sink.Send(ctx, events)
}

// undecodableEvent is the dead-letter body of an outbox record that does not
// map to an envelope: its identifying fields and the stored bytes, verbatim.
func undecodableEvent(record securityaudit.Record) Event {
	raw := json.RawMessage(record.Event)
	if !json.Valid(raw) {
		b, _ := json.Marshal(string(record.Event))
		raw = b
	}
	return Event{
		Schema:    SchemaVersion,
		EventID:   record.EventID,
		Time:      record.CreatedAt.UTC().Format("2006-01-02T15:04:05.000000Z07:00"),
		Source:    SourceSecurityAudit,
		EventType: string(record.EventType),
		Actor:     Actor{Type: "unknown", ID: "unknown"},
		Outcome:   OutcomeUnknown,
		Details:   map[string]any{"undecodable": true, "stored_event": raw},
		ref:       string(SourceSecurityAudit) + ":" + strconv.FormatInt(record.Seq, 10),
	}
}

var _ securityaudit.Exporter = SecurityExporter{}
