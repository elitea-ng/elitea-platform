package adminaccess_test

// The elevation gauge on the real ledger (ADR-0059 R2.10a, issue 2524): it is
// computed from PostgreSQL at scrape time, so an elevation that ran out reads
// as inactive with no reconciler pass, and two replicas report one value.

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
)

func gaugeAt(t *testing.T, metrics *adminaccess.BreakGlassMetrics) int64 {
	t.Helper()
	active, err := metrics.ActiveElevations(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	if err := metrics.WriteTextContext(t.Context(), &b); err != nil {
		t.Fatal(err)
	}
	if want := adminaccess.MetricElevationsActive + " " + strconv.FormatInt(active, 10) + "\n"; !strings.Contains(b.String(), want) {
		t.Fatalf("the exposition does not carry %q:\n%s", want, b.String())
	}
	return active
}

// 1 with one active elevation, 0 after its end; and, for a second one, 0
// one second after its expiry with no reconciler pass.
func TestTheElevationGaugeIsComputedFromTheLedger(t *testing.T) {
	pool := newElevationDatabase(t)
	clock := elevationClock.Add(time.Minute)
	metrics := adminaccess.NewBreakGlassMetrics(pool).WithClock(func() time.Time { return clock })
	if got := gaugeAt(t, metrics); got != 0 {
		t.Fatalf("an empty ledger: %d, want 0", got)
	}

	ended := startElevation(t, pool, seededStart(t, pool))
	if got := gaugeAt(t, metrics); got != 1 {
		t.Fatalf("one active elevation: %d, want 1", got)
	}
	if _, changed := endElevation(t, pool, adminaccess.ElevationEnd{
		ID: ended.ID, At: clock, Reason: adminaccess.ElevationEndHolder,
	}); !changed {
		t.Fatal("the end changed nothing")
	}
	if got := gaugeAt(t, metrics); got != 0 {
		t.Fatalf("after its end: %d, want 0", got)
	}

	expiring := startElevation(t, pool, seededStart(t, pool))
	if got := gaugeAt(t, metrics); got != 1 {
		t.Fatalf("a second active elevation: %d, want 1", got)
	}
	clock = expiring.ExpiresAt.Add(time.Second)
	if got := gaugeAt(t, metrics); got != 0 {
		t.Fatalf("one second after its expiry, no reconciler pass: %d, want 0", got)
	}
	if read := readElevation(t, pool, expiring.ID); !read.EndedAt.IsZero() {
		t.Fatal("premise: something ended the expired elevation in the ledger")
	}
}

// Two collectors on one database (two replicas) report the same value.
func TestTwoReplicasReportTheSameElevationGauge(t *testing.T) {
	pool := newElevationDatabase(t)
	clock := func() time.Time { return elevationClock.Add(time.Minute) }
	startElevation(t, pool, seededStart(t, pool))
	startElevation(t, pool, seededStart(t, pool))
	first := adminaccess.NewBreakGlassMetrics(pool).WithClock(clock)
	second := adminaccess.NewBreakGlassMetrics(pool).WithClock(clock)
	a, b := gaugeAt(t, first), gaugeAt(t, second)
	if a != 2 || a != b {
		t.Fatalf("replica one reports %d, replica two %d; want 2 on both", a, b)
	}
}
