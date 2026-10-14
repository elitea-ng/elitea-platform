package cleanupjournal

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

type pointerJournal struct{}

func (*pointerJournal) EnqueueMemoryUsers(context.Context, pgx.Tx, int64, ...int64) (string, error) {
	return "memory_user", nil
}

type valueJournal struct{}

func (valueJournal) EnqueueMemoryUsers(context.Context, pgx.Tx, int64, ...int64) (string, error) {
	return "memory_user", nil
}

// Every MemoryUserJournal setter (the project members route, the
// administrators' user delete, SCIM) applies this one guard: a nil and a typed
// nil are both "not wired", anything else is kept.
func TestUsableMemoryUserJournalDropsNilAndTypedNil(t *testing.T) {
	t.Parallel()
	if UsableMemoryUserJournal(nil) != nil {
		t.Fatal("a nil journal was kept")
	}
	var typedNil *pointerJournal
	if UsableMemoryUserJournal(typedNil) != nil {
		t.Fatal("a typed nil journal was kept")
	}
	if UsableMemoryUserJournal(&pointerJournal{}) == nil {
		t.Fatal("a real pointer journal was dropped")
	}
	if UsableMemoryUserJournal(valueJournal{}) == nil {
		t.Fatal("a value journal was dropped")
	}
}
