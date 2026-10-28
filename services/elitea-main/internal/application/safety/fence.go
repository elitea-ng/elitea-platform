package safety

import "context"

// The admission fence (ADR-0041 decision 1): what closes the window between
// the Gate's cached check and the admission's commit.
//
// The Gate's snapshot can be up to MaxAge old, and it is read before and
// outside the admission transaction. Without more, a run admitted in that
// window would be neither refused (the snapshot predates the suspension) nor
// cancelled (the suspension's cancel ran before the run's job committed).
//
// The fence serialises the two on one PostgreSQL advisory lock:
//
//   - every admission transaction takes it SHARED as its first statement and
//     then re-reads the active suspensions that cover its subject (a direct,
//     indexed read; the Gate stays the cheap first filter);
//   - CreateSuspension takes it EXCLUSIVE as its first statement, then
//     inserts the row and cancels the covered runs.
//
// Both run READ COMMITTED, so each statement after the lock sees everything
// committed before the lock was granted:
//
//   - the suspension commits first: the admission's re-read, which runs after
//     its shared lock is granted, sees the row and refuses the run;
//   - the admission holds the shared lock first: the suspension waits for its
//     commit, and its cancel UPDATE sees the committed job and cancels it.
//
// Shared holders never block each other, so admissions do not serialise
// among themselves; only a suspension being written stops them, briefly.

// AdmissionFenceLockName is the advisory lock's name; the key is
// hashtextextended(AdmissionFenceLockName, 0), the convention of the other
// advisory locks of this service.
const AdmissionFenceLockName = "elitea.safety.admission_fence"

type admissionSubjectKey struct{}

// WithAdmissionSubject marks ctx as an admission the kill switch fences, for
// subject. The admission decorators set it after the Gate's check passes; the
// admission stores read it inside their transaction.
func WithAdmissionSubject(ctx context.Context, subject Subject) context.Context {
	return context.WithValue(ctx, admissionSubjectKey{}, subject)
}

// AdmissionSubject is the subject WithAdmissionSubject stored. ok is false
// for an admission no gate fences (the gate is not composed).
func AdmissionSubject(ctx context.Context) (subject Subject, ok bool) {
	subject, ok = ctx.Value(admissionSubjectKey{}).(Subject)
	return subject, ok
}
