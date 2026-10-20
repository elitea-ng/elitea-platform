package repos

// The egress proxy's per-turn summaries against a REAL migrated database
// (shared/0169). What the unit tests cannot show is the claim: that two Main
// replicas sweeping at once receive each row exactly once, that an upsert
// re-opens a claimed row, and that the table accepts what the proxy reports.

import (
	"context"
	"sync"
	"testing"
	"time"

	app "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/egressaudit"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/egressaudit/storetest"
)

func newEgressSummaries(t *testing.T) *EgressRunSummaries {
	t.Helper()
	pool := newMigratedPostgresIntegrationPool(t)
	if _, err := pool.Exec(t.Context(), `TRUNCATE elitea_runtime.egress_run_summaries`); err != nil {
		t.Fatalf("empty the table: %v", err)
	}
	return NewEgressRunSummaries(pool)
}

// The same cases as the in-memory store, against the table: two Main
// replicas writing one event per turn, a restart losing nothing, the hold,
// the sticky final, the resumed turn and the retention.
func TestEgressRunSummariesMeetTheStoreContract(t *testing.T) {
	storetest.Run(t, func(t *testing.T) app.Store { return newEgressSummaries(t) })
}

// Many Main replicas, many turns, sweeping at the same instant: every row
// reaches exactly one caller.
func TestEgressRunSummariesClaimIsExclusiveAcrossConcurrentSweeps(t *testing.T) {
	store := newEgressSummaries(t)
	ctx := context.Background()
	now := time.Unix(1_800_000_000, 0).UTC()
	const turns, replicas = 40, 3
	for i := range turns {
		for r := range replicas {
			report := app.Report{
				ReplicaID: string(rune('a' + r)), ExecutionID: "exec-" + string(rune('A'+i/26)) + string(rune('a'+i%26)),
				ProjectID: 7, Final: true, TokenExpiresAt: now.Add(time.Minute),
				HostsReached: []app.HostReached{{Host: "pypi.org", Port: 443, Connections: 1}},
			}
			if err := store.Upsert(ctx, report, now); err != nil {
				t.Fatalf("upsert: %v", err)
			}
		}
	}
	var (
		mu      sync.Mutex
		claimed = map[string]int{}
		wg      sync.WaitGroup
	)
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rows, err := store.ClaimDue(ctx, now.Add(time.Minute), 30*time.Second, 10*time.Minute)
			if err != nil {
				t.Errorf("claim: %v", err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for _, row := range rows {
				claimed[row.ExecutionID+"/"+row.ReplicaID]++
			}
		}()
	}
	wg.Wait()
	if len(claimed) != turns*replicas {
		t.Fatalf("claimed %d rows, want %d", len(claimed), turns*replicas)
	}
	for key, n := range claimed {
		if n != 1 {
			t.Fatalf("%s was claimed %d times", key, n)
		}
	}
}

// Every field the proxy reports comes back as it went in.
func TestEgressRunSummariesRoundTripEveryField(t *testing.T) {
	store := newEgressSummaries(t)
	ctx := context.Background()
	now := time.Unix(1_800_000_000, 0).UTC()
	in := app.Report{
		ReplicaID: "proxy-a", ExecutionID: "exec-1", ProjectID: 42, Final: true, TokenExpiresAt: now.Add(time.Minute),
		Connections: 5, BytesUp: 6, BytesDown: 7, OtherHosts: 2,
		HostsReached: []app.HostReached{{Host: "pypi.org", Port: 443, Connections: 3, BytesUp: 1, BytesDown: 2}},
		HostsRefused: []app.HostRefused{{Host: "evil.example", Port: 0, Reason: "host-not-allowed", Count: 4}},
	}
	if err := store.Upsert(ctx, in, now); err != nil {
		t.Fatal(err)
	}
	rows, err := store.ClaimDue(ctx, now.Add(time.Minute), 30*time.Second, 10*time.Minute)
	if err != nil || len(rows) != 1 {
		t.Fatalf("%v %v", rows, err)
	}
	got := rows[0]
	if got.ProjectID != 42 || !got.Final || !got.TokenExpiresAt.Equal(in.TokenExpiresAt) || got.Connections != 5 ||
		got.BytesUp != 6 || got.BytesDown != 7 || got.OtherHosts != 2 ||
		len(got.HostsReached) != 1 || got.HostsReached[0] != in.HostsReached[0] ||
		len(got.HostsRefused) != 1 || got.HostsRefused[0] != in.HostsRefused[0] ||
		!got.FirstReport.Equal(now) || !got.UpdatedAt.Equal(now) {
		t.Fatalf("round trip: %+v", got)
	}
	// A report with no hosts and no expiry is stored too (empty arrays, NULL).
	if err := store.Upsert(ctx, app.Report{ReplicaID: "proxy-b", ExecutionID: "exec-2", ProjectID: 1}, now); err != nil {
		t.Fatal(err)
	}
}
