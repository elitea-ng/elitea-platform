package control

import (
	"context"
	"errors"
	"reflect"
	"testing"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/browsersession"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
)

const testBrowserAudience = "dns:elitea-sandbox-browser"

type browserSessionsStub struct {
	calls   *[]string
	fences  []runtimedomain.Fence
	reports []BrowserSessionReport
	err     error
}

func (s *browserSessionsStub) RecordBrowserSession(_ context.Context, fence runtimedomain.Fence, report BrowserSessionReport) error {
	*s.calls = append(*s.calls, "record-browser")
	if s.err != nil {
		return s.err
	}
	s.fences = append(s.fences, fence)
	s.reports = append(s.reports, report)
	return nil
}

func browserSessionRequestFor(fence runtimedomain.Fence) *runtimev1.ReportBrowserSessionRequestV1 {
	return &runtimev1.ReportBrowserSessionRequestV1{
		Identity: identityForFence(fence), Fence: fenceProto(fence),
		Spec: &runtimev1.BrowserSessionSpecV1{
			Revision: 1, Name: browsersession.NameFor(fence.ExecutionID), Engine: runtimev1.BrowserEngineV1_BROWSER_ENGINE_V1_FULL,
			CpuMillis: 1000, MemoryBytes: 1 << 30, WallSeconds: 600, MaxPages: 3,
		},
		Audience: testBrowserAudience,
		State:    runtimev1.BrowserSessionReportStateV1_BROWSER_SESSION_REPORT_STATE_V1_LIVE,
	}
}

func TestReportBrowserSessionVerifiesTheClaimAndStores(t *testing.T) {
	calls := []string{}
	lease := validLease()
	store := &browserSessionsStub{calls: &calls}
	server := newSteeringTestServer(t, &calls, lease).WithBrowserSessions(store, []string{testBrowserAudience})
	request := browserSessionRequestFor(lease.Fence)
	request.State = runtimev1.BrowserSessionReportStateV1_BROWSER_SESSION_REPORT_STATE_V1_LINGERING
	request.LingerUntilUnixSeconds = 1_900_000_000
	response, err := server.ReportBrowserSession(context.Background(), request)
	if err != nil || response.GetRejection() != nil {
		t.Fatalf("err=%v rejection=%v", err, response.GetRejection())
	}
	if !reflect.DeepEqual(calls, []string{"authorize-peer", "verify-fence", "record-browser"}) {
		t.Fatalf("calls=%v", calls)
	}
	if len(store.fences) != 1 || store.fences[0] != lease.Fence {
		t.Fatalf("fences=%+v", store.fences)
	}
	got := store.reports[0]
	if got.SupervisorAudience != testBrowserAudience || got.State != browsersession.StateLingering ||
		got.LingerUntil.Unix() != 1_900_000_000 || got.Spec.Name != browsersession.NameFor(lease.Fence.ExecutionID) ||
		got.Spec.MemoryBytes != 1<<30 {
		t.Fatalf("report=%+v", got)
	}
}

func TestReportBrowserSessionRefusals(t *testing.T) {
	lease := validLease()
	stale := lease.Fence
	stale.Token[0] ^= 0xff
	mutated := func(mutate func(*runtimev1.ReportBrowserSessionRequestV1)) *runtimev1.ReportBrowserSessionRequestV1 {
		request := browserSessionRequestFor(lease.Fence)
		mutate(request)
		return request
	}
	denied := runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_AUTHORIZATION_FAILED
	violation := runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION
	for _, test := range []struct {
		name      string
		store     *browserSessionsStub
		request   *runtimev1.ReportBrowserSessionRequestV1
		code      runtimev1.RuntimeErrorCodeV1
		retryable bool
		verified  bool
	}{
		{name: "not composed", request: browserSessionRequestFor(lease.Fence), code: runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_UNSUPPORTED_CAPABILITY},
		{name: "malformed", store: &browserSessionsStub{}, request: &runtimev1.ReportBrowserSessionRequestV1{}, code: violation},
		{name: "an unconfigured supervisor", store: &browserSessionsStub{}, request: mutated(func(r *runtimev1.ReportBrowserSessionRequestV1) { r.Audience = "dns:attacker" }), code: denied},
		{name: "no state", store: &browserSessionsStub{}, request: mutated(func(r *runtimev1.ReportBrowserSessionRequestV1) { r.State = 0 }), code: violation},
		{name: "a malformed spec", store: &browserSessionsStub{}, request: mutated(func(r *runtimev1.ReportBrowserSessionRequestV1) { r.Spec.Revision = 2 }), code: violation},
		{name: "a lingering session without its linger end", store: &browserSessionsStub{}, request: mutated(func(r *runtimev1.ReportBrowserSessionRequestV1) {
			r.State = runtimev1.BrowserSessionReportStateV1_BROWSER_SESSION_REPORT_STATE_V1_LINGERING
			r.LingerUntilUnixSeconds = 0
		}), code: violation},
		{name: "a linger on a live session", store: &browserSessionsStub{}, request: mutated(func(r *runtimev1.ReportBrowserSessionRequestV1) { r.LingerUntilUnixSeconds = 5 }), code: violation},
		{name: "another run's session", store: &browserSessionsStub{}, request: mutated(func(r *runtimev1.ReportBrowserSessionRequestV1) {
			r.Spec.Name = browsersession.NameFor("some-other-execution")
		}), code: denied},
		{name: "stale fence", store: &browserSessionsStub{}, request: browserSessionRequestFor(stale), code: runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_STALE_FENCE},
		{name: "the row refuses", store: &browserSessionsStub{err: ErrBrowserSessionReportRefused}, request: browserSessionRequestFor(lease.Fence), code: denied, verified: true},
		{name: "store unavailable", store: &browserSessionsStub{err: errors.New("database down")}, request: browserSessionRequestFor(lease.Fence), code: runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_DEPENDENCY_UNAVAILABLE, retryable: true, verified: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := []string{}
			server := newSteeringTestServer(t, &calls, lease)
			if test.store != nil {
				test.store.calls = &calls
				server.WithBrowserSessions(test.store, []string{testBrowserAudience})
			}
			response, err := server.ReportBrowserSession(context.Background(), test.request)
			if err != nil {
				t.Fatal(err)
			}
			rejection := response.GetRejection()
			if rejection == nil || rejection.GetCode() != test.code || rejection.GetRetryable() != test.retryable {
				t.Fatalf("rejection=%v", rejection)
			}
			if test.store != nil && !test.verified && len(test.store.reports) != 0 {
				t.Fatalf("a refused report was stored: %+v", test.store.reports)
			}
		})
	}
}
