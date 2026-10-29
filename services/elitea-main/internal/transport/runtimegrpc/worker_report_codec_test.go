package runtimegrpc

import (
	"bytes"
	"testing"

	"google.golang.org/protobuf/proto"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
)

// runStepReport builds a report whose encoding is close to size bytes.
func runStepReport(t *testing.T, size int) []byte {
	t.Helper()
	request := &runtimev1.ReportRunStepsRequestV1{}
	for remaining := size; remaining > 0; {
		chunk := min(remaining, 200*1024)
		request.Steps = append(request.Steps, &runtimev1.RunStepV1{
			StepId: "s", Outputs: &runtimev1.RunStepContentV1{Content: bytes.Repeat([]byte("x"), chunk)},
		})
		remaining -= chunk
	}
	raw, err := proto.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestRunStepReportBound(t *testing.T) {
	codec, _ := NewDirectionalStrictProtoCodec(64*1024, 64*1024)
	raw := runStepReport(t, RunStepReportMaxRequestBytes-1024)
	if len(raw) > RunStepReportMaxRequestBytes {
		t.Fatalf("fixture is %d bytes", len(raw))
	}
	if codec.Unmarshal(raw, new(runtimev1.ReportRunStepsRequestV1)) == nil {
		t.Fatal("a report above the listener bound was accepted before the report limits were set")
	}
	codec.WithWorkerReportRequestLimits()
	if err := codec.Unmarshal(raw, new(runtimev1.ReportRunStepsRequestV1)); err != nil {
		t.Fatalf("a ~1 MiB report was refused: %v", err)
	}
	if over := runStepReport(t, RunStepReportMaxRequestBytes+1); codec.Unmarshal(over, new(runtimev1.ReportRunStepsRequestV1)) == nil {
		t.Fatal("a report over 1 MiB was accepted")
	}
	if got := codec.largestReceive(); got != RunStepReportMaxRequestBytes {
		t.Fatalf("transport frame bound = %d", got)
	}
	// Other control requests keep the listener bound.
	ordinary, _ := proto.Marshal(&runtimev1.AuthorizeSandboxJobRequestV1{ActivationId: string(bytes.Repeat([]byte("x"), 80*1024))})
	if codec.Unmarshal(ordinary, new(runtimev1.AuthorizeSandboxJobRequestV1)) == nil {
		t.Fatal("ordinary control bound expanded")
	}
}

func TestEgressSummaryReportBound(t *testing.T) {
	codec, _ := NewDirectionalStrictProtoCodec(64*1024, 64*1024)
	codec.WithWorkerReportRequestLimits()
	summary := func(hostBytes int) []byte {
		request := &runtimev1.ReportEgressSummaryRequestV1{}
		host := string(bytes.Repeat([]byte("h"), 1000))
		for n := 0; n < hostBytes/1006; n++ {
			request.HostsReached = append(request.HostsReached, &runtimev1.EgressHostReachedV1{Host: host})
		}
		raw, _ := proto.Marshal(request)
		return raw
	}
	raw := summary(EgressSummaryReportMaxRequestBytes - 4096)
	if len(raw) <= 64*1024 || len(raw) > EgressSummaryReportMaxRequestBytes {
		t.Fatalf("fixture is %d bytes", len(raw))
	}
	if err := codec.Unmarshal(raw, new(runtimev1.ReportEgressSummaryRequestV1)); err != nil {
		t.Fatalf("a ~256 KiB egress summary was refused: %v", err)
	}
	if codec.Unmarshal(summary(EgressSummaryReportMaxRequestBytes+4096), new(runtimev1.ReportEgressSummaryRequestV1)) == nil {
		t.Fatal("an egress summary over 256 KiB was accepted")
	}
}
