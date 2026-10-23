// Package egressaudit serves elitea.runtime.v1.EgressAuditService on
// elitea-main's private mTLS control listener (ADR-0034 decision 9): the
// egress proxy reports its run summaries, and Main merges them into one
// audit event per turn (application/egressaudit).
//
// The service answers only the proxy's configured client certificate
// identities (ELITEA_EGRESS_AUDIT_CLIENTS, "dns:<name>" or a spiffe:// URI),
// the same binding to a workload identity elitea-vector's introspection
// uses. The control listener already requires a certificate from the
// runtime CA; this narrows it to the proxy.
package egressaudit

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	app "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/egressaudit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/transport/grpcauth"
)

// Bounds of one report: the proxy holds at most 256 hosts per run.
const (
	maxHosts       = 256
	maxHostBytes   = 253 + 2 // a bracketed IPv6 literal fits
	maxIDBytes     = 128
	maxReasonBytes = 64
)

// Reporter stores validated reports (*app.Merger). An error means the report
// was not stored; the service answers UNAVAILABLE and the proxy retries.
type Reporter interface {
	Report(context.Context, app.Report) error
}

// Server is the audit service.
type Server struct {
	runtimev1.UnimplementedEgressAuditServiceServer

	reporter Reporter
	clients  grpcauth.Allowlist
}

// NewServer answers only the identities in clients.
func NewServer(reporter Reporter, clients []string) (*Server, error) {
	if reporter == nil {
		return nil, errors.New("egress audit requires a reporter")
	}
	allowed, err := grpcauth.NewAllowlist(clients)
	if err != nil {
		return nil, fmt.Errorf("egress audit clients: %w", err)
	}
	return &Server{reporter: reporter, clients: allowed}, nil
}

// ReportRunSummary records one replica's summary of one run.
func (s *Server) ReportRunSummary(ctx context.Context, request *runtimev1.ReportRunSummaryRequestV1) (*runtimev1.ReportRunSummaryResponseV1, error) {
	if _, err := s.clients.Authorize(ctx); err != nil {
		return nil, err
	}
	report, err := toReport(request)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := s.reporter.Report(ctx, report); err != nil {
		return nil, status.Error(codes.Unavailable, "The summary could not be stored; retry.")
	}
	return &runtimev1.ReportRunSummaryResponseV1{}, nil
}

func validID(id string) bool {
	if id == "" || len(id) > maxIDBytes {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == ':' || c == '-'
		if !ok {
			return false
		}
	}
	return true
}

func validText(text string, limit int) bool {
	if text == "" || len(text) > limit {
		return false
	}
	for i := 0; i < len(text); i++ {
		if text[i] <= ' ' || text[i] >= 0x7f {
			return false
		}
	}
	return true
}

func toReport(r *runtimev1.ReportRunSummaryRequestV1) (app.Report, error) {
	switch {
	case !validID(r.GetReplicaId()):
		return app.Report{}, errors.New("the replica id is malformed")
	case !validID(r.GetExecutionId()):
		return app.Report{}, errors.New("the execution id is malformed")
	case r.GetProjectId() <= 0:
		return app.Report{}, errors.New("the project id is malformed")
	case len(r.GetHostsReached())+len(r.GetHostsRefused()) > maxHosts:
		return app.Report{}, errors.New("the report names too many hosts")
	case r.GetConnections() < 0 || r.GetBytesUp() < 0 || r.GetBytesDown() < 0 || r.GetOtherHosts() < 0:
		return app.Report{}, errors.New("the report has a negative count")
	}
	report := app.Report{
		ReplicaID: r.GetReplicaId(), ExecutionID: r.GetExecutionId(), ProjectID: r.GetProjectId(),
		Final: r.GetFinal(), ReplicaShutdown: r.GetReplicaShutdown(),
		TokenExpiresAt: time.Unix(r.GetTokenExpiresAtUnix(), 0),
		Connections:    r.GetConnections(), BytesUp: r.GetBytesUp(), BytesDown: r.GetBytesDown(), OtherHosts: r.GetOtherHosts(),
	}
	// A host entry that does not hold up (the proxy records what a sandbox
	// asked for, which may be junk) is counted, not trusted: the rest of the
	// report still lands.
	for _, h := range r.GetHostsReached() {
		if !validText(h.GetHost(), maxHostBytes) || h.GetPort() == 0 || h.GetPort() > 65535 ||
			h.GetConnections() < 0 || h.GetBytesUp() < 0 || h.GetBytesDown() < 0 {
			report.OtherHosts++
			continue
		}
		report.HostsReached = append(report.HostsReached, app.HostReached{Host: h.GetHost(), Port: int(h.GetPort()),
			Connections: h.GetConnections(), BytesUp: h.GetBytesUp(), BytesDown: h.GetBytesDown()})
	}
	for _, h := range r.GetHostsRefused() {
		// A refusal can name a host the proxy could not parse; its port may
		// be 0.
		if !validText(h.GetHost(), maxHostBytes) || h.GetPort() > 65535 || !validText(h.GetReason(), maxReasonBytes) || h.GetCount() < 0 {
			report.OtherHosts++
			continue
		}
		report.HostsRefused = append(report.HostsRefused, app.HostRefused{Host: h.GetHost(), Port: int(h.GetPort()),
			Reason: h.GetReason(), Count: h.GetCount()})
	}
	return report, nil
}
