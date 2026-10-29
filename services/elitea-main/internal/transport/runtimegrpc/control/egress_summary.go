package control

import (
	"context"
	"log/slog"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	egressauditapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/egressaudit"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/transport/runtimegrpc/egressaudit"
)

// EgressSummaries keeps the worker's egress reports (ADR-0044 decision 2) as
// one `worker:<command id>` row per claimed command in
// elitea_runtime.egress_run_summaries, beside the egress proxy's rows (the
// composition's adapter over egressaudit.Merger). The command is the fence's,
// which VerifyActive (and the settlement's own lease check) bound to the
// job's current command: a resumed run is a new command, and its report never
// replaces the earlier command's row.
type EgressSummaries interface {
	// RecordWorkerEgress replaces the claimed command's worker row with this
	// snapshot.
	// The project is the one Main admitted the claimed generation in, read
	// from the execution kernel, never one the worker names.
	RecordWorkerEgress(ctx context.Context, fence runtimedomain.Fence, summary egressauditapp.WorkerSummary) error
	// SettleWorkerEgress makes the settled command's worker row final.
	SettleWorkerEgress(ctx context.Context, fence runtimedomain.Fence) error
}

// WithEgressSummaries serves ReportEgressSummary and marks the worker row
// final when the run's settlement is prepared. Nil answers
// UNSUPPORTED_CAPABILITY, and the worker keeps its summary in its own log.
func (s *Server) WithEgressSummaries(summaries EgressSummaries) *Server {
	s.egressSummaries = summaries
	return s
}

// ReportEgressSummary records the public hosts the worker's own HTTP clients
// reached and refused for one claimed execution. The peer is authenticated
// and the fence verified live, exactly as the steering claim does; a report
// for a released or stale claim is refused. A report is a cumulative
// snapshot, so it replaces the previous one. A junk host entry is counted in
// other_hosts, not stored.
func (s *Server) ReportEgressSummary(ctx context.Context, request *runtimev1.ReportEgressSummaryRequestV1) (*runtimev1.ReportEgressSummaryResponseV1, error) {
	if request == nil || request.GetIdentity() == nil || request.GetFence() == nil || hasUnknownFields(request.ProtoReflect()) {
		return egressSummaryRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION, "The egress summary request is malformed.", false), nil
	}
	if len(request.GetHostsReached()) > egressaudit.MaxHosts || len(request.GetHostsRefused()) > egressaudit.MaxHosts || request.GetOtherHosts() < 0 {
		return egressSummaryRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION, "The egress summary is over its bounds.", false), nil
	}
	if s.egressSummaries == nil {
		return egressSummaryRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_UNSUPPORTED_CAPABILITY, "Egress summaries are not served by this deployment.", false), nil
	}
	workloadIdentity, err := s.authorizer.AuthorizeWorkload(ctx, request.GetFence().GetWorkloadSessionId(), request.GetFence().GetProducerId())
	if err != nil || workloadIdentity == "" {
		return egressSummaryRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_AUTHENTICATION_FAILED, "The workload session is not accepted.", false), nil
	}
	fence, err := fenceDomain(request.GetIdentity(), request.GetFence(), workloadIdentity)
	if err != nil {
		return egressSummaryRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION, "The execution fence is malformed.", false), nil
	}
	if err := s.claims.VerifyActive(ctx, fence); err != nil {
		code, message, retryable := safeRuntimeError(err)
		return egressSummaryRejection(code, message, retryable), nil
	}
	reached, refused, junk := egressaudit.Hosts(request.GetHostsReached(), request.GetHostsRefused())
	if err := s.egressSummaries.RecordWorkerEgress(ctx, fence, egressauditapp.WorkerSummary{
		ExecutionID: fence.ExecutionID, CommandID: fence.CommandID, HostsReached: reached, HostsRefused: refused,
		OtherHosts: request.GetOtherHosts() + junk,
	}); err != nil {
		slog.WarnContext(ctx, "worker egress summary not stored", "execution_id", fence.ExecutionID)
		return egressSummaryRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_DEPENDENCY_UNAVAILABLE, "The egress summary could not be stored; retry.", true), nil
	}
	return &runtimev1.ReportEgressSummaryResponseV1{}, nil
}

// settleWorkerEgress is best effort: a settlement never fails because its
// egress row could not be marked (the row is still written after its hold).
func (s *Server) settleWorkerEgress(ctx context.Context, fence runtimedomain.Fence) {
	if s.egressSummaries == nil {
		return
	}
	if err := s.egressSummaries.SettleWorkerEgress(ctx, fence); err != nil {
		slog.WarnContext(ctx, "worker egress summary not marked final", "execution_id", fence.ExecutionID)
	}
}

func egressSummaryRejection(code runtimev1.RuntimeErrorCodeV1, message string, retryable bool) *runtimev1.ReportEgressSummaryResponseV1 {
	return &runtimev1.ReportEgressSummaryResponseV1{Rejection: runtimeError(code, message, retryable)}
}
