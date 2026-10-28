package auditexport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// Sink delivers a batch of events to one receiver. Send returns nil only
// when the receiver accepted every event.
//
// An error is transient unless it wraps a *PermanentError: a transient error
// is retried by the relay forever, and only an outage alert tells an operator;
// a permanent error means this exact batch was refused as malformed.
type Sink interface {
	Name() string
	Send(ctx context.Context, events []Event) error
}

// PermanentError marks a batch the receiver refused for its content (HTTP
// 400, 413 or 422). Retrying the same bytes cannot succeed.
type PermanentError struct{ Err error }

func (e *PermanentError) Error() string { return "permanent rejection: " + e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// batchKey identifies a batch by its event ids: the relays re-offer the same
// head batch until it is delivered.
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
	// MaxPermanentAttempts is how many times the same batch may be refused
	// permanently before it is dead-lettered. Default 5.
	MaxPermanentAttempts int
	Logger               *slog.Logger
}

// DefaultMaxPermanentAttempts is PipelineConfig.MaxPermanentAttempts' default.
const DefaultMaxPermanentAttempts = 5

// Wrap adds the pipeline every sink runs through, outermost first:
//
//   - memo: a batch this sink already accepted is not sent again when another
//     sink of the fan-out failed, so one dead receiver does not make the
//     healthy one see every batch each second;
//   - metrics: delivered and failed counters;
//   - dead letter: ONLY a permanent rejection of the same batch, repeated,
//     is dead-lettered. A transient failure (receiver down, 5xx, 429, wrong
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
	cfg.Metrics.Register(sink.Name())
	return &pipeline{inner: sink, cfg: cfg}
}

type pipeline struct {
	inner Sink
	cfg   PipelineConfig

	mu           sync.Mutex
	lastAccepted string
	refusedKey   string
	refusedCount int
}

func (p *pipeline) Name() string { return p.inner.Name() }

func (p *pipeline) Send(ctx context.Context, events []Event) error {
	if len(events) == 0 {
		return nil
	}
	key := batchKey(events)
	p.mu.Lock()
	if p.lastAccepted == key {
		p.mu.Unlock()
		return nil
	}
	p.mu.Unlock()

	err := p.inner.Send(ctx, events)
	if err == nil {
		p.mu.Lock()
		p.lastAccepted, p.refusedKey, p.refusedCount = key, "", 0
		p.mu.Unlock()
		p.cfg.Metrics.addDelivered(p.Name(), events)
		return nil
	}
	var permanent *PermanentError
	if !errors.As(err, &permanent) {
		p.cfg.Metrics.addFailed(p.Name(), "transient")
		return err
	}
	p.cfg.Metrics.addFailed(p.Name(), "permanent")

	p.mu.Lock()
	if p.refusedKey != key {
		p.refusedKey, p.refusedCount = key, 0
	}
	p.refusedCount++
	attempts := p.refusedCount
	p.mu.Unlock()

	if p.cfg.DeadLetters == nil || attempts < p.cfg.MaxPermanentAttempts {
		return err
	}
	if storeErr := p.cfg.DeadLetters.RecordDeadLetters(ctx, p.Name(), events, permanent.Error(), attempts); storeErr != nil {
		// Recording failed: keep the batch pending rather than lose the only
		// copy of the decision.
		return errors.Join(err, fmt.Errorf("record dead letters: %w", storeErr))
	}
	p.cfg.Metrics.addDeadLettered(p.Name(), events)
	refs := make([]string, len(events))
	for i, e := range events {
		refs[i] = e.Ref()
	}
	p.cfg.Logger.Error("auditexport: batch dead-lettered after repeated permanent rejection; the events remain in their source tables",
		"sink", p.Name(), "attempts", attempts, "events", len(events), "refs", refs, "reason", permanent.Error())
	p.mu.Lock()
	p.lastAccepted, p.refusedKey, p.refusedCount = key, "", 0
	p.mu.Unlock()
	return nil
}

// Fanout sends to every sink and returns an error when any failed. A sink
// that failed keeps the batch pending for all of them (the relays have one
// acknowledgement per row); the memo in Wrap stops healthy sinks resending.
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
			errs = append(errs, fmt.Errorf("sink %s: %w", s.Name(), err))
		}
	}
	return errors.Join(errs...)
}

// SecurityExporter adapts a Sink to securityaudit.Exporter, the interface the
// security audit relay offers batches to. The relay marks rows exported only
// when Export returns nil.
type SecurityExporter struct{ Sink Sink }

// Name is the exporter's configuration name.
func (e SecurityExporter) Name() string { return e.Sink.Name() }

// Export maps the records and sends them.
func (e SecurityExporter) Export(ctx context.Context, records []securityaudit.Record) error {
	events := make([]Event, 0, len(records))
	for _, record := range records {
		event, err := FromSecurityRecord(record)
		if err != nil {
			return err
		}
		events = append(events, event)
	}
	return e.Sink.Send(ctx, events)
}

var _ securityaudit.Exporter = SecurityExporter{}
