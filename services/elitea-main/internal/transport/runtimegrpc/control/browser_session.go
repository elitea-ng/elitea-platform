package control

import (
	"context"
	"errors"
	"log/slog"
	"time"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/browsersession"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
)

// BrowserSessionRecorder keeps the cloud browser session row (ADR-0049
// decision 6, #1597): repos.BrowserSessions in production.
type BrowserSessionRecorder interface {
	// RecordBrowserSession stores one report. The fence is verified live;
	// the run (tenant, project, owner, conversation) is read from the
	// execution kernel at the fence's generation, never from the worker.
	// ErrBrowserSessionReportRefused is a report that is not the run's.
	RecordBrowserSession(ctx context.Context, fence runtimedomain.Fence, report BrowserSessionReport) error
}

// BrowserSessionReport is a ReportBrowserSession request after its shape is
// checked.
type BrowserSessionReport struct {
	SupervisorAudience string
	Spec               browsersession.Spec
	State              browsersession.State
	LingerUntil        time.Time
}

// ErrBrowserSessionReportRefused is a report the recorder does not store.
var ErrBrowserSessionReportRefused = errors.New("browser session report refused")

// WithBrowserSessions serves ReportBrowserSession for the supervisors named
// by audiences (the browser profiles the view relay can reach). Nil or no
// audience answers UNSUPPORTED_CAPABILITY: the session then has no viewer.
func (s *Server) WithBrowserSessions(recorder BrowserSessionRecorder, audiences []string) *Server {
	if recorder == nil || len(audiences) == 0 {
		return s
	}
	allowed := make(map[string]struct{}, len(audiences))
	for _, audience := range audiences {
		allowed[audience] = struct{}{}
	}
	s.browserSessions = recorder
	s.browserAudiences = allowed
	return s
}

// ReportBrowserSession records that the claim started its run's cloud
// browser session, or that the run ended it. The peer is authenticated and
// the fence verified live, as ReportEgressSummary does. The session name
// must be the run's own, and the audience a configured browser supervisor:
// a worker cannot point a viewer at an address, or at another run's
// session.
func (s *Server) ReportBrowserSession(ctx context.Context, request *runtimev1.ReportBrowserSessionRequestV1) (*runtimev1.ReportBrowserSessionResponseV1, error) {
	if request == nil || request.GetIdentity() == nil || request.GetFence() == nil || request.GetSpec() == nil || hasUnknownFields(request.ProtoReflect()) {
		return browserSessionRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION, "The browser session report is malformed.", false), nil
	}
	if s.browserSessions == nil {
		return browserSessionRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_UNSUPPORTED_CAPABILITY, "Browser session reports are not served by this deployment.", false), nil
	}
	if _, ok := s.browserAudiences[request.GetAudience()]; !ok {
		return browserSessionRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_AUTHORIZATION_FAILED, "The browser supervisor is not configured.", false), nil
	}
	state, ok := browserReportState(request.GetState())
	if !ok {
		return browserSessionRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION, "The browser session state is malformed.", false), nil
	}
	wire := request.GetSpec()
	spec := browsersession.Spec{
		Revision: wire.GetRevision(), Name: wire.GetName(), Engine: int32(wire.GetEngine()),
		CPUMillis: wire.GetCpuMillis(), MemoryBytes: wire.GetMemoryBytes(), WallSeconds: wire.GetWallSeconds(), MaxPages: wire.GetMaxPages(),
	}
	if browsersession.ValidateSpec(spec) != nil {
		return browserSessionRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION, "The browser session spec is malformed.", false), nil
	}
	// A lingering session names when its linger ends, and only a lingering
	// one does: a LINGERING report without it would leave a row no bound
	// but the wall.
	var linger time.Time
	seconds := request.GetLingerUntilUnixSeconds()
	if (state == browsersession.StateLingering) != (seconds != 0) || seconds > 1<<40 {
		return browserSessionRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION, "The browser session linger is malformed.", false), nil
	}
	if seconds != 0 {
		linger = time.Unix(int64(seconds), 0).UTC() //nolint:gosec // bounded above
	}
	workloadIdentity, err := s.authorizer.AuthorizeWorkload(ctx, request.GetFence().GetWorkloadSessionId(), request.GetFence().GetProducerId())
	if err != nil || workloadIdentity == "" {
		return browserSessionRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_AUTHENTICATION_FAILED, "The workload session is not accepted.", false), nil
	}
	fence, err := fenceDomain(request.GetIdentity(), request.GetFence(), workloadIdentity)
	if err != nil {
		return browserSessionRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION, "The execution fence is malformed.", false), nil
	}
	if spec.Name != browsersession.NameFor(fence.ExecutionID) {
		return browserSessionRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_AUTHORIZATION_FAILED, "The browser session is not this run's.", false), nil
	}
	if err := s.claims.VerifyActive(ctx, fence); err != nil {
		code, message, retryable := safeRuntimeError(err)
		return browserSessionRejection(code, message, retryable), nil
	}
	if err := s.browserSessions.RecordBrowserSession(ctx, fence, BrowserSessionReport{
		SupervisorAudience: request.GetAudience(), Spec: spec, State: state, LingerUntil: linger,
	}); err != nil {
		if errors.Is(err, ErrBrowserSessionReportRefused) {
			return browserSessionRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_AUTHORIZATION_FAILED, "The browser session report does not match the run.", false), nil
		}
		slog.WarnContext(ctx, "browser session report not stored", "execution_id", fence.ExecutionID)
		return browserSessionRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_DEPENDENCY_UNAVAILABLE, "The browser session report could not be stored; retry.", true), nil
	}
	return &runtimev1.ReportBrowserSessionResponseV1{}, nil
}

func browserReportState(state runtimev1.BrowserSessionReportStateV1) (browsersession.State, bool) {
	switch state {
	case runtimev1.BrowserSessionReportStateV1_BROWSER_SESSION_REPORT_STATE_V1_LIVE:
		return browsersession.StateLive, true
	case runtimev1.BrowserSessionReportStateV1_BROWSER_SESSION_REPORT_STATE_V1_LINGERING:
		return browsersession.StateLingering, true
	case runtimev1.BrowserSessionReportStateV1_BROWSER_SESSION_REPORT_STATE_V1_ENDED:
		return browsersession.StateEnded, true
	default:
		return "", false
	}
}

func browserSessionRejection(code runtimev1.RuntimeErrorCodeV1, message string, retryable bool) *runtimev1.ReportBrowserSessionResponseV1 {
	return &runtimev1.ReportBrowserSessionResponseV1{Rejection: runtimeError(code, message, retryable)}
}
