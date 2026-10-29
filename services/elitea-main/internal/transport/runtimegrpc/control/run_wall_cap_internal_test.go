package control

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.opentelemetry.io/otel/metric/noop"

	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
)

type fakeWallCap struct {
	cancel bool
	err    error
	calls  int
	limit  time.Duration
}

func (f *fakeWallCap) EnforceWallCap(_ context.Context, _ string, _ uint64, limit time.Duration) (bool, error) {
	f.calls++
	f.limit = limit
	return f.cancel, f.err
}

func newWallCap(limit time.Duration, limitErr error, enforcer *fakeWallCap) *runWallCap {
	counter, _ := noop.NewMeterProvider().Meter("t").Int64Counter("c")
	return &runWallCap{
		limit:    func(context.Context) (time.Duration, error) { return limit, limitErr },
		enforcer: enforcer,
		reached:  counter,
	}
}

func TestRunWallCapDesiredState(t *testing.T) {
	t.Parallel()
	fence := runtimedomain.Fence{ExecutionID: "exec-1", Generation: 1}
	agent := executiondomain.AgentApplicationCapability
	running := runtimedomain.DesiredRunning

	var nilCap *runWallCap
	if got := nilCap.desiredState(context.Background(), fence, agent, running); got != running {
		t.Fatalf("no cap configured = %v", got)
	}

	for _, test := range []struct {
		name       string
		limit      time.Duration
		limitErr   error
		enforcer   fakeWallCap
		capability string
		desired    runtimedomain.DesiredState
		want       runtimedomain.DesiredState
		wantCalls  int
	}{
		{"past the cap cancels", time.Hour, nil, fakeWallCap{cancel: true}, agent, running, runtimedomain.DesiredCancelled, 1},
		{"inside the cap runs on", time.Hour, nil, fakeWallCap{}, agent, running, running, 1},
		{"cap off runs no statement", 0, nil, fakeWallCap{cancel: true}, agent, running, running, 0},
		{"unreadable cap keeps the answer", 0, errors.New("down"), fakeWallCap{cancel: true}, agent, running, running, 0},
		{"failed write keeps the answer", time.Hour, nil, fakeWallCap{err: errors.New("down")}, agent, running, running, 1},
		{"a toolkit run is not capped", time.Hour, nil, fakeWallCap{cancel: true}, "toolkit.call_tool.v1", running, running, 0},
		{"already cancelled is untouched", time.Hour, nil, fakeWallCap{cancel: true}, agent, runtimedomain.DesiredCancelled, runtimedomain.DesiredCancelled, 0},
	} {
		enforcer := test.enforcer
		got := newWallCap(test.limit, test.limitErr, &enforcer).desiredState(context.Background(), fence, test.capability, test.desired)
		if got != test.want || enforcer.calls != test.wantCalls {
			t.Errorf("%s: desired %v after %d calls, want %v after %d", test.name, got, enforcer.calls, test.want, test.wantCalls)
		}
		if test.wantCalls == 1 && enforcer.limit != test.limit {
			t.Errorf("%s: enforced %s, want %s", test.name, enforcer.limit, test.limit)
		}
	}
}
