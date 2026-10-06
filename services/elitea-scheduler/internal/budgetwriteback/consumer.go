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
	Term() error
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
	for {
		if ctx.Err() != nil {
			c.logger.Info("budgetwriteback: consumer stopping")
			return
		}
		c.pruneUsageEventsIfDue(ctx)
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

	outcome, err := c.store.Apply(ctx, deltas)
	switch {
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

func (c *Consumer) ackAll(items []decoded) {
	for _, it := range items {
		if err := it.msg.Ack(); err != nil {
			c.logger.Warn("budgetwriteback: ack failed", "event_id", it.delta.EventID, "err", err)
		}
	}
}

func (c *Consumer) nakAll(items []decoded) {
	for _, it := range items {
		if err := it.msg.Nak(); err != nil {
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
			return nil, fmt.Errorf("%w: %s/%s is created by the nats-bootstrap Job (deploy/helm/nats-bootstrap; in compose, the nats-bootstrap service) — run it", ErrConsumerMissing, DeltasStream, DurableName)
		}
		return nil, fmt.Errorf("bind %s/%s: %w", DeltasStream, DurableName, err)
	}
	if err := verifyConsumer(cons.CachedInfo().Config); err != nil {
		return nil, fmt.Errorf("consumer %s/%s: %w; it is configured by the nats-bootstrap Job — re-run it", DeltasStream, DurableName, err)
	}
	fetcher := &jsFetcher{cons: cons, batch: cfg.BatchSize, wait: cfg.FetchWait}
	return NewConsumer(fetcher, NewStore(db), logger), nil
}
