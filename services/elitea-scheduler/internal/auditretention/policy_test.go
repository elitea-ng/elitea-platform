package auditretention

// The administrator's retention policy for `admin_audit` (ADR-0042 decision
// 4, issue #1495): followed once saved, the deployment value until then.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-scheduler/internal/retentionpolicy"
)

func policyReader(decision retentionpolicy.Decision, err error) retentionpolicy.Reader {
	return func(context.Context) (retentionpolicy.Decision, error) { return decision, err }
}

func newPolicySweeper(t *testing.T, store Store, envDays int, policy retentionpolicy.Reader, at time.Time) *Sweeper {
	t.Helper()
	sweeper, err := NewWithPolicy(store, openGate, Config{RetentionDays: envDays, BatchSize: 100, MaxBatchesPerPass: 5}, policy, nil)
	if err != nil {
		t.Fatalf("NewWithPolicy: %v", err)
	}
	sweeper.now = func() time.Time { return at }
	return sweeper
}

func TestAnUnsavedPolicyKeepsTheDeploymentWindow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	store := &recordingStore{}
	stats, err := newPolicySweeper(t, store, 365, policyReader(retentionpolicy.Decision{}, nil), now).Sweep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.WindowDays != 365 || !stats.Cutoff.Equal(now.Add(-365*24*time.Hour)) {
		t.Fatalf("window %d cutoff %s, want the deployment's 365 days", stats.WindowDays, stats.Cutoff)
	}
}

func TestASavedPolicyWins(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	days := 30
	store := &recordingStore{}
	stats, err := newPolicySweeper(t, store, 365,
		policyReader(retentionpolicy.Decision{Set: true, MaxAgeDays: &days}, nil), now).Sweep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.WindowDays != 30 || len(store.calls) != 1 {
		t.Fatalf("window %d, %d statements; want the policy's 30 days", stats.WindowDays, len(store.calls))
	}
}

func TestKeepForeverAndAnUnreadablePolicyDeleteNothing(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	for name, reader := range map[string]retentionpolicy.Reader{
		"keep forever": policyReader(retentionpolicy.Decision{Set: true}, nil),
		"unreadable":   policyReader(retentionpolicy.Decision{}, errors.New("db down")),
	} {
		store := &recordingStore{}
		_, err := newPolicySweeper(t, store, 365, reader, now).Sweep(context.Background())
		if name == "unreadable" && err == nil {
			t.Errorf("%s: want an error", name)
		}
		if len(store.calls) != 0 {
			t.Errorf("%s: issued %d DELETE(s)", name, len(store.calls))
		}
	}
}

// A deployment that switched the sweep off still starts it with a reader, and
// it deletes nothing until a policy is saved.
func TestADisabledDeploymentWaitsForAPolicy(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	store := &recordingStore{}
	stats, err := newPolicySweeper(t, store, 0, policyReader(retentionpolicy.Decision{}, nil), now).Sweep(context.Background())
	if err != nil || !stats.KeptForever || len(store.calls) != 0 {
		t.Fatalf("stats %+v err %v calls %d", stats, err, len(store.calls))
	}
	short := 3
	_, err = newPolicySweeper(t, store, 0,
		policyReader(retentionpolicy.Decision{Set: true, MaxAgeDays: &short}, nil), now).Sweep(context.Background())
	if !errors.Is(err, ErrWindowTooShort) || len(store.calls) != 0 {
		t.Fatalf("a policy below the floor: %v, %d calls", err, len(store.calls))
	}
}
