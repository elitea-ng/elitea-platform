package shadow

import (
	"sync"
	"time"
)

type Metrics struct {
	mu      sync.RWMutex
	results []CompareResult
	maxSize int

	total      int64
	matches    int64
	statusOnly int64
	bodyOnly   int64
	errors     int64
	latencySum time.Duration
	legacySum  time.Duration
}

func NewMetrics(bufferSize int) *Metrics {
	return &Metrics{
		results: make([]CompareResult, 0, bufferSize),
		maxSize: bufferSize,
	}
}

func (m *Metrics) Record(r CompareResult) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.total++
	if r.StatusMatch && r.BodyMatch {
		m.matches++
	} else if r.StatusMatch && !r.BodyMatch {
		m.bodyOnly++
	} else if !r.StatusMatch && r.BodyMatch {
		m.statusOnly++
	}
	if r.Error != "" {
		m.errors++
	}
	m.latencySum += r.NewLatency
	m.legacySum += r.LegacyLatency

	if len(m.results) >= m.maxSize {
		m.results = m.results[1:]
	}
	m.results = append(m.results, r)
}

type Stats struct {
	Total            int64   `json:"total"`
	FullMatches      int64   `json:"full_matches"`
	StatusMismatches int64   `json:"status_mismatches"`
	BodyMismatches   int64   `json:"body_mismatches"`
	Errors           int64   `json:"errors"`
	MatchRate        float64 `json:"match_rate"`
	AvgNewLatencyMs  int64   `json:"avg_new_latency_ms"`
	AvgLegacyMs      int64   `json:"avg_legacy_latency_ms"`
}

func (m *Metrics) Stats() Stats {
	m.mu.RLock()
	defer m.mu.RUnlock()

	s := Stats{
		Total:            m.total,
		FullMatches:      m.matches,
		StatusMismatches: m.statusOnly,
		BodyMismatches:   m.bodyOnly,
		Errors:           m.errors,
	}
	if m.total > 0 {
		s.MatchRate = float64(m.matches) / float64(m.total)
		s.AvgNewLatencyMs = m.latencySum.Milliseconds() / m.total
		s.AvgLegacyMs = m.legacySum.Milliseconds() / m.total
	}
	return s
}

func (m *Metrics) Recent(limit int) []CompareResult {
	m.mu.RLock()
	defer m.mu.RUnlock()

	n := len(m.results)
	if limit > n {
		limit = n
	}
	out := make([]CompareResult, limit)
	copy(out, m.results[n-limit:])
	return out
}

func (m *Metrics) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.results = m.results[:0]
	m.total = 0
	m.matches = 0
	m.statusOnly = 0
	m.bodyOnly = 0
	m.errors = 0
	m.latencySum = 0
	m.legacySum = 0
}
