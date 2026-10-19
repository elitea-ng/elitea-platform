//go:build loadgate

package repos

// Budget gate (run on a quiet machine): go test -tags loadgate -run TestHTTPActionMainOverheadBudgetPostgres
// with ELITEA_TEST_DATABASE_URL set. It is build-tagged like the workspace
// store load gate because its p95 depends on the host, not on the code alone.

import (
	"net/http"
	"sort"
	"strconv"
	"testing"
	"time"
)

// Budget: Main overhead per action (route, admission, rule, credential,
// Lookup/Begin/Commit; the local remote answers immediately) p95 ≤ 15 ms over
// 1,000 actions on real PostgreSQL.
func TestHTTPActionMainOverheadBudgetPostgres(t *testing.T) {
	if testing.Short() {
		t.Skip("budget run")
	}
	w := newHTTPServiceWorld(t)
	const runs = 1000
	durations := make([]time.Duration, 0, runs)
	for i := range runs {
		body := w.invocation(uint64(i+1), "item-"+strconv.Itoa(i))
		started := time.Now()
		response := w.post(httpWorldClaim, 0x11, body)
		durations = append(durations, time.Since(started))
		if response.Code != http.StatusOK {
			t.Fatalf("run %d: %d %s", i, response.Code, response.Body)
		}
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	p50, p95 := durations[runs/2], durations[runs*95/100]
	t.Logf("HTTP action Main overhead over %d runs: p50=%s p95=%s max=%s", runs, p50, p95, durations[runs-1])
	if p95 > 15*time.Millisecond {
		t.Fatalf("p95 %s exceeds the 15 ms budget", p95)
	}
	if w.remoteCalls.Load() != runs {
		t.Fatalf("remote calls %d", w.remoteCalls.Load())
	}
}
