package securitycontext

import "context"

// RunAuthority is the explicit authority of an unattended governed run
// (ADR-0039, issue #1122): the mandate attached to a pipeline schedule, an
// inbound pipeline trigger or an index schedule, as the fire-time check read
// it. Admission puts it in the run's security context instead of inferring
// one from the run's user (repos.recordSecurityContext).
type RunAuthority struct {
	// MandateID is the attached mandate.
	MandateID string
	// MandateEpoch is the mandate's revocation epoch at the fire-time
	// check. Admission refuses the run when the stored epoch moved since.
	MandateEpoch int64
	// SubjectUserID is the mandate's subject user; 0 for an autonomous
	// mandate, whose run has no subject principal.
	SubjectUserID int64
	// Caller is how the run was started, recorded in CallerContextV1 only:
	// an inbound trigger names its authentication mode and trigger here.
	// Zero for a schedule.
	Caller Caller
}

type runAuthorityKey struct{}

// WithRunAuthority carries a for the admission that ctx reaches. Only an
// unattended governed entry point sets it.
func WithRunAuthority(ctx context.Context, a RunAuthority) context.Context {
	return context.WithValue(ctx, runAuthorityKey{}, a)
}

// RunAuthorityFrom reads the authority WithRunAuthority carried.
func RunAuthorityFrom(ctx context.Context) (RunAuthority, bool) {
	a, ok := ctx.Value(runAuthorityKey{}).(RunAuthority)
	return a, ok && a.MandateID != ""
}
