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

// SweepReport counts one sweep.
type SweepReport struct {
	Expired          int
	ForcedReclaims   int
	AbandonedReclaim int
	IdleErased       int
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

	// CLOUD_IDLE past the project's idle retention → erased. The claim reads
	// rows idle for at least one day; each row is checked against its
	// project's own retention.
	if due, err := s.repo.DueIdle(ctx, now.Add(-24*time.Hour), SweepBatch); err != nil {
		errs = append(errs, err)
	} else {
		projects := map[int64]idleProject{}
		for _, handoff := range due {
			project, seen := projects[handoff.ProjectID]
			if !seen {
				policy, err := s.policy.Effective(ctx, handoff.ProjectID)
				project = idleProject{retention: policy.IdleRetention, ok: err == nil && policy.IdleRetention > 0}
				projects[handoff.ProjectID] = project
			}
			if !project.ok || now.Sub(handoff.StateChangedAt) < project.retention {
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
	}

	// A thread whose conversation or project is gone is erased, whatever its
	// state (conversation delete, project delete).
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
				fmt.Sprintf("handoff.erase: conversation deleted, epoch %d", handoff.Epoch+1), true)
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
