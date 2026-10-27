package authzcompose

import (
	"context"
	"sort"
	"testing"
	"time"
)

// BenchmarkDecide runs the eight gates over warm in-memory readers. It also
// reports the p99 latency of one decision (issue acceptance: under 1 ms).
func BenchmarkDecide(b *testing.B) {
	d := newWorld().decider(b)
	r := permitRequest()
	ctx := context.Background()
	samples := make([]time.Duration, 0, min(b.N, 1_000_000))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		start := time.Now()
		decision := d.Decide(ctx, r)
		if len(samples) < cap(samples) {
			samples = append(samples, time.Since(start))
		}
		if !decision.Permitted() {
			b.Fatalf("decision denied: %v", decision.Reasons)
		}
	}
	b.StopTimer()
	if len(samples) == 0 {
		return
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	p99 := samples[len(samples)*99/100]
	b.ReportMetric(float64(p99.Nanoseconds()), "p99-ns")
}
