package budgetwriteback

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/nats-io/nats.go/jetstream"
)

// The write-back consumer's operator metrics, in the Prometheus text format on
// the scheduler's health port (GET /metrics). The scheduler has no metrics
// library, and these are the only figures it needs to expose, so the
// exposition is written here.
//
//   - budget_writeback_dropped_total{reason,sqlstate_class}: deltas TERMINATED
//     because their apply fails PERMANENTLY ([human decision] 2026-10-16,
//     "Isolate, then drop", revised "Drop only 22/23, retry 42"). reason is
//     "permanent_apply_error"; sqlstate_class is "22" (data) or "23"
//     (constraint, never the dedup conflict). Class 42 is never dropped. Each
//     drop is spend missing from the accumulator and the usage ledger, so
//     alarm on any increase (the chart's PrometheusRule does).
//   - budget_writeback_num_ack_pending / budget_writeback_num_redelivered: the
//     budget-writeback consumer's own figures, read from the server. A
//     growing ack-pending count is a deferral burst approaching
//     MaxAckPending; num_redelivered is how many are waiting on a redelivery.
var metrics = newWriteBackMetrics()

type writeBackMetrics struct {
	mu      sync.Mutex
	dropped map[droppedKey]int64

	numAckPending  atomic.Int64
	numRedelivered atomic.Int64
	consumerSeen   atomic.Bool
}

type droppedKey struct{ reason, class string }

// droppedReasonPermanent is the one reason a delta is dropped for.
const droppedReasonPermanent = "permanent_apply_error"

// newWriteBackMetrics pre-creates every drop series at 0. A counter series
// that first appears at 1 makes increase() see no change, so the FIRST
// dropped delta, the common single-poison case, would never page.
func newWriteBackMetrics() *writeBackMetrics {
	return &writeBackMetrics{dropped: map[droppedKey]int64{
		{droppedReasonPermanent, "22"}: 0,
		{droppedReasonPermanent, "23"}: 0,
	}}
}

func (m *writeBackMetrics) addDropped(reason, class string) {
	m.mu.Lock()
	m.dropped[droppedKey{reason, class}]++
	m.mu.Unlock()
}

// totalDropped reports every series' sum (tests).
func (m *writeBackMetrics) totalDropped() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for _, v := range m.dropped {
		n += v
	}
	return n
}

// droppedCount reports one series' value (tests).
func (m *writeBackMetrics) droppedCount(reason, class string) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.dropped[droppedKey{reason, class}]
}

// clearConsumerInfo withdraws the consumer gauges: the drain loop stopped, so
// the last figures read are no longer this scheduler's view of the consumer,
// and a scrape must not keep reporting them.
func (m *writeBackMetrics) clearConsumerInfo() {
	m.consumerSeen.Store(false)
	m.numAckPending.Store(0)
	m.numRedelivered.Store(0)
}

func (m *writeBackMetrics) setConsumerInfo(info *jetstream.ConsumerInfo) {
	if info == nil {
		return
	}
	m.numAckPending.Store(int64(info.NumAckPending))
	m.numRedelivered.Store(int64(info.NumRedelivered))
	m.consumerSeen.Store(true)
}

// MetricsHandler serves the write-back metrics. Mount it on the scheduler's
// health mux at /metrics.
func MetricsHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(metrics.render()))
	})
}

func (m *writeBackMetrics) render() string {
	var b strings.Builder
	b.WriteString("# HELP budget_writeback_dropped_total Deltas terminated because their apply fails permanently.\n")
	b.WriteString("# TYPE budget_writeback_dropped_total counter\n")
	m.mu.Lock()
	keys := make([]droppedKey, 0, len(m.dropped))
	for k := range m.dropped {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].reason != keys[j].reason {
			return keys[i].reason < keys[j].reason
		}
		return keys[i].class < keys[j].class
	})
	for _, k := range keys {
		fmt.Fprintf(&b, "budget_writeback_dropped_total{reason=%q,sqlstate_class=%q} %d\n", k.reason, k.class, m.dropped[k])
	}
	m.mu.Unlock()
	if m.consumerSeen.Load() {
		b.WriteString("# HELP budget_writeback_num_ack_pending Deliveries of the budget-writeback consumer not yet acked.\n")
		b.WriteString("# TYPE budget_writeback_num_ack_pending gauge\n")
		fmt.Fprintf(&b, "budget_writeback_num_ack_pending %d\n", m.numAckPending.Load())
		b.WriteString("# HELP budget_writeback_num_redelivered Messages of the budget-writeback consumer delivered more than once.\n")
		b.WriteString("# TYPE budget_writeback_num_redelivered gauge\n")
		fmt.Fprintf(&b, "budget_writeback_num_redelivered %d\n", m.numRedelivered.Load())
	}
	return b.String()
}

// consumerInfoInterval is how often the drain loop refreshes the consumer
// gauges from the server.
const consumerInfoInterval = 30 * time.Second

// consumerInfoSource reads the bound consumer's current info. jetstream.Consumer
// satisfies it.
type consumerInfoSource interface {
	Info(ctx context.Context) (*jetstream.ConsumerInfo, error)
}

// permanentApplyFailure classifies an Apply error ([human decision]
// 2026-10-16, "Isolate, then drop", revised the same day: "Drop only 22/23,
// retry 42"). It reports permanent ONLY for an error in ONE delta's data:
// class 22 (data exception) and class 23 (integrity constraint violation)
// except the dedup conflict 23505 on processed_event_ids.
//
// Class 42 (syntax error or access rule: 42501 permission denied, 42P01 a
// missing table, 42703 a missing column) is NOT permanent. It is the
// environment, not the delta: a scheduler started before the migrations ran,
// or a revoked grant, fails EVERY delta the same way, and dropping them would
// empty the 72h stream into the void. environmentalApplyFailure reports it.
// Anything else, a connection (08), a serialization or deadlock (40), lock
// (55P03), resources (53), operator intervention (57), or an error with no
// SQLSTATE at all, is transient. Both are deferred.
func permanentApplyFailure(err error) (class string, permanent bool) {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || len(pgErr.Code) < 2 {
		return "", false
	}
	class = pgErr.Code[:2]
	switch class {
	case "22":
		return class, true
	case "23":
		// The dedup claim is ON CONFLICT DO NOTHING and never raises; a
		// unique violation on processed_event_ids would be the same event
		// racing another replica, which a retry resolves.
		if pgErr.Code == "23505" && strings.Contains(pgErr.ConstraintName+pgErr.TableName, "processed_event_ids") {
			return class, false
		}
		return class, true
	}
	return class, false
}

// environmentalApplyFailure reports a class-42 error: the schema or the grants
// are wrong for EVERY delta (see permanentApplyFailure). It is deferred like a
// transient error, and logged at ERROR so an operator fixes the environment.
func environmentalApplyFailure(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && strings.HasPrefix(pgErr.Code, "42")
}
