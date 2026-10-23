package workspacehandoff

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspacestore"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
)

// idleRepo is the CLOUD_IDLE part of the repository. The other methods are
// not called by sweepIdle; the embedded nil interface panics if one is.
type idleRepo struct {
	Repository
	mu       sync.Mutex
	rows     []Handoff
	warned   map[string]time.Time
	erased   map[string]bool
	pageSize []int
}

func (r *idleRepo) DueIdle(_ context.Context, after IdleCursor, limit int) ([]Handoff, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []Handoff{}
	for _, row := range r.rows {
		if r.erased[row.ConversationUUID] || !idleAfter(row, after) {
			continue
		}
		out = append(out, row)
		if len(out) == limit {
			break
		}
	}
	r.pageSize = append(r.pageSize, len(out))
	return out, nil
}

func idleAfter(row Handoff, after IdleCursor) bool {
	if !row.StateChangedAt.Equal(after.StateChangedAt) {
		return row.StateChangedAt.After(after.StateChangedAt)
	}
	if row.ProjectID != after.ProjectID {
		return row.ProjectID > after.ProjectID
	}
	return row.ConversationUUID > after.ConversationUUID
}

func (r *idleRepo) NotifyIdleExpiring(_ context.Context, handoff Handoff, deleteAt time.Time) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.warned[handoff.ConversationUUID]; ok {
		return false, nil
	}
	r.warned[handoff.ConversationUUID] = deleteAt
	return true, nil
}

func (r *idleRepo) IdleErase(_ context.Context, transition TimedTransition) (Handoff, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.erased[transition.Thread.ConversationUUID] = true
	return Handoff{}, nil
}

type retentionPolicy map[int64]time.Duration

func (p retentionPolicy) Effective(_ context.Context, projectID int64) (Policy, error) {
	return Policy{IdleRetention: p[projectID]}, nil
}

type noEraser struct{}

func (noEraser) EraseThreadObjects(context.Context, Thread) (bool, error) { return true, nil }

type noCanceller struct{}

func (noCanceller) CancelCloudTurn(context.Context, int64, int64, string) error { return nil }

type noAudit struct{}

func (noAudit) Record(context.Context, audit.Event) {}

// The idle walk is keyset paged and continues across sweeps: a long list of
// idle threads of a project with a long retention does not hide another
// project's thread that is due. The owner is warned once, 3 days before the
// erasure.
func TestSweepIdleReachesEveryRowAndWarnsOnce(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	repo := &idleRepo{warned: map[string]time.Time{}, erased: map[string]bool{}}
	// 1,050 threads of project 1 (30-day retention), idle 25 days: older
	// than everything else, not due, not yet in the warning window.
	for i := 0; i < 1050; i++ {
		repo.rows = append(repo.rows, Handoff{
			ProjectID: 1, ConversationUUID: fmt.Sprintf("00000000-0000-4000-8000-%012d", i),
			State: workspacestore.StateCloudIdle, Epoch: 2, StateChangedAt: now.Add(-25 * day),
		})
	}
	// Project 1, idle 28 days: inside the 3-day window.
	warn := Handoff{ProjectID: 1, ConversationUUID: "00000000-0000-4000-8000-100000000001",
		State: workspacestore.StateCloudIdle, Epoch: 2, StateChangedAt: now.Add(-28 * day)}
	// Project 2 (3-day retention), idle 5 days: due, and the newest row.
	due := Handoff{ProjectID: 2, ConversationUUID: "00000000-0000-4000-8000-200000000002",
		State: workspacestore.StateCloudIdle, Epoch: 3, StateChangedAt: now.Add(-5 * day)}
	repo.rows = append(repo.rows, warn, due)
	sort.Slice(repo.rows, func(i, j int) bool {
		return idleAfter(repo.rows[j], IdleCursor{}.Next(repo.rows[i]))
	})

	service, err := NewService(repo, retentionPolicy{1: 30 * day, 2: 3 * day}, noEraser{}, noCanceller{}, noAudit{},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	service.WithClock(func() time.Time { return now })

	first := SweepReport{}
	if err := service.sweepIdle(context.Background(), now, &first); err != nil {
		t.Fatal(err)
	}
	// The oldest row (28 days) is first in the walk, so it is warned now;
	// the project 2 row is past the 1,000 rows one sweep walks.
	if first.IdleWarned != 1 || first.IdleErased != 0 || repo.erased[due.ConversationUUID] {
		t.Fatalf("first sweep = %+v", first)
	}
	if got := repo.warned[warn.ConversationUUID]; !got.Equal(warn.StateChangedAt.Add(30 * day)) {
		t.Fatalf("warning deadline = %v, want the idle deadline", got)
	}
	second := SweepReport{}
	if err := service.sweepIdle(context.Background(), now, &second); err != nil {
		t.Fatal(err)
	}
	if second.IdleErased != 1 || !repo.erased[due.ConversationUUID] || second.IdleWarned != 0 {
		t.Fatalf("second sweep = %+v; the walk did not continue to the due row", second)
	}
	// The walk wrapped: the next sweep starts at the beginning again and
	// warns nobody twice.
	third := SweepReport{}
	if err := service.sweepIdle(context.Background(), now, &third); err != nil {
		t.Fatal(err)
	}
	if third.IdleWarned != 0 || third.IdleErased != 0 || len(repo.warned) != 1 {
		t.Fatalf("third sweep = %+v, warned %d", third, len(repo.warned))
	}
}

// Idle retention 0 is the minimum (one day), never "keep forever": Narrow
// clamps it, and the sweeper erases at the minimum even if a policy source
// answers 0.
func TestIdleRetentionZeroIsTheMinimum(t *testing.T) {
	zero := int64(0)
	got := Narrow(amendedCeiling(), ProjectSetting{IdleRetentionDays: &zero, ReturnGraceDays: &zero}, true, true)
	if got.IdleRetention != MinIdleRetention || got.ReturnGrace != 0 {
		t.Fatalf("project 0 = idle %s, grace %s; want %s and 0 (erase at release)", got.IdleRetention, got.ReturnGrace, MinIdleRetention)
	}
	ceiling := amendedCeiling()
	ceiling.IdleRetentionDays = 0
	if got := Narrow(ceiling, ProjectSetting{}, true, true); got.IdleRetention != MinIdleRetention {
		t.Fatalf("ceiling 0 = %s", got.IdleRetention)
	}

	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	repo := &idleRepo{warned: map[string]time.Time{}, erased: map[string]bool{}, rows: []Handoff{
		{ProjectID: 1, ConversationUUID: "00000000-0000-4000-8000-000000000001", State: "CLOUD_IDLE", Epoch: 2, StateChangedAt: now.Add(-25 * time.Hour)},
		{ProjectID: 1, ConversationUUID: "00000000-0000-4000-8000-000000000002", State: "CLOUD_IDLE", Epoch: 2, StateChangedAt: now.Add(-2 * time.Hour)},
	}}
	service, err := NewService(repo, retentionPolicy{1: 0}, noEraser{}, noCanceller{}, noAudit{},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	report := SweepReport{}
	if err := service.sweepIdle(context.Background(), now, &report); err != nil {
		t.Fatal(err)
	}
	if !repo.erased["00000000-0000-4000-8000-000000000001"] || repo.erased["00000000-0000-4000-8000-000000000002"] || report.IdleErased != 1 {
		t.Fatalf("retention 0: erased %v, report %+v; want the thread idle over a day erased", repo.erased, report)
	}
}

// The scan event records counts and rule ids only: no attribute, allowed or
// not, can carry a path.
func TestScanBlockedRecordsNoPath(t *testing.T) {
	keys := executorEventKeys[EventScanBlocked]
	if _, ok := keys["files"]; ok {
		t.Fatal("scan_blocked allows a files attribute")
	}
	for _, value := range []string{"./a.txt", "a/b", "secrets.txt", "secrets.txt ", "1 2", "~/x", "id_rsa.pub"} {
		for name, valid := range keys {
			if valid(value) {
				t.Fatalf("scan_blocked %s accepts %q", name, value)
			}
		}
	}
	if !keys["findings"]("3") || !keys["rules"]("aws-access-key,generic-api-key") || keys["rules"]("rule,./p") {
		t.Fatal("the scan attributes refuse their own shapes")
	}
}
