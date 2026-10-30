package guardrail

import (
	"sort"
	"sync"
	"time"
)

// Outcomes of one detector run.
const (
	OutcomePass        = "pass"
	OutcomeFlagged     = "flagged"
	OutcomeMasked      = "masked"
	OutcomeBlocked     = "blocked"
	OutcomeErrorOpen   = "error_open"
	OutcomeErrorClosed = "error_closed"
)

// LabelDetectorError is the label of a verdict whose detector failed.
const LabelDetectorError = "detector_error"

// Verdict is the record of one detector on one request. It is the step record
// of ADR-0042 for a guardrail: it names the detector, the labels and the
// action, and has no field that the inspected content can reach.
type Verdict struct {
	Stage        string   `json:"stage"`
	Row          string   `json:"row"`
	Detector     string   `json:"detector"`
	DetectorType string   `json:"detector_type"`
	Action       string   `json:"action"`
	Outcome      string   `json:"outcome"`
	FailMode     string   `json:"fail_mode"`
	Labels       []string `json:"labels"`
	Findings     int      `json:"findings"`
	MaxScore     float64  `json:"max_score"`
	LatencyMS    int64    `json:"latency_ms"`
	// Runs counts the detector runs merged into this verdict: one for a
	// request body, one per evaluated window for a streamed answer.
	Runs int `json:"runs"`
}

func outcomeRank(o string) int {
	switch o {
	case OutcomeFlagged:
		return 1
	case OutcomeErrorOpen:
		return 2
	case OutcomeMasked:
		return 3
	case OutcomeBlocked:
		return 4
	case OutcomeErrorClosed:
		return 5
	}
	return 0
}

// maxVerdictLabels bounds the labels one merged verdict keeps.
const maxVerdictLabels = MaxLabels

// Collector merges the verdicts of one request: one verdict per (stage, row,
// detector), however many windows a stream evaluated. It is safe for
// concurrent use.
type Collector struct {
	mu    sync.Mutex
	order []string
	byKey map[string]*Verdict
}

// NewCollector returns an empty collector.
func NewCollector() *Collector { return &Collector{byKey: map[string]*Verdict{}} }

func (c *Collector) add(v Verdict, latency time.Duration) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key := v.Stage + "\x00" + v.Row + "\x00" + v.Detector
	cur, ok := c.byKey[key]
	if !ok {
		v.LatencyMS = latency.Milliseconds()
		v.Runs = 1
		v.Labels = boundLabels(dedupe(v.Labels))
		c.byKey[key] = &v
		c.order = append(c.order, key)
		return
	}
	cur.Runs++
	cur.LatencyMS += latency.Milliseconds()
	cur.Findings += v.Findings
	if v.MaxScore > cur.MaxScore {
		cur.MaxScore = v.MaxScore
	}
	if outcomeRank(v.Outcome) > outcomeRank(cur.Outcome) {
		cur.Outcome = v.Outcome
	}
	cur.Labels = boundLabels(dedupe(append(cur.Labels, v.Labels...)))
}

// Verdicts returns the merged verdicts in first-seen order.
func (c *Collector) Verdicts() []Verdict {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Verdict, 0, len(c.order))
	for _, k := range c.order {
		v := *c.byKey[k]
		v.Labels = append([]string(nil), v.Labels...)
		out = append(out, v)
	}
	return out
}

func dedupe(in []string) []string {
	if len(in) == 0 {
		return []string{}
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func boundLabels(in []string) []string {
	if len(in) > maxVerdictLabels {
		return in[:maxVerdictLabels]
	}
	return in
}
