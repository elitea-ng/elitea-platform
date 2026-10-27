package credentialbroker

import (
	"context"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Instrument names. The Prometheus exporter writes the counter as
// elitea_credential_redeem_total and the histogram as
// elitea_credential_redeem_duration_seconds. Labels are result, kind and
// reason only, every one a bounded code: never a tenant, user, execution or
// credential id.
const (
	metricRedeemTotal    = "elitea.credential.redeem"
	metricRedeemDuration = "elitea.credential.redeem.duration"
)

type metrics struct {
	total    metric.Int64Counter
	duration metric.Float64Histogram
}

func newMetrics(meter metric.Meter) (metrics, error) {
	if meter == nil {
		meter = otel.Meter("elitea-main")
	}
	total, err := meter.Int64Counter(metricRedeemTotal,
		metric.WithDescription("Governed credential redemptions, by result (permit, deny, error), envelope kind and reason code."))
	if err != nil {
		return metrics{}, err
	}
	duration, err := meter.Float64Histogram(metricRedeemDuration, metric.WithUnit("s"),
		metric.WithDescription("Governed credential redemption latency, by result."))
	if err != nil {
		return metrics{}, err
	}
	return metrics{total: total, duration: duration}, nil
}

func (m metrics) record(ctx context.Context, result Result, err error, elapsed time.Duration) {
	outcome, kind, reason := "error", "none", "internal"
	switch {
	case err != nil:
	case result.Envelope != nil:
		outcome, kind, reason = "permit", string(result.Envelope.Kind), "ok"
	case result.Denial != nil:
		outcome, reason = "deny", result.Denial.reason
		if reason == "" {
			reason = string(result.Denial.Code)
		}
	}
	// A context without a deadline still records: the metric is about the
	// redemption, not the caller.
	ctx = context.WithoutCancel(ctx)
	m.total.Add(ctx, 1, metric.WithAttributes(
		attribute.String("result", outcome), attribute.String("kind", kind), attribute.String("reason", reason)))
	m.duration.Record(ctx, elapsed.Seconds(), metric.WithAttributes(attribute.String("result", outcome)))
}
