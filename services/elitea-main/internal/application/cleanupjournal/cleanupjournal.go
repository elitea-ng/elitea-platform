// Package cleanupjournal holds what the two cleanup journals share: the
// project-delete journal (internal/application/projectprovisioning, #1245) and
// the toolkit-delete journal (internal/application/toolkitcleanup, #1244). Each
// journal records, in the transaction that deletes a row, the slow or external
// work the row's removal owes, and a reconciler performs it afterwards. The
// pieces here are the ones both need word for word: the bookkeeping context of
// a journal write, the typed-nil guard of a wiring option, and the class texts
// a step or row is closed with when an engine answer cannot be retried.
//
// The journals themselves stay apart on purpose: they key their rows
// differently (a project id against a surrogate id) and report different
// terminal errors, and #1245's journal is covered by its own tests as it stands.
package cleanupjournal

import (
	"context"
	"reflect"
	"time"

	"github.com/jackc/pgx/v5"
)

// BookkeepingTimeout bounds each journal write a run makes, on a context of its
// own so a run that hit its deadline still records what it did. A variable so a
// test can shrink it.
var BookkeepingTimeout = 10 * time.Second

// BookkeepingContext is the context for one journal write: detached from the
// run's (which may have ended at its deadline) with a short bound of its own.
func BookkeepingContext(runCtx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(runCtx), BookkeepingTimeout)
}

// IsNilPointer reports a nil pointer wrapped in a non-nil interface. A wiring
// option uses it because the composition root declares the interface type, and
// a typed nil would pass the nil check and panic inside a journal run. The same
// helper internal/infra/authattempt uses, for the same reason.
func IsNilPointer(value any) bool {
	reflected := reflect.ValueOf(value)
	return reflected.Kind() == reflect.Pointer && reflected.IsNil()
}

// MemoryUserJournal records, in the transaction that removes users from a
// project (projectID > 0) or deletes them outright (projectID 0: every
// project), that their PERSONAL memories owe the vector store a deletion
// (worker unit M9). *toolkitcleanup.Journal satisfies it. It is the one
// interface every hook (the project members route, the administrators' user
// delete, SCIM group sync) is wired with, through UsableMemoryUserJournal.
type MemoryUserJournal interface {
	EnqueueMemoryUsers(ctx context.Context, tx pgx.Tx, projectID int64, userIDs ...int64) (string, error)
}

// UsableMemoryUserJournal is the guard every MemoryUserJournal setter applies:
// it answers nil for a nil journal and for a typed nil (a nil pointer in the
// interface, which would pass a plain nil check and panic inside the delete's
// transaction), and the journal otherwise.
func UsableMemoryUserJournal(journal MemoryUserJournal) MemoryUserJournal {
	if journal == nil || IsNilPointer(journal) {
		return nil
	}
	return journal
}

// The notes a step or row is closed with when the engine's answer cannot be
// retried. They are CLASS TEXTS, never the engine's message: a note is stored in
// the journal and reported by DELETE of a project (StepStatus.Msg), and the raw
// error can carry an address or the engine's statement of what it refused; the
// raw error goes to the log, with the row's ids, and nowhere else.
const (
	// NoteUnsupported: the engine does not implement the deletion
	// (UNIMPLEMENTED). It keeps nothing of this kind.
	NoteUnsupported = "engine does not support deletion"
	// NoteRefused: the engine refused this process's client certificate
	// (platformops.ErrNotAllowed). The allowlist or the material is wrong, and
	// a retry cannot fix it.
	NoteRefused = "failed permanently: the engine refused this process's client certificate"
	// NoteInvalidRequest: the request itself was invalid (platformops.
	// ErrConfig); a retry cannot fix it either.
	NoteInvalidRequest = "failed permanently: the request was invalid"
)
