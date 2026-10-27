package epoch

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Change announces that a record moved to a new epoch. It is a reference:
// kind, id, new epoch and when it changed. Never a tenant secret, a user or
// a value.
type Change struct {
	Epoch SecurityEpoch
	// ChangedAt is the commit time the writer observed; zero when unknown.
	ChangedAt time.Time
}

// Notifier is the seam every transition site calls after its transaction
// commits. Notify never blocks on the network and never fails the caller:
// delivery is advisory.
type Notifier interface {
	Notify(ctx context.Context, changes ...Change)
}

// Listener receives every change, local or remote. It must not block.
type Listener func(Change)

// Transport carries changes between replicas (core NATS in production).
type Transport interface {
	Publish(data []byte) error
	Subscribe(deliver func([]byte)) (unsubscribe func() error, err error)
}

// Instrument names. Labels are bounded codes only: never a tenant, user,
// execution or record id.
const (
	metricInvalidationsReceived = "elitea.credential.invalidation.received"
	metricRevocationDenials     = "elitea.credential.revocation.denials"
	metricRevocationLatency     = "elitea.credential.revocation.deny_latency"
)

const (
	hubQueueSize       = 1024
	hubMaxTrackedCount = 4096
	hubRetryMin        = 250 * time.Millisecond
	hubRetryMax        = 30 * time.Second
	// messageVersion is the wire version of an invalidation message.
	messageVersion  = 1
	maxMessageBytes = 1024
)

type wireChange struct {
	Version     int    `json:"v"`
	Origin      string `json:"origin"`
	Kind        Kind   `json:"kind"`
	ID          string `json:"id"`
	Epoch       int64  `json:"epoch"`
	ChangedAtMs int64  `json:"changed_at_ms,omitempty"`
}

type trackKey struct {
	kind Kind
	id   string
}

// HubOptions configures a Hub. Every field is optional.
type HubOptions struct {
	Meter  metric.Meter
	Logger *slog.Logger
	Now    func() time.Time
}

// Hub is this replica's invalidation fan-out. Built once at the composition
// root, only with credential governance on.
type Hub struct {
	origin string
	logger *slog.Logger
	now    func() time.Time

	mu        sync.RWMutex
	listeners []Listener

	queue       chan Change
	queueWarned atomic.Bool

	trackMu sync.Mutex
	changed map[trackKey]time.Time
	order   []trackKey

	received metric.Int64Counter
	denials  metric.Int64Counter
	latency  metric.Float64Histogram
}

// NewHub builds a hub.
func NewHub(opts HubOptions) (*Hub, error) {
	meter := opts.Meter
	if meter == nil {
		meter = otel.Meter("elitea-main")
	}
	received, err := meter.Int64Counter(metricInvalidationsReceived,
		metric.WithDescription("Security epoch invalidations this replica received, by source (local, remote)."))
	if err != nil {
		return nil, err
	}
	denials, err := meter.Int64Counter(metricRevocationDenials,
		metric.WithDescription("First governed redemption denied after an epoch change, by whether the change time was known."))
	if err != nil {
		return nil, err
	}
	latency, err := meter.Float64Histogram(metricRevocationLatency, metric.WithUnit("s"),
		metric.WithDescription("Time from a security epoch change to the first governed redemption it denied on this replica."))
	if err != nil {
		return nil, err
	}
	h := &Hub{
		origin: uuid.NewString(), logger: opts.Logger, now: opts.Now,
		queue:    make(chan Change, hubQueueSize),
		changed:  map[trackKey]time.Time{},
		received: received, denials: denials, latency: latency,
	}
	if h.logger == nil {
		h.logger = slog.Default()
	}
	if h.now == nil {
		h.now = time.Now
	}
	return h, nil
}

// Listen adds a listener.
func (h *Hub) Listen(l Listener) {
	if h == nil || l == nil {
		return
	}
	h.mu.Lock()
	h.listeners = append(h.listeners, l)
	h.mu.Unlock()
}

// Notify implements Notifier: it delivers to this replica's listeners at
// once and queues the change for the other replicas. A full queue drops the
// remote copy (epochs are re-checked on every hit, so only latency suffers).
func (h *Hub) Notify(ctx context.Context, changes ...Change) {
	if h == nil {
		return
	}
	for _, change := range changes {
		if !change.Epoch.WellFormed() {
			continue
		}
		if change.ChangedAt.IsZero() {
			change.ChangedAt = h.now()
		}
		h.deliver(ctx, change, "local")
		select {
		case h.queue <- change:
		default:
			if h.queueWarned.CompareAndSwap(false, true) {
				h.logger.Warn("security invalidation queue is full; replicas fall back to the epoch re-check")
			}
		}
	}
}

func (h *Hub) deliver(ctx context.Context, change Change, source string) {
	h.track(change)
	h.received.Add(context.WithoutCancel(ctx), 1, metric.WithAttributes(attribute.String("source", source)))
	h.mu.RLock()
	listeners := h.listeners
	h.mu.RUnlock()
	for _, l := range listeners {
		l(change)
	}
}

func (h *Hub) track(change Change) {
	if change.ChangedAt.IsZero() {
		return
	}
	key := trackKey{kind: change.Epoch.Kind, id: change.Epoch.ID}
	h.trackMu.Lock()
	defer h.trackMu.Unlock()
	if previous, ok := h.changed[key]; ok {
		if change.ChangedAt.Before(previous) {
			h.changed[key] = change.ChangedAt
		}
		return
	}
	if len(h.order) >= hubMaxTrackedCount {
		delete(h.changed, h.order[0])
		h.order = h.order[1:]
	}
	h.changed[key] = change.ChangedAt
	h.order = append(h.order, key)
}

// ObserveDenied records the revocation-to-deny latency of the first denial
// each tracked change caused on this replica. A stale key whose change this
// replica never heard of counts as unknown.
func (h *Hub) ObserveDenied(ctx context.Context, stale []SecurityEpoch) {
	if h == nil || len(stale) == 0 {
		return
	}
	ctx = context.WithoutCancel(ctx)
	now := h.now()
	var earliest time.Time
	h.trackMu.Lock()
	for _, key := range stale {
		k := trackKey{kind: key.Kind, id: key.ID}
		if at, ok := h.changed[k]; ok {
			if earliest.IsZero() || at.Before(earliest) {
				earliest = at
			}
			delete(h.changed, k)
			// Drop its slot too: a later change of the same record is then
			// tracked afresh, and evicting a stale slot never deletes it.
			h.order = slices.DeleteFunc(h.order, func(o trackKey) bool { return o == k })
		}
	}
	h.trackMu.Unlock()
	if earliest.IsZero() {
		h.denials.Add(ctx, 1, metric.WithAttributes(attribute.String("change_time", "unknown")))
		return
	}
	h.denials.Add(ctx, 1, metric.WithAttributes(attribute.String("change_time", "known")))
	h.latency.Record(ctx, max(now.Sub(earliest), 0).Seconds())
}

// Run subscribes through t (retrying with backoff until the first subscribe
// succeeds) and publishes queued changes until ctx ends.
func (h *Hub) Run(ctx context.Context, t Transport) error {
	if h == nil || t == nil || ctx == nil {
		return errors.New("epoch: the hub and its transport are required")
	}
	go h.publish(ctx, t)
	delay := hubRetryMin
	for {
		unsubscribe, err := t.Subscribe(h.receive)
		if err == nil {
			<-ctx.Done()
			_ = unsubscribe()
			return ctx.Err()
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		h.logger.Warn("security invalidation subscription failed; replicas fall back to the epoch re-check", "err", err)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		delay = min(delay*2, hubRetryMax)
	}
}

func (h *Hub) publish(ctx context.Context, t Transport) {
	for {
		select {
		case <-ctx.Done():
			return
		case change := <-h.queue:
			h.queueWarned.Store(false)
			encoded, err := h.encode(change)
			if err != nil {
				continue
			}
			if err := t.Publish(encoded); err != nil && ctx.Err() == nil {
				h.logger.Warn("security invalidation publish failed; replicas fall back to the epoch re-check", "err", err)
			}
		}
	}
}

func (h *Hub) encode(change Change) ([]byte, error) {
	w := wireChange{
		Version: messageVersion, Origin: h.origin,
		Kind: change.Epoch.Kind, ID: change.Epoch.ID, Epoch: change.Epoch.Value,
	}
	if !change.ChangedAt.IsZero() {
		w.ChangedAtMs = change.ChangedAt.UnixMilli()
	}
	return json.Marshal(w)
}

// receive handles one remote message. Malformed, foreign-version and own
// messages are dropped.
func (h *Hub) receive(data []byte) {
	if len(data) == 0 || len(data) > maxMessageBytes {
		return
	}
	var w wireChange
	if json.Unmarshal(data, &w) != nil || w.Version != messageVersion || w.Origin == h.origin {
		return
	}
	change := Change{Epoch: SecurityEpoch{Kind: w.Kind, ID: w.ID, Value: w.Epoch}}
	if !change.Epoch.WellFormed() {
		return
	}
	if w.ChangedAtMs > 0 {
		change.ChangedAt = time.UnixMilli(w.ChangedAtMs)
	}
	h.deliver(context.Background(), change, "remote")
}

var _ Notifier = (*Hub)(nil)
