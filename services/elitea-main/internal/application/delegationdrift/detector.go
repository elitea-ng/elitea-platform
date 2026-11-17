// Package delegationdrift detects that the callee of an approved delegation
// grant now holds more than the grant lists (ADR-0044 amendment 1, A1.2 rule
// 3). A wider set marks the grant stale and records what changed, and the
// caller's owners and editors are told once per edge and digest. A same or
// smaller set needs nothing. When the closure of an approved edge grows the
// editors are told and nothing changes state.
//
// The check is idempotent and reads the database as it is, so it runs from
// every place a callee can change (a request that writes an agent, a toolkit
// or the registry; a Marketplace install, upgrade or merge) and, as a backstop,
// when the caller's grants are read. The worker needs none of it: a stale
// grant already narrows the callee by set containment at run time.
package delegationdrift

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	dg "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/delegationgrant"
)

// MaxClosureDepth bounds the closure a check walks (the platform's default
// delegation depth cap).
const MaxClosureDepth = 4

// Store reads and writes the tracked grants.
type Store interface {
	ListTracked(ctx context.Context, projectID int64, calleeIDs []int64) ([]dg.Tracked, error)
	ApplyDrift(ctx context.Context, projectID, grantID int64, expect dg.State, verdict dg.Verdict, closure dg.Closure) (dg.DriftApplied, error)
	ConsumableFromCallers(ctx context.Context, projectID int64, callerIDs []int64) ([]dg.Grant, error)
}

// Callees reads what a callee version holds now.
type Callees interface {
	Snapshot(ctx context.Context, projectID, applicationID, versionID int64) (dg.Callee, error)
	AttachedVersions(ctx context.Context, projectID, callerID, calleeID int64) ([]int64, error)
}

// Detector is the drift check.
type Detector struct {
	store   Store
	callees Callees
}

// New builds the detector.
func New(store Store, callees Callees) (*Detector, error) {
	if store == nil || callees == nil {
		return nil, errors.New("delegation drift: a store and a callee reader are required")
	}
	return &Detector{store: store, callees: callees}, nil
}

// Result counts what a check did.
type Result struct {
	Checked  int
	Stale    int
	Restored int
	// Notified is the number of people told (stale and closure notices).
	Notified int
}

// CheckProject checks every approved and stale grant of the project.
func (d *Detector) CheckProject(ctx context.Context, projectID int64) (Result, error) {
	return d.check(ctx, projectID, nil, 0)
}

// CheckCallees checks the grants into the given callees: the check a change
// to those agents asks for.
func (d *Detector) CheckCallees(ctx context.Context, projectID int64, calleeIDs []int64) (Result, error) {
	if len(calleeIDs) == 0 {
		return Result{}, nil
	}
	return d.check(ctx, projectID, calleeIDs, 0)
}

// CheckCaller checks the grants of one caller's edges: what the grant list of
// the caller runs, so an editor who opens it never sees a stale view of drift.
func (d *Detector) CheckCaller(ctx context.Context, projectID, callerID int64) (Result, error) {
	return d.check(ctx, projectID, nil, callerID)
}

func (d *Detector) check(ctx context.Context, projectID int64, calleeIDs []int64, callerID int64) (Result, error) {
	tracked, err := d.store.ListTracked(ctx, projectID, calleeIDs)
	if err != nil {
		return Result{}, err
	}
	var result Result
	var errs []error
	for _, item := range tracked {
		if callerID > 0 && item.Grant.Edge.CallerEntityID != callerID {
			continue
		}
		result.Checked++
		if err := d.checkOne(ctx, projectID, item, &result); err != nil {
			// One edge failing must not hide drift on the others.
			errs = append(errs, fmt.Errorf("grant %d: %w", item.Grant.ID, err))
		}
	}
	return result, errors.Join(errs...)
}

func (d *Detector) checkOne(ctx context.Context, projectID int64, item dg.Tracked, result *Result) error {
	verdict, err := d.assess(ctx, projectID, item)
	if err != nil {
		return err
	}
	closure, err := d.closure(ctx, projectID, item.Grant.Edge.CalleeEntityID)
	if err != nil {
		return err
	}
	if verdict.Action == dg.DriftNone && sameClosure(item.Closure, closure) {
		return nil
	}
	applied, err := d.store.ApplyDrift(ctx, projectID, item.Grant.ID, item.Grant.State, verdict, closure)
	if err != nil {
		return err
	}
	if !applied.Changed {
		return nil
	}
	switch verdict.Action {
	case dg.DriftMarkStale:
		result.Stale++
		slog.InfoContext(ctx, "delegation drift: the callee holds more than its grant",
			"project_id", projectID, "grant_id", item.Grant.ID, "callee_application_id", item.Grant.Edge.CalleeEntityID,
			"added", len(verdict.Drift.Added))
	case dg.DriftRestore:
		result.Restored++
	}
	result.Notified += applied.NotifiedStale + applied.NotifiedClosure
	return nil
}

// assess reads the callee versions the caller attaches (or the version the
// grant was taken from, when it attaches none) and answers the verdict of the
// widest one: any attached version that holds more than the grant makes it
// drift, and the grant comes back only when every one is within it.
func (d *Detector) assess(ctx context.Context, projectID int64, item dg.Tracked) (dg.Verdict, error) {
	edge := item.Grant.Edge
	versions, err := d.callees.AttachedVersions(ctx, projectID, edge.CallerEntityID, edge.CalleeEntityID)
	if err != nil {
		return dg.Verdict{}, err
	}
	if len(versions) == 0 {
		versions = []int64{item.Grant.CalleeVersionID}
	}
	best := dg.Verdict{Action: dg.DriftNone}
	rank := func(v dg.Verdict) int {
		switch {
		case v.Action == dg.DriftMarkStale || v.Action == dg.DriftUpdate:
			return 3
		case v.Action == dg.DriftNone && v.Drift.Digest != "":
			return 2
		case v.Action == dg.DriftRestore:
			return 1
		}
		return 0
	}
	for _, version := range versions {
		callee, err := d.callees.Snapshot(ctx, projectID, edge.CalleeEntityID, version)
		if errors.Is(err, dg.ErrCalleeNotFound) {
			continue // a deleted version holds nothing, so it cannot widen anything
		}
		if err != nil {
			return dg.Verdict{}, err
		}
		verdict := dg.Assess(item.Grant, callee.Set, version, item.Drift)
		if rank(verdict) > rank(best) {
			best = verdict
		}
	}
	return best, nil
}

// closure is the callees reachable from the edge's callee through approved
// grants, to MaxClosureDepth, with the digest of the grant into each.
func (d *Detector) closure(ctx context.Context, projectID, calleeID int64) (dg.Closure, error) {
	closure := dg.Closure{}
	seen := map[int64]bool{calleeID: true}
	frontier := []int64{calleeID}
	for depth := 0; depth < MaxClosureDepth && len(frontier) > 0; depth++ {
		grants, err := d.store.ConsumableFromCallers(ctx, projectID, frontier)
		if err != nil {
			return nil, err
		}
		var next []int64
		for _, grant := range grants {
			callee := grant.Edge.CalleeEntityID
			closure[callee] = grant.Digest
			if !seen[callee] {
				seen[callee] = true
				next = append(next, callee)
			}
		}
		frontier = next
	}
	return closure, nil
}

func sameClosure(a, b dg.Closure) bool {
	if a == nil {
		return false // never read: the first reading is recorded as the baseline
	}
	if len(a) != len(b) {
		return false
	}
	for id, digest := range a {
		if other, ok := b[id]; !ok || other != digest {
			return false
		}
	}
	return true
}
