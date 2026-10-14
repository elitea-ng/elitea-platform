package admin

import (
	"context"
	"errors"
	"testing"
)

type recordingMemoryUsers struct {
	calls [][2]int64
}

func (r *recordingMemoryUsers) DeleteMemoryUser(_ context.Context, projectID, userID int64) error {
	r.calls = append(r.calls, [2]int64{projectID, userID})
	if userID == 11 {
		return errors.New("elitea-vector is down")
	}
	return nil
}

// A deleted user's personal memories are deleted in every project they were a
// member of; one failure is logged and does not stop the rest.
func TestDeletingUsersDeletesTheirPersonalMemoriesInEveryProject(t *testing.T) {
	t.Parallel()
	remover := &recordingMemoryUsers{}
	handler := &Handler{}
	WithMemoryUserRemover(remover)(handler)
	handler.removeMemoriesOf(context.Background(), []membership{{7, 10}, {8, 10}, {7, 11}, {9, 12}})
	if len(remover.calls) != 4 {
		t.Fatalf("calls = %v", remover.calls)
	}
	(&Handler{}).removeMemoriesOf(context.Background(), []membership{{7, 10}}) // no remover: nothing
	if got := (&Handler{}).membershipsOf(context.Background(), []int{10}); got != nil {
		t.Fatalf("a handler without a remover read memberships: %v", got)
	}
}
