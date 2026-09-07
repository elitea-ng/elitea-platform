package toolkitcatalogue

// The resolution order, in one place.
//
//	project grant row for THIS project   → decides, unless the policy disables
//	deployment policy row                → decides
//	no row at all                        → ALLOW (the shipping default)
//
// and then, outside this package and after it, the guardrails deny-list, which
// is terminal.
//
// # Why a `disabled` deployment policy beats a project grant
//
// The three availabilities are not a simple precedence ladder, and collapsing
// them into one would make the page lie. `restricted` is an ALLOW-LIST: it
// exists so that grants can admit projects, so a grant must win over it.
// `disabled` is a REFUSAL: an operator who disables a type has said "not on this
// platform", and a grant filed earlier for one project must not quietly survive
// that. If it did, disabling a type would leave it working somewhere, and the
// listing's own project count is the only place that would show it.
//
// `enabled` with a `disabled` grant is the remaining case, and there the grant
// wins: the deployment says yes, one project says no, and the narrower statement
// is the exception the grant table exists to record.

import "strings"

// Filter answers, for the project it was built for, whether one type is served.
//
// It is a VALUE built from one read, not a live query. A catalogue request asks
// about every type in the map; a filter that queried per type would turn one
// listing into fifty round trips, and the answers could disagree with each other
// inside a single response.
type Filter struct {
	// allowed is nil when nothing was decided, which is the common case. A nil
	// map reads as "no deviations", and Allows returns true for every key —
	// the absence rule this package exists to keep.
	decisions map[string]Decision
}

// EmptyFilter allows every type. It is what an unwired or unreadable source
// resolves to, and what a deployment with no rows gets.
func EmptyFilter() Filter { return Filter{} }

// Allows reports whether the type is served.
//
// An unknown type is ALLOWED. This filter subtracts from a catalogue it does not
// author: the candidate set comes from the SDK snapshot and the projections, and
// a type nobody has decided about is one this table has nothing to say about.
func (f Filter) Allows(toolkitType string) bool {
	decision, found := f.decisions[strings.TrimSpace(toolkitType)]
	if !found {
		return true
	}
	return decision.Allowed
}

// Decision returns the resolved decision for one type, including the layer that
// made it. The admin page renders it; the catalogue only calls Allows.
func (f Filter) Decision(toolkitType string) Decision {
	key := strings.TrimSpace(toolkitType)
	decision, found := f.decisions[key]
	if !found {
		return Decision{ToolkitType: key, Allowed: true, Source: SourceDefault}
	}
	return decision
}

// Len reports how many types carry a recorded deviation. Tests and the log line
// in the composition adapter use it; a listing that suddenly resolves zero
// deviations on a configured deployment is worth seeing.
func (f Filter) Len() int { return len(f.decisions) }

// BuildFilter resolves one project's view from the two row sets.
//
// `grants` may hold rows for other projects; only those whose ProjectID matches
// are read. That keeps the caller free to pass one project's rows or a
// pre-warmed set without the two behaving differently.
func BuildFilter(projectID int64, policies []Policy, grants []Grant) Filter {
	if len(policies) == 0 {
		// No deployment decision means no exception can apply either: the grant
		// table cascades off the policy table, so a grant with no policy row is
		// a row the database does not permit.
		return EmptyFilter()
	}

	grantByType := make(map[string]Grant, len(grants))
	for _, grant := range grants {
		if grant.ProjectID != projectID {
			continue
		}
		grantByType[grant.ToolkitType] = grant
	}

	decisions := make(map[string]Decision, len(policies))
	for _, policy := range policies {
		decisions[policy.ToolkitType] = resolveOne(policy, grantByType[policy.ToolkitType])
	}
	return Filter{decisions: decisions}
}

// resolveOne applies the order this file's header states. The zero Grant means
// "no grant row", which its empty ToolkitType distinguishes from a real one.
func resolveOne(policy Policy, grant Grant) Decision {
	decision := Decision{ToolkitType: policy.ToolkitType, Source: SourceDeployment, Reason: policy.Reason}

	switch policy.Availability {
	case AvailabilityDisabled:
		// Terminal within this package. See the header: a refusal that one
		// stale grant could survive is a refusal an operator cannot trust.
		decision.Allowed = false
		return decision
	case AvailabilityRestricted:
		decision.Allowed = false
	case AvailabilityEnabled:
		decision.Allowed = true
	default:
		// A stored value outside the CHECK cannot exist, so this branch is
		// unreachable through the database. It resolves to ALLOW rather than
		// refuse, because a row this build cannot read must not take a toolkit
		// away from a deployment that never asked for that.
		decision.Allowed = true
		decision.Source = SourceDefault
		decision.Reason = ""
		return decision
	}

	if grant.ToolkitType == "" {
		return decision
	}
	return Decision{
		ToolkitType: policy.ToolkitType,
		Allowed:     grant.Availability == AvailabilityEnabled,
		Source:      SourceProject,
		Reason:      grant.Reason,
	}
}
