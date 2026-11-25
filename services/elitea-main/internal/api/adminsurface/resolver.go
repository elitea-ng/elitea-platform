package adminsurface

import (
	"context"
	"sync"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

// Resolver is the recording permission resolver. It holds no state of its
// own: every request Drive sends carries a recorder in its context, and the
// resolver answers from it. A resolution with no recorder in its context (a
// goroutine that outlived its request, or a request Drive did not send) is
// refused and counted as an orphan, so it cannot pass unnoticed.
//
// It implements auth.PermissionResolver and auth.MembershipPermissionResolver.
type Resolver struct {
	mu      sync.Mutex
	orphans []Call
}

// Call is one permission resolution a request made.
type Call struct {
	Mode      string
	ProjectID string
	// Caller is the function that asked, e.g.
	// ".../internal/api/v2/admin.(*Handler).hasAdministrationPermission".
	Caller string
	// Gate is true when the caller is in internal/api/middleware, in front of
	// the handler (RequireCentralPermissions and its siblings, the maintenance
	// window), false when the check is in a handler. The in-handler helpers of
	// that package (HoldsPermissionIn, HoldsResolvedPermission) name the
	// handler that called them instead.
	Gate bool
}

// Orphans answers the resolutions that reached the resolver without a recorder.
func (r *Resolver) Orphans() []Call {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Call(nil), r.orphans...)
}

// ResolvePermissions answers through the request's recorder.
func (r *Resolver) ResolvePermissions(
	ctx context.Context, _ auth.User, mode string, projectID string,
) (auth.PermissionResolution, error) {
	return r.answer(ctx, mode, projectID)
}

// ResolveMembershipPermissions answers through the request's recorder, as a
// resolution with no project.
func (r *Resolver) ResolveMembershipPermissions(
	ctx context.Context, _ auth.User, mode string,
) (auth.PermissionResolution, error) {
	return r.answer(ctx, mode, "")
}

func (r *Resolver) answer(ctx context.Context, mode, projectID string) (auth.PermissionResolution, error) {
	call := Call{Mode: mode, ProjectID: projectID}
	call.Caller, call.Gate = caller()
	rec, ok := ctx.Value(recorderKey{}).(*recorder)
	if !ok {
		r.mu.Lock()
		r.orphans = append(r.orphans, call)
		r.mu.Unlock()
		return auth.PermissionResolution{}, auth.ErrPermissionDenied
	}
	permissions := rec.record(call)
	resolution := auth.PermissionResolution{UserID: principalUserID, Permissions: permissions}
	if rec.elevation != nil && mode == auth.PermissionModeAdministration {
		// No standing set: everything the answer holds the elevation gave.
		resolution.ElevationSessionRef = ElevatedSessionRef
		resolution.ElevatedPermissions = append([]string(nil), permissions...)
		resolution.ElevationID = ElevationID
		resolution.ElevationIDs = []string{ElevationID}
		resolution.Elevations = []auth.ElevationGrant{{
			ID: ElevationID, Level: rec.elevation.Level, Path: rec.elevation.Path,
		}}
	}
	return resolution, nil
}

// principalUserID is the numeric id of the synthetic principal.
const principalUserID = 1

type recorderKey struct{}

// recorder holds one request's resolutions and the probe that answers them.
type recorder struct {
	mu sync.Mutex
	// grant answers the n-th administration-mode resolution of the request.
	grant func(n int) []string
	// others answers every other mode.
	others []string
	calls  []Call
	admin  int
	// elevation makes every administration-mode answer elevated
	// (Variant.Elevation).
	elevation *Elevation
	flagged   bool
}

func (rec *recorder) record(call Call) []string {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.calls = append(rec.calls, call)
	if call.Mode != auth.PermissionModeAdministration {
		return append([]string{}, rec.others...)
	}
	n := rec.admin
	rec.admin++
	return append([]string{}, rec.grant(n)...)
}

// flag records that the request's admin audit row was marked for the
// elevation review.
func (rec *recorder) flag() {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.flagged = true
}

func (rec *recorder) isFlagged() bool {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.flagged
}

func (rec *recorder) snapshot() []Call {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return append([]Call(nil), rec.calls...)
}
