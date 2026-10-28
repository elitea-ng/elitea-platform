package auditexport

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
)

// Metrics are the export's operator figures, in the Prometheus text format.
// elitea-main has no metrics library and no /metrics route on its public
// port, and these are the only figures the export needs, so the exposition is
// written here (the scheduler's budget write-back does the same). The
// listener is separate from the public one (ELITEA_AUDIT_EXPORT_METRICS_ADDR).
//
//   - elitea_audit_export_delivered_total{sink,source}: events a receiver accepted.
//   - elitea_audit_export_failed_total{sink,kind}: failed deliveries of a
//     batch; kind is transient (retried) or permanent (a 400/413/422 answer).
//   - elitea_audit_export_dead_lettered_total{sink,source}: events given up on
//     after repeated permanent rejection. The event stays in its source table
//     and in elitea_runtime.audit_export_dead_letters. Alert on any increase.
//   - elitea_audit_export_backlog{stream}: events not yet delivered.
//   - elitea_audit_export_lag_seconds{stream}: age of the oldest undelivered event.
type Metrics struct {
	mu           sync.Mutex
	delivered    map[[2]string]int64
	failed       map[[2]string]int64
	deadLettered map[[2]string]int64
	backlog      map[string]int64
	lag          map[string]float64
}

// NewMetrics builds an empty registry.
func NewMetrics() *Metrics {
	return &Metrics{
		delivered:    map[[2]string]int64{},
		failed:       map[[2]string]int64{},
		deadLettered: map[[2]string]int64{},
		backlog:      map[string]int64{},
		lag:          map[string]float64{},
	}
}

// Register creates a sink's series at 0. A counter series that first appears
// at 1 makes increase() see no change, so the first dead letter, the case that
// matters, would never alert.
func (m *Metrics) Register(sink string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, source := range []Source{SourceSecurityAudit, SourceAdminAudit} {
		m.delivered[[2]string{sink, string(source)}] += 0
		m.deadLettered[[2]string{sink, string(source)}] += 0
	}
	for _, kind := range []string{"transient", "permanent"} {
		m.failed[[2]string{sink, kind}] += 0
	}
}

func (m *Metrics) addDelivered(sink string, events []Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range events {
		m.delivered[[2]string{sink, string(e.Source)}]++
	}
}

func (m *Metrics) addDeadLettered(sink string, events []Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range events {
		m.deadLettered[[2]string{sink, string(e.Source)}]++
	}
}

func (m *Metrics) addFailed(sink, kind string) {
	m.mu.Lock()
	m.failed[[2]string{sink, kind}]++
	m.mu.Unlock()
}

// SetStream records a stream's backlog and lag.
func (m *Metrics) SetStream(stream string, backlog int64, lagSeconds float64) {
	m.mu.Lock()
	m.backlog[stream] = backlog
	m.lag[stream] = lagSeconds
	m.mu.Unlock()
}

// Delivered reports one series (tests).
func (m *Metrics) Delivered(sink string, source Source) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.delivered[[2]string{sink, string(source)}]
}

// DeadLettered reports one series (tests).
func (m *Metrics) DeadLettered(sink string, source Source) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.deadLettered[[2]string{sink, string(source)}]
}

// Failed reports one series (tests).
func (m *Metrics) Failed(sink, kind string) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.failed[[2]string{sink, kind}]
}

// Handler serves GET /metrics.
func (m *Metrics) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_ = m.WriteText(w)
	})
}

// WriteText writes the exposition.
func (m *Metrics) WriteText(w io.Writer) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var b strings.Builder
	writePairs(&b, "elitea_audit_export_delivered_total", "Events a receiver accepted.", "sink", "source", m.delivered)
	writePairs(&b, "elitea_audit_export_failed_total", "Failed deliveries of a batch, by kind (transient or permanent).", "sink", "kind", m.failed)
	writePairs(&b, "elitea_audit_export_dead_lettered_total", "Events given up on after repeated permanent rejection by the receiver.", "sink", "source", m.deadLettered)
	fmt.Fprintf(&b, "# HELP elitea_audit_export_backlog Audit events not yet delivered, by stream.\n# TYPE elitea_audit_export_backlog gauge\n")
	for _, stream := range sortedKeys(m.backlog) {
		fmt.Fprintf(&b, "elitea_audit_export_backlog{stream=%q} %d\n", stream, m.backlog[stream])
	}
	fmt.Fprintf(&b, "# HELP elitea_audit_export_lag_seconds Age of the oldest undelivered audit event, by stream.\n# TYPE elitea_audit_export_lag_seconds gauge\n")
	for _, stream := range sortedKeys(m.lag) {
		fmt.Fprintf(&b, "elitea_audit_export_lag_seconds{stream=%q} %g\n", stream, m.lag[stream])
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func writePairs(b *strings.Builder, name, help, l1, l2 string, series map[[2]string]int64) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s counter\n", name, help, name)
	keys := make([][2]string, 0, len(series))
	for k := range series {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i][0] != keys[j][0] {
			return keys[i][0] < keys[j][0]
		}
		return keys[i][1] < keys[j][1]
	})
	for _, k := range keys {
		fmt.Fprintf(b, "%s{%s=%q,%s=%q} %d\n", name, l1, k[0], l2, k[1], series[k])
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
