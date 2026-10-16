package scimdirectory

import (
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitcleanup"
)

// The SCIM setter applies the same guard as the core and admin handlers
// (cleanupjournal.UsableMemoryUserJournal): a typed nil is not wired, so a
// group withdrawal never calls into a nil journal.
func TestWithMemoryUserJournalIgnoresATypedNil(t *testing.T) {
	t.Parallel()
	var journal *toolkitcleanup.Journal
	if store := NewStore(nil).WithMemoryUserJournal(journal); store.memoryUsers != nil {
		t.Fatal("a typed nil journal was wired")
	}
	if store := NewStore(nil).WithMemoryUserJournal(nil); store.memoryUsers != nil {
		t.Fatal("a nil journal was wired")
	}
	real := toolkitcleanup.New(nil, nil)
	if store := NewStore(nil).WithMemoryUserJournal(real); store.memoryUsers == nil {
		t.Fatal("a real journal was dropped")
	}
}
