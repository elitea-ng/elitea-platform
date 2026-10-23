package budgetwriteback

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

const (
	// defaultBatchSize is the max messages a single Fetch pulls (§8.6: "up to
	// 500 messages or a short block timeout").
	defaultBatchSize = 500
	// defaultFetchWait bounds a Fetch that finds fewer than batchSize messages,
	// so an idle stream doesn't block the loop forever.
	defaultFetchWait = 5 * time.Second
)

// Message is the minimal ack surface of a JetStream message the consumer drains.
// jetstream.Msg satisfies it (Data/Ack/Nak/Term), so the real batch adapter
// passes messages straight through and tests inject fakes with no live server.
type Message interface {
	Data() []byte
	Ack() error
	Nak() error
	NakWithDelay(delay time.Duration) error
	InProgress() error
	Term() error
	Metadata() (*jetstream.MsgMetadata, error)
}

// The redelivery backoff for a delta that cannot be applied yet ([human
// decision] 2026-10-16, "Healthy write", consumer half).
//
// A bare Nak redelivers at once. A delta deferred by an outage-owned row (the
// row stays outage-owned until the gateway's recovery sweep, every 30s) was
// therefore redelivered back-to-back, and a busy stream spent the consumer's
// MaxDeliver (10) in about a second and dropped the delta: money lost from the
// accumulator and the usage ledger. The delay now starts at 5s and doubles per
// delivery, to a 60s ceiling, so one deferral spans the sweep it waits for. The
// nats-bootstrap chart sets MaxDeliver to -1 for this consumer: with a delayed
// retry, the only bound a deferred delta needs is the stream's own MaxAge
// (72h), and a finite cap only decides when real money is dropped.
const (
	redeliverBaseDelay = 5 * time.Second
	redeliverMaxDelay  = 60 * time.Second
)

// redeliverDelay is the wait before the next delivery of a message delivered
// `delivered` times so far (1 on the first delivery).
func redeliverDelay(delivered uint64) time.Duration {
	delay := redeliverBaseDelay
	for i := uint64(1); i < delivered && delay < redeliverMaxDelay; i++ {
		delay *= 2
	}
	return min(delay, redeliverMaxDelay)
}

// Fetcher pulls the next batch of messages. The real implementation wraps a
// jetstream.Consumer.Fetch; tests inject a fake to drive the drain loop offline.
type Fetcher interface {
	// Fetch returns up to the configured batch size, blocking at most the
	// configured wait. An empty slice with a nil error means the stream was
	// idle — the loop simply fetches again.
	Fetch(ctx context.Context) ([]Message, error)
}

// Config tunes the write-back drain loop. Zero values fall back to §8.6
// defaults. The consumer's own settings (AckWait, MaxDeliver) are not here:
// the nats-bootstrap Job creates the consumer with them (#1076), and the
// scheduler only binds.
type Config struct {
	BatchSize int
	FetchWait time.Duration
}

func (c Config) withDefaults() Config {
	if c.BatchSize <= 0 {
		c.BatchSize = defaultBatchSize
	}
	if c.FetchWait <= 0 {
		c.FetchWait = defaultFetchWait
	}
	return c
}

// Consumer drains GATEWAY_BUDGET_DELTAS and lazily persists deltas to the
// durable accumulator tier (design §8.6). It is a single-goroutine drain loop;
// a durable pull consumer gives multi-replica safety at the JetStream tier.
type Consumer struct {
	fetch  Fetcher
	store  *Store
	logger *slog.Logger
	// lastPrune is when the usage-ledger retention prune last ran. It is only
	// ever touched from Run's single goroutine, so it needs no lock.
	lastPrune time.Time
	// info reads the bound consumer's server-side figures for the
	// num_ack_pending / num_redelivered gauges; nil leaves them unset.
	info     consumerInfoSource
	lastInfo time.Time
}

// NewConsumer builds a write-back Consumer over a message Fetcher and a Store.
func NewConsumer(fetch Fetcher, store *Store, logger *slog.Logger) *Consumer {
	if logger == nil {
		logger = slog.Default()
	}
	return &Consumer{fetch: fetch, store: store, logger: logger}
}

// Run drains the stream until ctx is cancelled or the consumer is gone (see
// consumerLost; the Supervisor re-attaches). Each pass fetches a batch,
// coalesces per (scope, scope_id, period_start), and applies each key-group in
// its own transaction; messages are ACK'd only after their group's transaction
// commits (§8.6). A fetch error is logged and retried after a short backoff so a
// transient JetStream blip doesn't kill the loop.
func (c *Consumer) Run(ctx context.Context) {
	c.logger.Info("budgetwriteback: consumer started")
	// The gauges describe a bound consumer; withdraw them when this drain
	// loop stops (the Supervisor re-attaches and refreshes them).
	defer metrics.clearConsumerInfo()
	for {
		if ctx.Err() != nil {
			c.logger.Info("budgetwriteback: consumer stopping")
			return
		}
		c.pruneUsageEventsIfDue(ctx)
		c.refreshConsumerInfoIfDue(ctx)
		msgs, err := c.fetch.Fetch(ctx)
		if err != nil {
			if ctx.Err() != nil {
				c.logger.Info("budgetwriteback: consumer stopping")
				return
			}
			if consumerLost(err) {
				// No retry of this handle can succeed; the Supervisor binds
				// again once the bootstrap has re-created the consumer.
				c.logger.Warn("budgetwriteback: the consumer is gone; stopping this drain loop", "err", err)
				return
			}
			c.logger.Warn("budgetwriteback: fetch failed; retrying", "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			continue
		}
		if len(msgs) == 0 {
			continue
		}
		c.processBatch(ctx, msgs)
	}
}

// refreshConsumerInfoIfDue updates the consumer gauges at most every
// consumerInfoInterval. Best-effort: a failed read keeps the last value.
func (c *Consumer) refreshConsumerInfoIfDue(ctx context.Context) {
	if c.info == nil {
		return
	}
	now := time.Now()
	if !c.lastInfo.IsZero() && now.Sub(c.lastInfo) < consumerInfoInterval {
		return
	}
	c.lastInfo = now
	info, err := c.info.Info(ctx)
	if err != nil {
		if ctx.Err() == nil {
			c.logger.Debug("budgetwriteback: consumer info read failed", "err", err)
		}
		return
	}
	metrics.setConsumerInfo(info)
}

// pruneInterval is how often the drain loop prunes the usage ledger. The prune
// is a bounded DELETE of rows past RetentionWindow, so it does not need to be
// frequent; hourly keeps the table inside its bound without competing with the
// drain for the pool.
const pruneInterval = time.Hour

// pruneUsageEventsIfDue runs the retention prune when the interval has elapsed
// (issue #320). It rides the drain loop rather than a goroutine of its own so
// it stops when the loop stops, and it is best-effort: a prune failure is
// logged and retried on the next tick, never allowed to interrupt the drain the
// money path depends on.
//
// It deletes in batches until a pass comes back short, so the prune keeps up
// with call volume rather than with the tick interval.
//
// The first pass happens on the loop's first iteration, so a scheduler that
// starts against a database holding years of rows begins reducing it
// immediately instead of an hour later.
func (c *Consumer) pruneUsageEventsIfDue(ctx context.Context) {
	now := time.Now()
	if !c.lastPrune.IsZero() && now.Sub(c.lastPrune) < pruneInterval {
		return
	}
	c.lastPrune = now

	// Delete in batches until a pass comes back short. One bounded pass per
	// hour caps the prune at pruneBatchSize rows an hour, which is BELOW the
	// call volume of any busy deployment — the table would then grow past the
	// retention window for ever while this function logged that it was
	// pruning. The batch bounds one statement's lock, not the work.
	//
	// maxPrunePasses stops a first run against years of history from holding
	// the drain loop; whatever is left goes on the next tick.
	var total int64
	for pass := 0; pass < maxPrunePasses; pass++ {
		deleted, err := c.store.PruneUsageEvents(ctx)
		if err != nil {
			if ctx.Err() == nil {
				c.logger.Warn("budgetwriteback: usage-ledger prune failed; retrying next tick",
					"deleted_before_error", total, "err", err)
			}
			return
		}
		total += deleted
		if deleted < pruneBatchSize {
			break
		}
		if ctx.Err() != nil {
			return
		}
	}
	if total > 0 {
		c.logger.Info("budgetwriteback: pruned usage-ledger rows past retention",
			"deleted", total, "retention", RetentionWindow.String())
	}
}

// maxPrunePasses bounds one tick's prune. At pruneBatchSize=5000 this clears up
// to 1,000,000 rows per tick, which is far above any plausible hour of billed
// calls, so a steady-state deployment always finishes in the first short pass.
const maxPrunePasses = 200

// decoded pairs a parsed delta with its source message so the message can be
// ACK'd/NAK'd after its group's outcome is known.
type decoded struct {
	delta BudgetDelta
	msg   Message
}

// processBatch decodes, coalesces, and applies one fetched batch. Poison
// messages (bad JSON or failing validation) are Term()'d immediately so they are
// not redelivered forever. Valid messages are grouped by key; each group is
// applied in one transaction and its messages ACK'd on commit or NAK'd on a
// transient failure / outage deferral.
func (c *Consumer) processBatch(ctx context.Context, msgs []Message) {
	groups := map[deltaKey][]decoded{}
	var order []deltaKey

	for _, m := range msgs {
		var d BudgetDelta
		if err := json.Unmarshal(m.Data(), &d); err != nil {
			c.logger.Error("budgetwriteback: undecodable delta; terminating", "err", err)
			c.term(m)
			continue
		}
		if err := d.validate(); err != nil {
			c.logger.Error("budgetwriteback: invalid delta; terminating",
				"event_id", d.EventID, "err", err)
			c.term(m)
			continue
		}
		k := d.key()
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], decoded{delta: d, msg: m})
	}

	for _, k := range order {
		c.applyGroup(ctx, groups[k])
	}
}

// applyGroup applies one coalesced key-group and settles its messages.
func (c *Consumer) applyGroup(ctx context.Context, items []decoded) {
	deltas := make([]BudgetDelta, len(items))
	for i, it := range items {
		deltas[i] = it.delta
	}

	// AckWait (30s) runs from the FETCH, for the whole batch of up to 500, and
	// the groups (and isolated members) apply one after another. So reset each
	// group's ack timer before its own Apply: a later group then has the full
	// AckWait for its transaction and is not redelivered mid-apply. The apply
	// itself is bounded below AckWait. A redelivery that does race an apply
	// is still SAFE: processed_event_ids dedup in the same transaction makes
	// the second apply add nothing; resetting only avoids the wasted work.
	for _, it := range items {
		if err := it.msg.InProgress(); err != nil {
			c.logger.Debug("budgetwriteback: in-progress ack failed", "event_id", it.delta.EventID, "err", err)
		}
	}
	applyCtx, cancel := context.WithTimeout(ctx, applyTimeout)
	outcome, err := c.store.Apply(applyCtx, deltas)
	cancel()
	class, permanent := permanentApplyFailure(err)
	switch {
	case err != nil && permanent && len(items) > 1:
		// Isolate ([human decision] 2026-10-16, "Isolate, then drop"): the
		// group failed on ONE delta's data, so apply each member alone and
		// only that one is dropped. A redelivery would re-coalesce it with
		// its neighbours for ever. A transient or environmental group error
		// is NOT isolated: every member would fail the same way, and a
		// k-member group would cost k+1 doomed transactions.
		c.logger.Warn("budgetwriteback: group apply failed on a delta's data; applying each delta alone",
			"scope", items[0].delta.Scope, "scope_id", items[0].delta.ScopeID,
			"deltas", len(items), "sqlstate_class", class, "err", err)
		for _, it := range items {
			c.applyGroup(ctx, []decoded{it})
		}
	case err != nil && permanent:
		// Drop: this delta fails the same way on every delivery, and with
		// MaxDeliver -1 it would retry for the stream's whole MaxAge while
		// holding an ack-pending slot. Its money is lost from the
		// accumulator; the ERROR line and the counter make that visible.
		c.logger.Error("budgetwriteback: delta fails permanently; terminating it (its spend is NOT in the accumulator)",
			"event_id", items[0].delta.EventID, "scope", items[0].delta.Scope,
			"scope_id", items[0].delta.ScopeID, "delta_nano_usd", items[0].delta.DeltaNanoUSD,
			"sqlstate_class", class, "err", err)
		metrics.addDropped(droppedReasonPermanent, class)
		c.term(items[0].msg)
	case err != nil && environmentalApplyFailure(err):
		// Class 42 ([human decision] 2026-10-16, "Drop only 22/23, retry
		// 42"): the schema or the grants are wrong for every delta, for
		// example a scheduler started before the migrations. Deferred, never
		// dropped; ERROR so an operator fixes the environment.
		c.logger.Error("budgetwriteback: apply fails on the database schema or grants; deferring (fix the environment)",
			"reason", "environmental", "scope", items[0].delta.Scope, "scope_id", items[0].delta.ScopeID,
			"deltas", len(items), "sqlstate_class", class, "err", err)
		c.nakAll(items)
	case err != nil:
		// Transient DB failure: NAK for redelivery (§8.6 at-least-once). The
		// same-transaction dedup guarantees the retry does not double-apply.
		c.logger.Warn("budgetwriteback: apply failed; will redeliver",
			"scope", items[0].delta.Scope, "scope_id", items[0].delta.ScopeID, "err", err)
		c.nakAll(items)
	case outcome == outcomeDeferred:
		// Row is outage-owned by the gateway recovery goroutine; nothing was
		// persisted. NAK so the delta is redelivered after recovery clears the
		// outage flag (§8.5).
		c.logger.Info("budgetwriteback: accumulator in outage state; deferring",
			"scope", items[0].delta.Scope, "scope_id", items[0].delta.ScopeID)
		c.nakAll(items)
	default:
		c.ackAll(items)
	}
}

// applyTimeout bounds one Apply transaction, so a stalled database cannot hold
// the drain loop. It is below the consumer's AckWait (30s,
// deploy/helm/elitea natsBootstrap deltas.writeback.ackWait), and applyGroup resets
// the group's ack timer (InProgress) just before the Apply, so a delta is not
// normally redelivered while it applies. That is an efficiency property, not
// the money invariant: exactly-once comes from the processed_event_ids claim
// in the same transaction, which makes any redelivery add nothing. A timed-out
// Apply rolls back and the group is deferred (transient), never dropped.
const applyTimeout = 20 * time.Second

func (c *Consumer) ackAll(items []decoded) {
	for _, it := range items {
		if err := it.msg.Ack(); err != nil {
			c.logger.Warn("budgetwriteback: ack failed", "event_id", it.delta.EventID, "err", err)
		}
	}
}

// nakAll defers every message of a group with the delivery-count backoff
// (redeliverDelay), never a bare Nak: see redeliverBaseDelay.
func (c *Consumer) nakAll(items []decoded) {
	for _, it := range items {
		var delivered uint64 = 1
		if meta, err := it.msg.Metadata(); err == nil && meta != nil && meta.NumDelivered > 0 {
			delivered = meta.NumDelivered
		}
		if err := it.msg.NakWithDelay(redeliverDelay(delivered)); err != nil {
			c.logger.Warn("budgetwriteback: nak failed", "event_id", it.delta.EventID, "err", err)
		}
	}
}

func (c *Consumer) term(m Message) {
	if err := m.Term(); err != nil {
		c.logger.Warn("budgetwriteback: term failed", "err", err)
	}
}

// ErrConsumerMissing is wrapped by Bind when the durable consumer (or its
// stream) does not exist yet: the nats-bootstrap Job has not run.
var ErrConsumerMissing = errors.New("the budget write-back consumer does not exist")

// verifyConsumer checks what the drain loop depends on in the consumer the
// bootstrap created: a pull consumer (no deliver subject — a push consumer's
// deliveries would go wherever its deliver subject says, not to this loop),
// filtered to exactly the delta subject, with explicit acks (the loop acks
// only after the accumulator transaction commits).
func verifyConsumer(cfg jetstream.ConsumerConfig) error {
	if cfg.DeliverSubject != "" {
		return fmt.Errorf("it is a push consumer delivering to %q; budget write-back drains a pull consumer", cfg.DeliverSubject)
	}
	filters := cfg.FilterSubjects
	if cfg.FilterSubject != "" {
		filters = append([]string{cfg.FilterSubject}, filters...)
	}
	if len(filters) != 1 || filters[0] != DeltaSubject {
		return fmt.Errorf("it filters %v; budget write-back reads exactly %s", filters, DeltaSubject)
	}
	if cfg.AckPolicy != jetstream.AckExplicitPolicy {
		return fmt.Errorf("its ack policy is %s; budget write-back acks each delta explicitly after it is applied", cfg.AckPolicy)
	}
	return nil
}

// jsFetcher adapts a jetstream.Consumer to the Fetcher seam.
type jsFetcher struct {
	cons  jetstream.Consumer
	batch int
	wait  time.Duration
}

// Fetch pulls one batch and converts the jetstream messages to the Message
// seam. A batch-level error (after draining whatever messages arrived) is
// returned so the loop can back off; individual jetstream.Msg values already
// satisfy Message so no per-message wrapping is needed.
func (f *jsFetcher) Fetch(ctx context.Context) ([]Message, error) {
	mb, err := f.cons.Fetch(f.batch, jetstream.FetchMaxWait(f.wait))
	if err != nil {
		return nil, err
	}
	var out []Message
	for m := range mb.Messages() {
		out = append(out, m)
	}
	if berr := mb.Error(); berr != nil && !errors.Is(berr, context.DeadlineExceeded) {
		return out, berr
	}
	return out, nil
}

// Bind binds to the durable pull consumer on GATEWAY_BUDGET_DELTAS, verifies
// it, and returns a Consumer ready to Run. js is the scheduler's JetStream
// handle (DialConfig.JetStream: in a cluster, the SCHEDULER account's import
// prefix onto GATEWAY's consumer API).
//
// It creates nothing (#1076). The nats-bootstrap Job creates the consumer,
// and the scheduler's NATS identity may read its info, pull and ack, but not
// create or redefine a consumer: that grant would also let it turn the
// consumer into a push consumer delivering spend deltas onto any subject in
// the GATEWAY account. GATEWAY exports SCHEDULER exactly those three
// subjects, so nothing else the scheduler sends reaches GATEWAY at all.
func Bind(ctx context.Context, js jetstream.JetStream, db DB, cfg Config, logger *slog.Logger) (*Consumer, error) {
	cfg = cfg.withDefaults()
	cons, err := js.Consumer(ctx, DeltasStream, DurableName)
	if err != nil {
		if errors.Is(err, jetstream.ErrConsumerNotFound) || errors.Is(err, jetstream.ErrStreamNotFound) {
			return nil, fmt.Errorf("%w: %s/%s is created by the nats-bootstrap Job (deploy/helm/elitea natsBootstrap; in compose, the nats-bootstrap service) — run it", ErrConsumerMissing, DeltasStream, DurableName)
		}
		return nil, fmt.Errorf("bind %s/%s: %w", DeltasStream, DurableName, err)
	}
	if err := verifyConsumer(cons.CachedInfo().Config); err != nil {
		return nil, fmt.Errorf("consumer %s/%s: %w; it is configured by the nats-bootstrap Job — re-run it", DeltasStream, DurableName, err)
	}
	if logger == nil {
		logger = slog.Default()
	}
	if maxDeliver := cons.CachedInfo().Config.MaxDeliver; maxDeliver > 0 {
		// Not a refusal: an older bootstrap left the finite cap, and the next
		// bootstrap run reconciles it. Until then a delta deferred longer than
		// the backoff spans is still dropped, so say so.
		logger.Warn("budgetwriteback: the consumer drops a delta after a finite number of deliveries; "+
			"re-run the nats-bootstrap Job so a delta deferred by an outage is kept (MaxDeliver -1)",
			"max_deliver", maxDeliver)
	}
	fetcher := &jsFetcher{cons: cons, batch: cfg.BatchSize, wait: cfg.FetchWait}
	consumer := NewConsumer(fetcher, NewStore(db), logger)
	consumer.info = cons
	metrics.setConsumerInfo(cons.CachedInfo())
	return consumer, nil
}
