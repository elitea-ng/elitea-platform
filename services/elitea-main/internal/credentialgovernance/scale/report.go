package scale

import (
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
	"time"
)

// Summary is the latency distribution of one measurement.
type Summary struct {
	N                  int
	P50, P95, P99, Max time.Duration
}

// Summarize sorts a copy of samples and reads the nearest-rank
// percentiles.
func Summarize(samples []time.Duration) Summary {
	if len(samples) == 0 {
		return Summary{}
	}
	sorted := slices.Clone(samples)
	slices.Sort(sorted)
	rank := func(p float64) time.Duration {
		i := int(math.Ceil(p*float64(len(sorted)))) - 1
		return sorted[max(0, min(i, len(sorted)-1))]
	}
	return Summary{N: len(sorted), P50: rank(0.50), P95: rank(0.95), P99: rank(0.99), Max: sorted[len(sorted)-1]}
}

// Report is the stable, line-oriented scale report. Every line is
// `<kind> <name> key=value ...` with keys in a fixed order, so two runs diff
// line by line.
type Report struct {
	lines []string
}

// ReportSchema is the first line of every report.
const ReportSchema = "governed-scale-report v1"

// Info adds a line of key=value pairs in the given order.
func (r *Report) Info(kind, name string, pairs ...any) {
	var b strings.Builder
	b.WriteString(kind)
	b.WriteString(" ")
	b.WriteString(name)
	for i := 0; i+1 < len(pairs); i += 2 {
		fmt.Fprintf(&b, " %v=%v", pairs[i], pairs[i+1])
	}
	r.lines = append(r.lines, b.String())
}

// Latency adds a latency line in milliseconds with three decimals.
func (r *Report) Latency(name string, s Summary, extra ...any) {
	pairs := []any{"n", s.N, "p50_ms", ms(s.P50), "p95_ms", ms(s.P95), "p99_ms", ms(s.P99), "max_ms", ms(s.Max)}
	r.Info("latency", name, append(pairs, extra...)...)
}

func ms(d time.Duration) string { return fmt.Sprintf("%.3f", float64(d)/float64(time.Millisecond)) }

// WriteTo writes the report.
func (r *Report) WriteTo(w io.Writer) (int64, error) {
	var total int64
	for _, line := range append([]string{ReportSchema}, r.lines...) {
		n, err := io.WriteString(w, line+"\n")
		total += int64(n)
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// String is the report text.
func (r *Report) String() string {
	var b strings.Builder
	_, _ = r.WriteTo(&b)
	return b.String()
}
