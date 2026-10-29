package egressaudit

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	app "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/egressaudit"
)

type captured struct {
	reports []app.Report
	err     error
}

func (c *captured) Report(_ context.Context, r app.Report) error {
	if c.err != nil {
		return c.err
	}
	c.reports = append(c.reports, r)
	return nil
}

func peerContext(dnsNames ...string) context.Context {
	certificate := &x509.Certificate{DNSNames: dnsNames}
	return peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{
		State: tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{certificate}}},
	}})
}

func valid() *runtimev1.ReportRunSummaryRequestV1 {
	return &runtimev1.ReportRunSummaryRequestV1{
		ReplicaId: "elitea-egress-proxy-6d9f-abcde", ExecutionId: "exec-1", ProjectId: 7, Final: true,
		TokenExpiresAtUnix: 1_800_000_000, Connections: 2, BytesUp: 10, BytesDown: 20,
		HostsReached: []*runtimev1.EgressHostReachedV1{{Host: "pypi.org", Port: 443, Connections: 2, BytesUp: 10, BytesDown: 20}},
		HostsRefused: []*runtimev1.EgressHostRefusedV1{{Host: "evil.example", Port: 443, Reason: "sni-mismatch", Count: 1}},
	}
}

// Only the proxy's own workload identity may report.
func TestReportIsBoundToTheProxyIdentity(t *testing.T) {
	sink := &captured{}
	server, err := NewServer(sink, []string{"dns:elitea-egress-proxy"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.ReportRunSummary(context.Background(), valid()); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("no certificate: %v", err)
	}
	if _, err := server.ReportRunSummary(peerContext("elitea-agent-worker"), valid()); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("another workload: %v", err)
	}
	if _, err := server.ReportRunSummary(peerContext("elitea-egress-proxy"), valid()); err != nil {
		t.Fatal(err)
	}
	if len(sink.reports) != 1 {
		t.Fatalf("reports: %d", len(sink.reports))
	}
	got := sink.reports[0]
	if got.ExecutionID != "exec-1" || got.ProjectID != 7 || !got.Final || got.HostsReached[0].Port != 443 ||
		got.HostsRefused[0].Reason != "sni-mismatch" {
		t.Fatalf("%+v", got)
	}
}

func TestMalformedReportsAreRefused(t *testing.T) {
	sink := &captured{}
	server, _ := NewServer(sink, []string{"dns:elitea-egress-proxy"})
	ctx := peerContext("elitea-egress-proxy")
	for name, mutate := range map[string]func(*runtimev1.ReportRunSummaryRequestV1){
		"no replica":             func(r *runtimev1.ReportRunSummaryRequestV1) { r.ReplicaId = "" },
		"the worker's":           func(r *runtimev1.ReportRunSummaryRequestV1) { r.ReplicaId = "worker" },
		"a command's worker row": func(r *runtimev1.ReportRunSummaryRequestV1) { r.ReplicaId = "worker:cmd-1" },
		"bad execution":          func(r *runtimev1.ReportRunSummaryRequestV1) { r.ExecutionId = "a b" },
		"no project":             func(r *runtimev1.ReportRunSummaryRequestV1) { r.ProjectId = 0 },
		"negative bytes":         func(r *runtimev1.ReportRunSummaryRequestV1) { r.BytesUp = -1 },
		"too many hosts": func(r *runtimev1.ReportRunSummaryRequestV1) {
			for range 256 {
				r.HostsReached = append(r.HostsReached, &runtimev1.EgressHostReachedV1{Host: "x.example", Port: 443})
			}
		},
	} {
		request := valid()
		mutate(request)
		if _, err := server.ReportRunSummary(ctx, request); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
	if len(sink.reports) != 0 {
		t.Fatal("a refused report reached the merger")
	}
	// A junk host entry is counted, and the rest of the report lands.
	request := valid()
	request.HostsReached = append(request.HostsReached, &runtimev1.EgressHostReachedV1{Host: "a b", Port: 443},
		&runtimev1.EgressHostReachedV1{Host: "x.example", Port: 0})
	request.HostsRefused = append(request.HostsRefused, &runtimev1.EgressHostRefusedV1{Host: strings.Repeat("a", 300), Reason: "bad-target"})
	if _, err := server.ReportRunSummary(ctx, request); err != nil {
		t.Fatal(err)
	}
	got := sink.reports[0]
	if len(got.HostsReached) != 1 || len(got.HostsRefused) != 1 || got.OtherHosts != 3 {
		t.Fatalf("%+v", got)
	}
	if _, err := NewServer(sink, []string{"elitea-egress-proxy"}); err == nil {
		t.Fatal("an identity without dns: or spiffe:// is refused")
	}
	if _, err := NewServer(sink, nil); err == nil {
		t.Fatal("no identity is refused")
	}
}

// A report the store could not keep is answered UNAVAILABLE, so the proxy
// retries it instead of treating it as delivered.
func TestReportThatCannotBeStoredIsRetryable(t *testing.T) {
	sink := &captured{err: errors.New("database down")}
	server, err := NewServer(sink, []string{"dns:elitea-egress-proxy"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.ReportRunSummary(peerContext("elitea-egress-proxy"), valid()); status.Code(err) != codes.Unavailable {
		t.Fatalf("a store failure: %v", err)
	}
}
