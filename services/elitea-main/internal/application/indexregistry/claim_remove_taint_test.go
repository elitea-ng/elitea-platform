package indexregistry

import (
	"context"
	"errors"
	"testing"
)

type fakeTaint struct {
	tainted bool
	err     error
	asked   []string
}

func (f *fakeTaint) ExecutionConversationTainted(_ context.Context, executionID string, generation int64) (bool, error) {
	f.asked = append(f.asked, executionID)
	if generation != 1 {
		return false, errors.New("unexpected generation")
	}
	return f.tainted, f.err
}

// remove_index is a shared-index write: a claim whose conversation read
// personal memory removes nothing, and a doubt about the taint removes
// nothing either (worker unit M9).
func TestClaimRemoverRefusesARemovalFromATaintedConversation(t *testing.T) {
	ctx := context.Background()

	f := newRemoveFixture(t)
	taint := &fakeTaint{tainted: true}
	f.remover.WithConversationTaint(taint)
	disposition, err := f.remover.RemoveIndex(ctx, validClaim(), validRemoval())
	if err != nil || disposition != RemovalSharedWriteRefused {
		t.Fatalf("tainted: disposition=%q err=%v", disposition, err)
	}
	if len(f.store.deleted) != 0 || len(f.vectors.deleted) != 0 {
		t.Fatalf("a tainted conversation removed an index: rows %v vectors %v", f.store.deleted, f.vectors.deleted)
	}
	if len(taint.asked) != 1 || taint.asked[0] != "execution-1" {
		t.Fatalf("taint asked for %v", taint.asked)
	}

	failing := newRemoveFixture(t)
	failing.remover.WithConversationTaint(&fakeTaint{err: errors.New("db down")})
	if _, err := failing.remover.RemoveIndex(ctx, validClaim(), validRemoval()); !errors.Is(err, ErrClaimRemovalUnavailable) {
		t.Fatalf("a failed taint read: err=%v", err)
	}
	if len(failing.store.deleted) != 0 {
		t.Fatal("a failed taint read removed an index")
	}

	clean := newRemoveFixture(t)
	clean.remover.WithConversationTaint(&fakeTaint{})
	if disposition, err := clean.remover.RemoveIndex(ctx, validClaim(), validRemoval()); err != nil || disposition != RemovalRemoved {
		t.Fatalf("untainted: disposition=%q err=%v", disposition, err)
	}
}
