package proxy

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/libs/go/egresslib/runtoken"
)

// Every logged summary also reaches the sink (elitea-main's audit reporter):
// periodic, final, and the shutdown sweep's replica_shutdown.
func TestRunSummariesReachTheSink(t *testing.T) {
	rs := NewRuns(4, 1<<20, slog.New(slog.NewTextHandler(io.Discard, nil)))
	var got []Summary
	rs.SetSink(func(s Summary) { got = append(got, s) })
	now := time.Unix(1_800_000_000, 0)
	claims := runtoken.Claims{ExecutionID: "exec-1", ProjectID: 7, ExpiresAt: now.Add(time.Minute)}
	rs.refused(claims, hostPort{host: "evil.example", port: 443}, ReasonSNIMismatch)

	rs.Sweep(now, time.Second, false)
	if len(got) != 1 || got[0].Final || got[0].HostsRefused[0].Reason != ReasonSNIMismatch {
		t.Fatalf("periodic: %+v", got)
	}
	rs.refused(claims, hostPort{host: "evil.example", port: 443}, ReasonSNIMismatch)
	rs.Sweep(now.Add(time.Second), time.Second, true)
	if len(got) != 2 || !got[1].ReplicaShutdown || got[1].HostsRefused[0].Count != 2 {
		t.Fatalf("shutdown: %+v", got)
	}
	rs.Sweep(now.Add(2*time.Minute), time.Second, false)
	if len(got) != 3 || !got[2].Final || rs.Active() != 0 {
		t.Fatalf("final: %+v", got)
	}
}
