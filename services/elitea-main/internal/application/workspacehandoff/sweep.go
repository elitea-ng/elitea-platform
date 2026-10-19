package workspacehandoff

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/workspacestore"
)

// SweepBatch is how many rows one sweep step handles.
const SweepBatch = 50

// IdleWarningBefore is how long before the idle-retention erasure the owner is
// warned (ADR-0034 decision 12).
const IdleWarningBefore = 3 * 24 * time.Hour

// DeadTurnGrace is how long past the end of its last lease a running cloud
// turn's execution must be without a live claim before the sweeper ends the
// turn: the claim lease TTL (runtimecomposition claimLeaseTTL, 30 s), so a
// worker that renews on time is never reaped, and a dead one within a lease
// and a sweep (30 s) more.
const DeadTurnGrace = 30 * time.Second

// idleSweepPages caps the CLOUD_IDLE pages one sweep walks. The walk goes on
// from the same place at the next sweep.
const idleSweepPages = 20

// SweepReport counts one sweep.
type SweepReport struct {
	Expired          int
	ForcedReclaims   int
	AbandonedReclaim int
	DeadTurnsReaped  int
	IdleErased       int
	IdleWarned       int
	OrphansErased    int
	ObjectsErased    int
}

// idleProject is one project's idle retention. The sweeper reads it per
// project because a project can only shorten the platform value.
type idleProject struct {
	retention time.Duration
	ok        bool
}

// Sweep runs the time-driven transitions and the thread erasure (decisions 7
// and 12). Every transition is an epoch compare-and-set, so replicas sweeping
// at once change each row once; the claims take FOR UPDATE SKIP LOCKED
// batches. A failure of one row is logged and the sweep moves on.
func (s *Service) Sweep(ctx context.Context) (SweepReport, error) {
	now := s.now()
	report := SweepReport{}
	var errs []error

	// HANDING_OFF for 15 minutes without a complete → LOCAL, objects erased.
	if due, err := s.repo.DueHandingOff(ctx, now, SweepBatch); err != nil {
		errs = append(errs, err)
	} else {
		for _, handoff := range due {
			if _, err := s.repo.ExpireHandingOff(ctx, TimedTransition{Thread: threadOf(handoff), Epoch: handoff.Epoch}); err != nil {
				if !errors.Is(err, ErrStaleEpoch) {
					errs = append(errs, err)
				}
				continue
			}
			report.Expired++
			s.record(ctx, threadOf(handoff), handoff.OwnerUserID, "", "", now,
				fmt.Sprintf("handoff.expired: epoch %d", handoff.Epoch), true)
		}
	}

	// A take back during a turn: after the grace, cancel the turn and move to
	// RECLAIMING with a new epoch, so a late publish is refused.
	if due, err := s.repo.DueReclaimGrace(ctx, now.Add(-ReclaimGrace), SweepBatch); err != nil {
		errs = append(errs, err)
	} else {
		for _, handoff := range due {
			s.cancelRunning(ctx, handoff)
			if _, err := s.repo.ForceReclaim(ctx, TimedTransition{Thread: threadOf(handoff), Epoch: handoff.Epoch}); err != nil {
				if !errors.Is(err, ErrStaleEpoch) {
					errs = append(errs, err)
				}
				continue
			}
			report.ForcedReclaims++
			s.record(ctx, threadOf(handoff), handoff.OwnerUserID, "", "", now,
				fmt.Sprintf("handoff.reclaim: turn cancelled after the grace, epoch %d", handoff.Epoch+1), true)
		}
	}

	// A cloud turn whose worker died without reporting: its execution holds
	// no live lease past the grace. The turn ends as failed (nothing is
	// published: the publish is the worker's, and a late one is refused) and
	// the thread goes back to CLOUD_IDLE, or to RECLAIMING when a take back
	// waits; the chat answer is stopped the way the stop control does.
	if due, err := s.repo.DeadCloudTurns(ctx, DeadTurnGrace, SweepBatch); err != nil {
		errs = append(errs, err)
	} else {
		for _, handoff := range due {
			finished, err := s.repo.FinishCloudTurn(ctx, threadOf(handoff), handoff.Epoch, handoff.RunningExecutionID, nil)
			if err != nil {
				// Someone else ended the turn or erased the thread first.
				if !errors.Is(err, ErrStaleEpoch) && !errors.Is(err, ErrNotFound) {
					errs = append(errs, err)
				}
				continue
			}
			s.cancelRunning(ctx, handoff)
			report.DeadTurnsReaped++
			s.record(ctx, threadOf(handoff), handoff.OwnerUserID, "", "", now,
				fmt.Sprintf("handoff.turn_reaped: execution %s held no live lease, %s, epoch %d",
					truncate(handoff.RunningExecutionID, 64), finished.State, finished.Epoch), true)
		}
	}

	// RECLAIMING abandoned for 7 days → CLOUD_IDLE.
	if due, err := s.repo.DueAbandonedReclaim(ctx, now.Add(-ReclaimAbandonedAfter), SweepBatch); err != nil {
		errs = append(errs, err)
	} else {
		for _, handoff := range due {
			if _, err := s.repo.AbandonReclaim(ctx, TimedTransition{Thread: threadOf(handoff), Epoch: handoff.Epoch}); err != nil {
				if !errors.Is(err, ErrStaleEpoch) {
					errs = append(errs, err)
				}
				continue
			}
			report.AbandonedReclaim++
		}
	}

	// CLOUD_IDLE: past the project's idle retention → erased; within
	// IdleWarningBefore of it → the owner is warned once. The walk is keyset
	// paged and continues across sweeps, so every idle row is reached
	// whatever the other projects' retentions are.
	if err := s.sweepIdle(ctx, now, &report); err != nil {
		errs = append(errs, err)
	}

	// A thread whose conversation or project is gone, or whose owner is
	// offboarded (deleted or suspended), is erased, whatever its state
	// (conversation delete, project delete, user offboarding).
	s.mu.Lock()
	after := s.orphanAt
	s.mu.Unlock()
	orphans, next, err := s.repo.OrphanedThreads(ctx, after, SweepBatch)
	if err != nil {
		errs = append(errs, err)
	} else {
		s.mu.Lock()
		s.orphanAt = next
		s.mu.Unlock()
		for _, handoff := range orphans {
			s.cancelRunning(ctx, handoff)
			if _, err := s.repo.OrphanErase(ctx, TimedTransition{Thread: threadOf(handoff), Epoch: handoff.Epoch}); err != nil {
				if !errors.Is(err, ErrStaleEpoch) {
					errs = append(errs, err)
				}
				continue
			}
			report.OrphansErased++
			s.record(ctx, threadOf(handoff), handoff.OwnerUserID, "", "", now,
				fmt.Sprintf("handoff.erase: conversation, project or owner gone, epoch %d", handoff.Epoch+1), true)
		}
	}

	// The objects of every LOCAL thread whose erasure is due. deleted_at is set
	// only after a listing of the thread prefix came back empty.
	if due, err := s.repo.ClaimErasures(ctx, now, SweepBatch); err != nil {
		errs = append(errs, err)
	} else {
		for _, handoff := range due {
			if err := s.eraseThread(ctx, handoff); err != nil {
				errs = append(errs, err)
				continue
			}
			if view, ok, err := s.repo.Read(ctx, threadOf(handoff)); err == nil && ok && view.DeletedAt != nil {
				report.ObjectsErased++
			}
		}
	}
	return report, errors.Join(errs...)
}

func threadOf(handoff Handoff) Thread {
	return workspacestore.Thread{ProjectID: handoff.ProjectID, ConversationUUID: handoff.ConversationUUID}
}

// sweepIdle walks the CLOUD_IDLE rows from the stored cursor.
func (s *Service) sweepIdle(ctx context.Context, now time.Time, report *SweepReport) error {
	s.mu.Lock()
	cursor := s.idleAt
	s.mu.Unlock()
	projects := map[int64]idleProject{}
	var errs []error
	for page := 0; page < idleSweepPages; page++ {
		due, err := s.repo.DueIdle(ctx, cursor, SweepBatch)
		if err != nil {
			return errors.Join(append(errs, err)...)
		}
		for _, handoff := range due {
			cursor = cursor.Next(handoff)
			project, seen := projects[handoff.ProjectID]
			if !seen {
				policy, err := s.policy.Effective(ctx, handoff.ProjectID)
				// A retention of 0 is never "forever": the minimum applies.
				retention := max(policy.IdleRetention, MinIdleRetention)
				project = idleProject{retention: retention, ok: err == nil}
				projects[handoff.ProjectID] = project
			}
			if !project.ok {
				continue
			}
			deadline := handoff.StateChangedAt.Add(project.retention)
			if now.Before(deadline) {
				if !now.Before(deadline.Add(-IdleWarningBefore)) {
					warned, err := s.repo.NotifyIdleExpiring(ctx, handoff, deadline)
					if err != nil {
						errs = append(errs, err)
					} else if warned {
						report.IdleWarned++
					}
				}
				continue
			}
			message := EventMessage{Event: "handoff.erase", Text: EraseMessage}
			if _, err := s.repo.IdleErase(ctx, TimedTransition{Thread: threadOf(handoff), Epoch: handoff.Epoch, Message: &message}); err != nil {
				if !errors.Is(err, ErrStaleEpoch) {
					errs = append(errs, err)
				}
				continue
			}
			report.IdleErased++
			s.record(ctx, threadOf(handoff), handoff.OwnerUserID, "", "", now,
				fmt.Sprintf("handoff.erase: idle retention, epoch %d", handoff.Epoch+1), true)
		}
		if len(due) < SweepBatch {
			// The end of the walk: the next sweep starts again.
			cursor = IdleCursor{}
			break
		}
	}
	s.mu.Lock()
	s.idleAt = cursor
	s.mu.Unlock()
	return errors.Join(errs...)
}
