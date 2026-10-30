package breaker

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/safety"
)

// Metrics are the runtime safety figures, in the Prometheus text format
// (elitea-main has no metrics library; the audit export writes its own
// exposition the same way). They are served on the main metrics listener
// (ELITEA_AUDIT_EXPORT_METRICS_ADDR) and alerted on by the chart's
// PrometheusRule. No series carries a project or an agent: the labels are
// bounded enums.
//
//   - elitea_safety_suspensions_active{scope,source}: active suspensions,
//     read from this replica's admission snapshot at scrape time. Every
//     replica reports the same platform-wide count; alert on max().
//   - elitea_breaker_trips_total{rule}: suspensions this replica's breaker
//     pass wrote.
//   - elitea_breaker_storms_total: passes in which more agents would have
//     tripped on error_rate or tool_failure_streak than the storm limit, so
//     none was tripped.
//   - elitea_breaker_pass_failures_total: breaker passes that ended in an
//     error.
type Metrics struct {
	mu           sync.Mutex
	trips        map[Rule]int64
	storms       int64
	passFailures int64
	active       func(context.Context) ([]safety.Suspension, error)
	lastActive   map[[2]string]int64
}

var (
	allScopes  = []safety.Scope{safety.ScopePlatform, safety.ScopeProject, safety.ScopeAgent, safety.ScopeToolkitType}
	allSources = []safety.Source{safety.SourceManual, safety.SourceBreaker, safety.SourceCertification, safety.SourceSelfTest}
)

// NewMetrics builds the registry with every series at 0: a counter series
// that first appears at 1 makes increase() see no change.
func NewMetrics() *Metrics {
	m := &Metrics{trips: map[Rule]int64{}, lastActive: map[[2]string]int64{}}
	for _, rule := range AllRules {
		m.trips[rule] = 0
	}
	for _, scope := range allScopes {
		for _, source := range allSources {
			m.lastActive[[2]string{string(scope), string(source)}] = 0
		}
	}
	return m
}

// SetActiveSource names the reader of the active suspensions (the kill
// switch gate's snapshot). Until it is set, the gauge reports 0.
func (m *Metrics) SetActiveSource(read func(context.Context) ([]safety.Suspension, error)) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.active = read
	m.mu.Unlock()
}

// AddTrip counts one trip.
func (m *Metrics) AddTrip(rule Rule) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.trips[rule]++
	m.mu.Unlock()
}

// AddStorm counts one storm.
func (m *Metrics) AddStorm() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.storms++
	m.mu.Unlock()
}

// AddPassFailure counts one failed pass.
func (m *Metrics) AddPassFailure() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.passFailures++
	m.mu.Unlock()
}

// Trips reports one rule's count (tests).
func (m *Metrics) Trips(rule Rule) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.trips[rule]
}

// Storms reports the storm count (tests).
func (m *Metrics) Storms() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.storms
}

// refreshActive reads the active suspensions. A failed read keeps the last
// counts: a database blip must not read as "every suspension was resumed".
func (m *Metrics) refreshActive(ctx context.Context) {
	m.mu.Lock()
	read := m.active
	m.mu.Unlock()
	if read == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	rows, err := read(ctx)
	if err != nil {
		return
	}
	counts := map[[2]string]int64{}
	for _, scope := range allScopes {
		for _, source := range allSources {
			counts[[2]string{string(scope), string(source)}] = 0
		}
	}
	for _, row := range rows {
		if row.Active() {
			counts[[2]string{string(row.Target.Scope), string(row.Source)}]++
		}
	}
	m.mu.Lock()
	m.lastActive = counts
	m.mu.Unlock()
}

// Handler serves GET /metrics.
func (m *Metrics) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_ = m.WriteTextContext(r.Context(), w)
	})
}

// WriteText writes the exposition (the audit export's listener calls it
// after its own figures).
func (m *Metrics) WriteText(w io.Writer) error {
	return m.WriteTextContext(context.Background(), w)
}

// WriteTextContext writes the exposition, reading the active suspensions
// first.
func (m *Metrics) WriteTextContext(ctx context.Context, w io.Writer) error {
	if m == nil {
		return nil
	}
	m.refreshActive(ctx)
	m.mu.Lock()
	defer m.mu.Unlock()
	var b strings.Builder
	b.WriteString("# HELP elitea_safety_suspensions_active Active execution suspensions, by scope and source.\n")
	b.WriteString("# TYPE elitea_safety_suspensions_active gauge\n")
	keys := make([][2]string, 0, len(m.lastActive))
	for key := range m.lastActive {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i][0] != keys[j][0] {
			return keys[i][0] < keys[j][0]
		}
		return keys[i][1] < keys[j][1]
	})
	for _, key := range keys {
		fmt.Fprintf(&b, "elitea_safety_suspensions_active{scope=%q,source=%q} %d\n", key[0], key[1], m.lastActive[key])
	}
	b.WriteString("# HELP elitea_breaker_trips_total Agent suspensions the circuit breaker wrote, by rule.\n")
	b.WriteString("# TYPE elitea_breaker_trips_total counter\n")
	rules := make([]string, 0, len(m.trips))
	for rule := range m.trips {
		rules = append(rules, string(rule))
	}
	sort.Strings(rules)
	for _, rule := range rules {
		fmt.Fprintf(&b, "elitea_breaker_trips_total{rule=%q} %d\n", rule, m.trips[Rule(rule)])
	}
	b.WriteString("# HELP elitea_breaker_storms_total Breaker passes that tripped no agent because more than the storm limit would have tripped.\n")
	b.WriteString("# TYPE elitea_breaker_storms_total counter\n")
	fmt.Fprintf(&b, "elitea_breaker_storms_total %d\n", m.storms)
	b.WriteString("# HELP elitea_breaker_pass_failures_total Breaker passes that ended in an error.\n")
	b.WriteString("# TYPE elitea_breaker_pass_failures_total counter\n")
	fmt.Fprintf(&b, "elitea_breaker_pass_failures_total %d\n", m.passFailures)
	_, err := io.WriteString(w, b.String())
	return err
}
