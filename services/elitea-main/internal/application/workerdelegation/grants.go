package workerdelegation

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	dg "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/delegationgrant"
)

// The approved delegation grants a run carries (ADR-0044 amendment 1, story
// S2c). The worker reads one per composition edge Y to X as G(Y to X) in
// E(X) = own(X) ∩ (E(Y) ∪ G(Y to X)) ∩ P.
//
// The switch. Grants are sent, and the worker enforces them, only while the
// operator turns the grant switch on (ELITEA_DELEGATION_GRANTS_ENFORCED, off by
// default). It stays off until the migration of existing compositions (story
// S6) has created their pending legacy grants, in the same release; with it
// off a command is unchanged byte for byte and delegation behaves exactly as
// before grants. With it on, an edge with an approved grant is bounded by
// E(Y) ∪ G and an edge with none is refused (or recorded, in report mode) as
// it is today, so no composition that works is refused by the switch alone.

const (
	// MaxGrantEdges bounds the grants one command carries. A closure past it is
	// cut level by level from the root (the edges nearest the root first, and
	// within the level that overflows, by caller then callee id); the cut is
	// logged, because an edge that loses its grant is refused as an edge with
	// none.
	MaxGrantEdges = 256
	// maxClosureDepth is the deepest the closure is walked (the platform's
	// largest delegation depth).
	maxClosureDepth = 16
)

// GrantSource reads the grants a run may consume.
type GrantSource interface {
	// ConsumableFromCallers lists the approved and stale grants whose caller is
	// one of callerIDs.
	ConsumableFromCallers(ctx context.Context, projectID int64, callerIDs []int64) ([]dg.Grant, error)
}

// WithGrants composes the grant source and turns the grant switch on or off.
// Without a source, or with the switch off, no grant is sent.
func (r *Resolver) WithGrants(source GrantSource, enforced bool) *Resolver {
	r.grants, r.grantsEnforced = source, enforced
	return r
}

// closureGrants lists the consumable grants reachable from the root agent:
// the root's own edges, then the edges of every callee they reach, breadth
// first, each agent once. Only an approved edge can be reached, and only an
// approved edge matters: an edge with no grant adds nothing to E(Y) ∪ G.
func (r *Resolver) closureGrants(ctx context.Context, projectID, rootApplicationID int64) ([]*runtimev1.WorkerDelegationGrantV1, error) {
	seen := map[int64]bool{rootApplicationID: true}
	frontier := []int64{rootApplicationID}
	var out []*runtimev1.WorkerDelegationGrantV1
	for depth := 0; depth < maxClosureDepth && len(frontier) > 0 && len(out) < MaxGrantEdges; depth++ {
		grants, err := r.grants.ConsumableFromCallers(ctx, projectID, frontier)
		if err != nil {
			return nil, fmt.Errorf("worker delegation: read the delegation grants: %w", err)
		}
		var next []int64
		for _, grant := range grants {
			if len(out) >= MaxGrantEdges {
				break
			}
			out = append(out, grantMessage(grant))
			if callee := grant.Edge.CalleeEntityID; !seen[callee] {
				seen[callee] = true
				next = append(next, callee)
			}
		}
		frontier = next
	}
	if len(out) >= MaxGrantEdges {
		slog.WarnContext(ctx, "worker delegation: the grants of the run's closure reach the limit; edges past it carry none",
			"project_id", projectID, "root_application_id", rootApplicationID, "limit", MaxGrantEdges)
	}
	return out, nil
}

func grantMessage(grant dg.Grant) *runtimev1.WorkerDelegationGrantV1 {
	return &runtimev1.WorkerDelegationGrantV1{
		CallerApplicationId: uint64(grant.Edge.CallerEntityID), //nolint:gosec // a positive row id
		CalleeApplicationId: uint64(grant.Edge.CalleeEntityID), //nolint:gosec // a positive row id
		EdgeKind:            string(grant.Edge.Kind),
		GrantId:             uint64(grant.ID), //nolint:gosec // a positive row id
		Digest:              grant.Digest,
		GrantSet:            string(grant.GrantSet.Canonical()),
	}
}

// withGrants adds the run's grants to policy (building the default policy when
// the section was never saved) and returns it. It changes nothing while the
// switch is off, the run has no saved root, or no grant is reachable.
func (r *Resolver) withGrants(ctx context.Context, command *runtimev1.WorkerCommandV1, input []byte, policy *runtimev1.WorkerDelegationPolicyV1) (*runtimev1.WorkerDelegationPolicyV1, error) {
	if r.grants == nil || !r.grantsEnforced {
		return policy, nil
	}
	rootID := ApplicationID(input)
	if rootID <= 0 {
		return policy, nil
	}
	projectID, err := strconv.ParseInt(command.GetResourceProjectId(), 10, 64)
	if err != nil || projectID <= 0 {
		return policy, nil
	}
	grants, err := r.closureGrants(ctx, projectID, rootID)
	if err != nil || len(grants) == 0 {
		return policy, err
	}
	if policy == nil {
		policy = &runtimev1.WorkerDelegationPolicyV1{}
	}
	policy.Grants = grants
	policy.RootApplicationId = uint64(rootID) //nolint:gosec // positive, checked above
	policy.GrantsEnforced = true
	return policy, nil
}
