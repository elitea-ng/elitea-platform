package securityaudit

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// #1480: every append times its chain-head lock, labelled only by the
// partition's kind and the result, never by a tenant id.
func TestAppendRecordsTheHeadLockWait(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	meter := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test")
	appender := newTestAppender(t).WithMeter(meter)

	if err := appender.Append(context.Background(), &fakeTx{headHash: ZeroHash[:], moved: 1}, sampleEvent()); err != nil {
		t.Fatal(err)
	}
	platform := sampleEvent()
	platform.TenantProjectID = 0
	if err := appender.Append(context.Background(), &fakeTx{headHash: ZeroHash[:], moved: 1}, platform); err != nil {
		t.Fatal(err)
	}
	if err := appender.Append(context.Background(), &fakeTx{headErr: errors.New("lock timeout")}, sampleEvent()); err == nil {
		t.Fatal("a failed head lock appended")
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	counts := map[string]uint64{}
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != metricHeadLockWait {
				continue
			}
			if m.Unit != "s" {
				t.Fatalf("unit %q", m.Unit)
			}
			histogram, ok := m.Data.(metricdata.Histogram[float64])
			if !ok {
				t.Fatalf("%s is %T", m.Name, m.Data)
			}
			for _, point := range histogram.DataPoints {
				if point.Attributes.Len() != 2 {
					t.Fatalf("labels %v: only partition and result", point.Attributes.ToSlice())
				}
				partition, _ := point.Attributes.Value(attribute.Key("partition"))
				result, _ := point.Attributes.Value(attribute.Key("result"))
				counts[partition.AsString()+"/"+result.AsString()] += point.Count
			}
		}
	}
	want := map[string]uint64{"tenant/ok": 1, "platform/ok": 1, "tenant/error": 1}
	if len(counts) != len(want) {
		t.Fatalf("head lock samples %v, want %v", counts, want)
	}
	for key, n := range want {
		if counts[key] != n {
			t.Fatalf("head lock samples %v, want %v", counts, want)
		}
	}
}
