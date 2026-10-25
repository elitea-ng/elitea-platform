package workspacehandoff

import (
	"context"
)

// EventType is the live event of a handoff state change (ADR-0034, scope B):
// the web app and the desktop refresh their Cloud executor notice, badges
// and take-back toast from it instead of polling.
const EventType = "workspace.handoff"

// StateEvent is what a state change tells a reader: the conversation, its
// state, epoch and whether a take back waits. Never stats, paths, device or
// note.
type StateEvent struct {
	ConversationUUID string `json:"conversation_uuid"`
	State            State  `json:"state"`
	Epoch            int64  `json:"epoch"`
	ReclaimPending   bool   `json:"reclaim_pending"`
}

// StateEvents receives every committed state change, best effort (a lost
// event is repaired by the clients' slow poll). Audience is the user ids
// that may read the thread: a Local work thread is its owner's alone.
type StateEvents interface {
	HandoffChanged(ctx context.Context, projectID int64, audience []int64, event StateEvent)
}

// WithStateEvents publishes every committed transition the repository makes
// (request-driven and the sweeper's) to events.
func (s *Service) WithStateEvents(events StateEvents) *Service {
	if events == nil {
		return s
	}
	s.repo = eventingRepository{Repository: s.repo, events: events}
	return s
}

// eventingRepository emits after each successful transition. It wraps the
// repository so no transition, including the sweeper's, can forget to. A
// call a retry replays (TransitionResult, EraseResult) emits only when it
// changed the thread: a replay would make every client re-read for nothing
// (#1311). Begin, Complete, Finish and the sweeper's transitions always
// change it (a retry of those fails instead).
type eventingRepository struct {
	Repository
	events StateEvents
}

func (r eventingRepository) emit(ctx context.Context, handoff Handoff) {
	if handoff.ProjectID <= 0 || handoff.ConversationUUID == "" {
		return
	}
	audience := []int64{}
	if handoff.OwnerUserID > 0 {
		audience = append(audience, handoff.OwnerUserID)
	}
	r.events.HandoffChanged(context.WithoutCancel(ctx), handoff.ProjectID, audience, StateEvent{
		ConversationUUID: handoff.ConversationUUID, State: handoff.State,
		Epoch: handoff.Epoch, ReclaimPending: handoff.ReclaimPending,
	})
}

func (r eventingRepository) Begin(ctx context.Context, record BeginRecord) (BeginResult, error) {
	result, err := r.Repository.Begin(ctx, record)
	if err == nil {
		r.emit(ctx, result.Handoff)
	}
	return result, err
}

func (r eventingRepository) Complete(ctx context.Context, record CompleteRecord) (Handoff, error) {
	return r.after(ctx)(r.Repository.Complete(ctx, record))
}

func (r eventingRepository) Reclaim(ctx context.Context, thread Thread, actorUserID, epoch int64, deviceID string) (TransitionResult, error) {
	return r.afterChange(ctx)(r.Repository.Reclaim(ctx, thread, actorUserID, epoch, deviceID))
}

func (r eventingRepository) ReclaimRest(ctx context.Context, thread Thread, actorUserID, epoch int64, deviceID string) (TransitionResult, error) {
	return r.afterChange(ctx)(r.Repository.ReclaimRest(ctx, thread, actorUserID, epoch, deviceID))
}

func (r eventingRepository) Release(ctx context.Context, record ReleaseRecord) (TransitionResult, error) {
	return r.afterChange(ctx)(r.Repository.Release(ctx, record))
}

func (r eventingRepository) Erase(ctx context.Context, record EraseRecord) (EraseResult, error) {
	result, err := r.Repository.Erase(ctx, record)
	if err == nil && result.Changed {
		owner := result.Handoff
		if owner.OwnerUserID == 0 {
			owner.OwnerUserID = result.Previous.OwnerUserID
		}
		r.emit(ctx, owner)
	}
	return result, err
}

func (r eventingRepository) StartCloudTurn(ctx context.Context, thread Thread, epoch int64, executionID, responseMessageID string) (TransitionResult, error) {
	return r.afterChange(ctx)(r.Repository.StartCloudTurn(ctx, thread, epoch, executionID, responseMessageID))
}

func (r eventingRepository) FinishCloudTurn(ctx context.Context, thread Thread, epoch int64, executionID string, message *EventMessage) (Handoff, error) {
	return r.after(ctx)(r.Repository.FinishCloudTurn(ctx, thread, epoch, executionID, message))
}

func (r eventingRepository) ExpireHandingOff(ctx context.Context, transition TimedTransition) (Handoff, error) {
	return r.after(ctx)(r.Repository.ExpireHandingOff(ctx, transition))
}

func (r eventingRepository) ForceReclaim(ctx context.Context, transition TimedTransition) (Handoff, error) {
	return r.after(ctx)(r.Repository.ForceReclaim(ctx, transition))
}

func (r eventingRepository) AbandonReclaim(ctx context.Context, transition TimedTransition) (Handoff, error) {
	return r.after(ctx)(r.Repository.AbandonReclaim(ctx, transition))
}

func (r eventingRepository) ExpireRestReclaim(ctx context.Context, transition TimedTransition) (Handoff, error) {
	return r.after(ctx)(r.Repository.ExpireRestReclaim(ctx, transition))
}

func (r eventingRepository) IdleErase(ctx context.Context, transition TimedTransition) (Handoff, error) {
	return r.after(ctx)(r.Repository.IdleErase(ctx, transition))
}

func (r eventingRepository) OrphanErase(ctx context.Context, transition TimedTransition) (Handoff, error) {
	return r.after(ctx)(r.Repository.OrphanErase(ctx, transition))
}

func (r eventingRepository) afterChange(ctx context.Context) func(TransitionResult, error) (TransitionResult, error) {
	return func(result TransitionResult, err error) (TransitionResult, error) {
		if err == nil && result.Changed {
			r.emit(ctx, result.Handoff)
		}
		return result, err
	}
}

func (r eventingRepository) after(ctx context.Context) func(Handoff, error) (Handoff, error) {
	return func(handoff Handoff, err error) (Handoff, error) {
		if err == nil {
			r.emit(ctx, handoff)
		}
		return handoff, err
	}
}
