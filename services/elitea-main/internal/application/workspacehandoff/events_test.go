package workspacehandoff

import (
	"context"
	"errors"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspacestore"
)

// replayRepo answers every replayable transition with the same result. The
// other methods are not called; the embedded nil interface panics if one is.
type replayRepo struct {
	Repository
	result TransitionResult
	err    error
}

func (r replayRepo) Reclaim(context.Context, Thread, int64, int64, string) (TransitionResult, error) {
	return r.result, r.err
}

func (r replayRepo) Release(context.Context, ReleaseRecord) (TransitionResult, error) {
	return r.result, r.err
}

func (r replayRepo) StartCloudTurn(context.Context, Thread, int64, string, string) (TransitionResult, error) {
	return r.result, r.err
}

type countingStateEvents struct{ events []StateEvent }

func (c *countingStateEvents) HandoffChanged(_ context.Context, _ int64, _ []int64, event StateEvent) {
	c.events = append(c.events, event)
}

// #1311: a retried take back or release and a repeated claim redemption
// change nothing, so they publish no live event; a real change publishes one.
func TestReplayedTransitionsPublishNoLiveEvent(t *testing.T) {
	handoff := Handoff{ProjectID: 1, ConversationUUID: "10000000-0000-4000-8000-000000000001",
		State: workspacestore.StateReclaiming, Epoch: 3, OwnerUserID: 11}
	calls := map[string]func(Repository) error{
		"reclaim": func(r Repository) error {
			_, err := r.Reclaim(context.Background(), Thread{}, 11, 2, "")
			return err
		},
		"release": func(r Repository) error {
			_, err := r.Release(context.Background(), ReleaseRecord{})
			return err
		},
		"start cloud turn": func(r Repository) error {
			_, err := r.StartCloudTurn(context.Background(), Thread{}, 2, "exec", "response")
			return err
		},
	}
	for name, call := range calls {
		for _, test := range []struct {
			result TransitionResult
			err    error
			want   int
		}{
			{result: TransitionResult{Handoff: handoff}, want: 0},
			{result: TransitionResult{Handoff: handoff, Changed: true}, want: 1},
			{result: TransitionResult{Handoff: handoff, Changed: true}, err: errors.New("refused"), want: 0},
		} {
			live := &countingStateEvents{}
			repo := eventingRepository{Repository: replayRepo{result: test.result, err: test.err}, events: live}
			if err := call(repo); !errors.Is(err, test.err) {
				t.Fatalf("%s: err=%v", name, err)
			}
			if len(live.events) != test.want {
				t.Fatalf("%s (changed=%t, err=%v): %d events, want %d", name, test.result.Changed, test.err, len(live.events), test.want)
			}
		}
	}
}
