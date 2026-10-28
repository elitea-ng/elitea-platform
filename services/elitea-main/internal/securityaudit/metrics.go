package securityaudit

import (
	"context"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// metricHeadLockWait is the time an append waits for its chain's head row
// (#1480). The Prometheus exporter writes it as
// elitea_security_audit_head_lock_wait_seconds. A team's events form one
// chain, and every append holds that chain's head until its transaction
// commits, so one busy team's appends queue here: this histogram is the
// per-team ceiling showing. Labels are bounded: partition is "tenant" or
// "platform" and result "ok" or "error", never a tenant id.
const metricHeadLockWait = "elitea.security_audit.head_lock.wait"

// headLockBuckets cover a free row (sub-millisecond) to a chain that is
// saturated for seconds.
var headLockBuckets = []float64{0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5}

func newHeadLockWait(meter metric.Meter) metric.Float64Histogram {
	if meter == nil {
		meter = otel.Meter("elitea-main")
	}
	histogram, err := meter.Float64Histogram(metricHeadLockWait, metric.WithUnit("s"),
		metric.WithDescription("Time a security audit append waited to lock its chain head, by partition kind (tenant, platform) and result."),
		metric.WithExplicitBucketBoundaries(headLockBuckets...))
	if err != nil {
		// The instrument is evidence about latency, never a reason to
		// refuse an append: a meter that refuses it records nothing.
		return nil
	}
	return histogram
}

func recordHeadLockWait(ctx context.Context, histogram metric.Float64Histogram, tenantProjectID int64, err error, elapsed time.Duration) {
	if histogram == nil {
		return
	}
	partition, result := "tenant", "ok"
	if tenantProjectID == 0 {
		partition = "platform"
	}
	if err != nil {
		result = "error"
	}
	histogram.Record(context.WithoutCancel(ctx), elapsed.Seconds(), metric.WithAttributes(
		attribute.String("partition", partition), attribute.String("result", result)))
}
