package workerdelegation

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strconv"

	"google.golang.org/protobuf/proto"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	dg "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/delegationgrant"
)

// The approved delegation grants a run carries (ADR-0044 amendment 1, story
// S2c). The worker reads one per composition edge Y to X as G(Y to X) in
// E(X) = own(X) ∩ (E(Y) ∪ G(Y to X)) ∩ P.
//
// The switch. Grants are sent, and the worker enforces them, while the grant
// switch is on (ELITEA_DELEGATION_GRANTS_ENFORCED, on by default since the
// migration of existing compositions, story S6, ships in the same release and
// creates their pending legacy grants, never an approved one); with it off a
// command is unchanged byte for byte and delegation behaves exactly as before
// grants. With it on, an edge with an approved grant is bounded by E(Y) ∪ G and
// an edge with none is refused (or recorded, in report mode) as it is today,
// so no composition that works is refused by the switch alone.

const (
	// MaxGrantEdges bounds the grants one command carries. A closure past it is
	// cut level by level from the root (the edges nearest the root first, and
	// within the level that overflows, by caller then callee id); the cut is
	// logged, because an edge that loses its grant is refused as an edge with
	// none.
	MaxGrantEdges = 256
	// MaxGrantClosureNodes bounds the saved applications and pipelines visited
	// while walking reachable composition edges for a run's grant closure.
	MaxGrantClosureNodes = 256
	// MaxGrantClosureDepth is the deepest hop count walked from the root (the
	// platform's largest delegation depth).
	MaxGrantClosureDepth = 16
	maxClosureDepth      = MaxGrantClosureDepth
)

// GrantSource reads the grants a run may consume.
type GrantSource interface {
	// ConsumableFromCallers lists the approved and stale grants whose caller is
	// one of callerIDs.
	ConsumableFromCallers(ctx context.Context, projectID int64, callerIDs []int64) ([]dg.Grant, error)
}

// CompositionEdgeSource reads direct saved-application and pipeline attachment
// edges (caller application id -> callee application ids) strictly within one
// project. It lets closureGrants traverse intermediate hops whose callee holds
// no extra toolkits (and therefore has no grant row of its own) so approved
// grants at hop 2, hop 3, and beyond are still collected.
type CompositionEdgeSource interface {
	ReachableApplicationCallees(ctx context.Context, projectID int64, callerIDs []int64) (map[int64][]int64, error)
}

// WithGrants composes the grant source and turns the grant switch on or off.
// Without a source, or with the switch off, no grant is sent. If source also
// implements CompositionEdgeSource and no explicit edge source was set, it is
// wired for graph traversal automatically.
func (r *Resolver) WithGrants(source GrantSource, enforced bool) *Resolver {
	r.grants, r.grantsEnforced = source, enforced
	if r.edges == nil {
		if edges, ok := source.(CompositionEdgeSource); ok {
			r.edges = edges
		}
	}
	return r
}

// WithCompositionEdges sets the saved-application/pipeline graph reader used to
// expand reachable intermediate callers when collecting closure grants.
func (r *Resolver) WithCompositionEdges(source CompositionEdgeSource) *Resolver {
	r.edges = source
	return r
}

type grantDedupKey struct {
	id     int64
	caller int64
	callee int64
	kind   dg.EdgeKind
}

// closureGrants lists the consumable grants reachable from the root agent:
// breadth first across both approved grant edges and reachable saved
// application/pipeline composition edges, bounded by maxClosureDepth,
// MaxGrantClosureNodes, and MaxGrantEdges. Walking composition edges ensures
// that a hop without its own grant (for example an intermediate coordinator
// whose toolkits are a subset of its caller's) does not hide approved grants
// at hop 2, hop 3, or deeper.
func (r *Resolver) closureGrants(ctx context.Context, projectID, rootApplicationID int64, input []byte) ([]*runtimev1.WorkerDelegationGrantV1, error) {
	seen := map[int64]bool{rootApplicationID: true}
	frontier := []int64{rootApplicationID}
	rootSeedCallees := directAttachedApplicationIDs(input, projectID)
	emitted := map[grantDedupKey]bool{}
	var out []*runtimev1.WorkerDelegationGrantV1
	grantsTruncated := false
	nodesTruncated := false

	for depth := 0; depth < maxClosureDepth && len(frontier) > 0 && !grantsTruncated; depth++ {
		frontierSet := make(map[int64]bool, len(frontier))
		for _, callerID := range frontier {
			frontierSet[callerID] = true
		}
		grants, err := r.grants.ConsumableFromCallers(ctx, projectID, frontier)
		if err != nil {
			return nil, fmt.Errorf("worker delegation: read the delegation grants: %w", err)
		}
		nextCandidates := make([]int64, 0, len(grants))
		for _, grant := range grants {
			if grant.ProjectID != 0 && grant.ProjectID != projectID {
				continue
			}
			if grant.ID <= 0 || (grant.State != dg.StateApproved && grant.State != dg.StateStale) {
				continue
			}
			if grant.Edge.CallerEntityID <= 0 || grant.Edge.CalleeEntityID <= 0 || !frontierSet[grant.Edge.CallerEntityID] {
				continue
			}
			key := grantDedupKey{
				id:     grant.ID,
				caller: grant.Edge.CallerEntityID,
				callee: grant.Edge.CalleeEntityID,
				kind:   grant.Edge.Kind,
			}
			if emitted[key] {
				continue
			}
			if len(out) >= MaxGrantEdges {
				grantsTruncated = true
				break
			}
			emitted[key] = true
			out = append(out, grantMessage(grant))
			nextCandidates = append(nextCandidates, grant.Edge.CalleeEntityID)
		}
		if grantsTruncated || depth+1 >= maxClosureDepth {
			break
		}
		if depth == 0 && len(rootSeedCallees) > 0 {
			nextCandidates = append(nextCandidates, rootSeedCallees...)
		}
		if r.edges != nil {
			attached, err := r.edges.ReachableApplicationCallees(ctx, projectID, frontier)
			if err != nil {
				return nil, fmt.Errorf("worker delegation: read the composition edges: %w", err)
			}
			for _, caller := range frontier {
				callees := slices.Clone(attached[caller])
				slices.Sort(callees)
				for _, callee := range callees {
					if callee > 0 {
						nextCandidates = append(nextCandidates, callee)
					}
				}
			}
		}
		var next []int64
		for _, callee := range nextCandidates {
			if callee <= 0 || seen[callee] {
				continue
			}
			if len(seen) >= MaxGrantClosureNodes {
				nodesTruncated = true
				break
			}
			seen[callee] = true
			next = append(next, callee)
		}
		frontier = next
	}
	if grantsTruncated {
		slog.WarnContext(ctx, "worker delegation: the grants of the run's closure reach the limit; edges past it carry none",
			"project_id", projectID, "root_application_id", rootApplicationID, "limit", MaxGrantEdges)
	}
	if nodesTruncated {
		slog.WarnContext(ctx, "worker delegation: the composition closure of the run reached the node limit; deeper edges carry no grants",
			"project_id", projectID, "root_application_id", rootApplicationID, "limit", MaxGrantClosureNodes)
	}
	return out, nil
}

// directAttachedApplicationIDs extracts same-project application tool targets
// directly attached to the root input (application.version_details.tools and
// input.tools). Any foreign or non-canonical project_id fails closed and is
// ignored so traversal never crosses a project boundary.
func directAttachedApplicationIDs(content []byte, projectID int64) []int64 {
	if len(content) == 0 || projectID <= 0 {
		return nil
	}
	input := &runtimev1.AgentExecutionInputV1{}
	if err := proto.Unmarshal(content, input); err != nil {
		return nil
	}
	var ids []int64
	if len(input.GetApplication()) > 0 {
		var application struct {
			VersionDetails struct {
				Tools json.RawMessage `json:"tools"`
			} `json:"version_details"`
		}
		if err := json.Unmarshal(input.GetApplication(), &application); err == nil {
			ids = appendSameProjectApplicationTools(ids, application.VersionDetails.Tools, projectID)
		}
	}
	ids = appendSameProjectApplicationTools(ids, input.GetTools(), projectID)
	slices.Sort(ids)
	return slices.Compact(ids)
}

func appendSameProjectApplicationTools(dst []int64, raw json.RawMessage, projectID int64) []int64 {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return dst
	}
	var tools []struct {
		Type      string          `json:"type"`
		ProjectID json.RawMessage `json:"project_id"`
		Settings  struct {
			ProjectID     json.RawMessage `json:"project_id"`
			ApplicationID json.RawMessage `json:"application_id"`
		} `json:"settings"`
	}
	if err := json.Unmarshal(raw, &tools); err != nil {
		return dst
	}
	for _, tool := range tools {
		if tool.Type != "application" {
			continue
		}
		if !sameProjectJSON(tool.ProjectID, projectID) || !sameProjectJSON(tool.Settings.ProjectID, projectID) {
			continue
		}
		if appID := strictPositiveJSONInt(tool.Settings.ApplicationID); appID > 0 {
			dst = append(dst, appID)
		}
	}
	return dst
}

// sameProjectJSON reports whether an optional JSON project_id field is either
// absent/null or an exact canonical positive JSON integer equal to projectID.
func sameProjectJSON(raw json.RawMessage, projectID int64) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return true
	}
	return strictPositiveJSONInt(trimmed) == projectID
}

// strictPositiveJSONInt parses an exact canonical positive JSON integer (no
// quotes, decimals, whitespace, signs, or leading zeros), or returns 0.
func strictPositiveJSONInt(raw json.RawMessage) int64 {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || len(trimmed) > 18 || trimmed[0] < '1' || trimmed[0] > '9' {
		return 0
	}
	for _, b := range trimmed {
		if b < '0' || b > '9' {
			return 0
		}
	}
	id, err := strconv.ParseInt(string(trimmed), 10, 64)
	if err != nil || id <= 0 {
		return 0
	}
	return id
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
// switch is off or the run has no saved root. A root with no reachable grant
// still carries its id (see below).
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
	grants, err := r.closureGrants(ctx, projectID, rootID, input)
	if err != nil {
		return policy, err
	}
	// A root with no consumable grant still names itself: the worker puts the
	// root on the edge of a refusal and its notice (`caller_id`), which is how
	// Review delegation finds the calling agent. Sent only with the grant
	// switch on, which is also what makes an edge without a grant refused.
	if policy == nil {
		policy = &runtimev1.WorkerDelegationPolicyV1{}
	}
	policy.Grants = grants
	policy.RootApplicationId = uint64(rootID) //nolint:gosec // positive, checked above
	policy.GrantsEnforced = true
	return policy, nil
}
