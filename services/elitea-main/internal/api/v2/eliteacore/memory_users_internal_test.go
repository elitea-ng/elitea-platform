package eliteacore

import (
	"context"
	"errors"
	"testing"
)

type recordingMemoryUsers struct {
	calls [][2]int64
	fail  map[int64]bool
}

func (r *recordingMemoryUsers) DeleteMemoryUser(_ context.Context, projectID, userID int64) error {
	r.calls = append(r.calls, [2]int64{projectID, userID})
	if r.fail[userID] {
		return errors.New("elitea-vector is down")
	}
	return nil
}

// A user removed from a project has their personal memories there deleted,
// user by user; one failure is logged and does not stop the others or undo the
// removal; without a remover nothing is asked.
func TestRemovingUsersFromAProjectDeletesTheirPersonalMemories(t *testing.T) {
	t.Parallel()
	remover := &recordingMemoryUsers{fail: map[int64]bool{11: true}}
	handler := NewHandler(nil, WithMemoryUserRemover(remover))
	handler.removeMemoriesOf(context.Background(), 7, []int{10, 11, 12})
	if len(remover.calls) != 3 || remover.calls[0] != [2]int64{7, 10} || remover.calls[2] != [2]int64{7, 12} {
		t.Fatalf("calls = %v", remover.calls)
	}
	NewHandler(nil).removeMemoriesOf(context.Background(), 7, []int{10}) // no remover: no panic
	var typedNil *recordingMemoryUsers
	if NewHandler(nil, WithMemoryUserRemover(typedNil)).memoryUsers != nil {
		t.Fatal("a typed nil remover was stored")
	}
}
