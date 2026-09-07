// Package toolkitcatalogue holds the operator's decision about which toolkit
// TYPES this deployment offers, and to which projects.
//
// It is the store and the resolver behind the `Admin › Toolkits` page and
// behind the served type catalogue's project filter. Shared migration 0114
// creates the two tables.
//
// # The one rule this package exists to keep
//
// ABSENCE IS NOT A DECISION TO WITHHOLD. A deployment with no rows serves the
// full default catalogue. Every function here that cannot answer — no pool, no
// tables, a failed read — resolves to ALLOW, and says so in a log line. The
// opposite choice would take the whole toolkit chooser down whenever this small
// table is unreadable, to enforce a policy that is empty on most deployments.
//
// # Where this sits in the served catalogue
//
//	SDK snapshot / projections   → the candidate types
//	  ∧ this package's Filter    → the operator's policy and per-project grants
//	  ∧ guardrails deny-list     → terminal; never overruled by a policy row
//
// The guardrail step runs LAST and is not this package's business. A type an
// operator marks `enabled` here does not re-enter a catalogue the deny-list
// blocks it out of.
package toolkitcatalogue

import (
	"fmt"
	"strings"
	"time"
)

// Availability is what an operator decided about one toolkit type.
type Availability string

const (
	// AvailabilityEnabled serves the type to every project.
	AvailabilityEnabled Availability = "enabled"
	// AvailabilityDisabled serves the type to no project. A project grant
	// cannot re-admit it — see Resolve.
	AvailabilityDisabled Availability = "disabled"
	// AvailabilityRestricted serves the type only to projects that carry an
	// enabled grant row.
	//
	// A third value rather than "disabled plus grants" because the two read
	// differently to the operator who has to revisit the decision: disabled is
	// a refusal, restricted is an allow-list that is expected to have members.
	AvailabilityRestricted Availability = "restricted"
)

// ParseAvailability accepts the three stored spellings and nothing else.
//
// It refuses rather than defaulting. A write path that turned an unknown word
// into "enabled" would answer 200 to a request that asked for something it did
// not do, and the operator would read the page as if their decision had landed.
func ParseAvailability(value string) (Availability, error) {
	switch Availability(strings.TrimSpace(value)) {
	case AvailabilityEnabled:
		return AvailabilityEnabled, nil
	case AvailabilityDisabled:
		return AvailabilityDisabled, nil
	case AvailabilityRestricted:
		return AvailabilityRestricted, nil
	default:
		return "", fmt.Errorf(
			"toolkitcatalogue: availability %q is not one of enabled, disabled, restricted", value)
	}
}

// ParseGrantAvailability accepts the two spellings a per-project grant may
// carry. `restricted` is meaningless on a grant: a grant IS the exception, so
// there is nothing further to restrict it to.
func ParseGrantAvailability(value string) (Availability, error) {
	switch Availability(strings.TrimSpace(value)) {
	case AvailabilityEnabled:
		return AvailabilityEnabled, nil
	case AvailabilityDisabled:
		return AvailabilityDisabled, nil
	default:
		return "", fmt.Errorf(
			"toolkitcatalogue: project grant availability %q is not one of enabled, disabled", value)
	}
}

// Policy is one row of centry.toolkit_type_policy.
type Policy struct {
	ToolkitType  string
	Availability Availability
	Reason       string
	DecidedBy    string
	DecidedAt    time.Time
}

// Grant is one row of centry.toolkit_type_project_grant.
type Grant struct {
	ToolkitType  string
	ProjectID    int64
	Availability Availability
	Reason       string
	GrantedBy    string
	GrantedAt    time.Time
}

// Source names the layer that decided one type's availability. The admin page
// renders it as a chip, so an operator can see WHICH control is holding a type
// off rather than guessing between four of them.
type Source string

const (
	// SourceDefault means no row decided anything: the type is served because
	// nothing said otherwise. This is the value a fresh deployment has for
	// every type, and it must never render as a refusal.
	SourceDefault Source = "default"
	// SourceDeployment means a centry.toolkit_type_policy row decided it.
	SourceDeployment Source = "deployment"
	// SourceProject means a centry.toolkit_type_project_grant row for the
	// project in question decided it.
	SourceProject Source = "project"
)

// Decision is the resolved answer for one type, in one project.
type Decision struct {
	ToolkitType string
	Allowed     bool
	Source      Source
	// Reason is the operator's own sentence when a row decided this, and empty
	// when nothing did. It is never invented: "no decision was recorded" is
	// carried by Source, not by a manufactured explanation.
	Reason string
}

// PolicyValidationError reports a body this package refuses to store.
//
// A distinct type so the HTTP layer can answer 400 for it and 503 for a read or
// write failure, without matching on message text.
type PolicyValidationError struct{ Message string }

func (e PolicyValidationError) Error() string { return e.Message }

// ValidateReason enforces the CHECK the migration also enforces.
//
// Both, on purpose. The database constraint is what makes the rule true for
// every writer including psql; this function is what makes the refusal a 400
// with a sentence instead of a 500 with a constraint name.
func ValidateReason(reason string) (string, error) {
	trimmed := strings.TrimSpace(reason)
	if trimmed == "" {
		return "", PolicyValidationError{
			Message: "a reason is required: the page shows it beside the decision, " +
				"and a decision with no reason is one the next operator cannot safely reverse",
		}
	}
	if len(trimmed) > maxReasonLength {
		return "", PolicyValidationError{
			Message: fmt.Sprintf("the reason must be %d characters or fewer", maxReasonLength),
		}
	}
	return trimmed, nil
}

// ValidateToolkitType bounds the key without normalising it.
//
// NO NORMALISATION, deliberately. The key must match the served catalogue's map
// key byte for byte — the Pydantic schema title, e.g. `kubernetes`, not the SDK
// import key `k8s`. Lower-casing or underscoring here would produce a row that
// looks right in the admin listing and matches nothing in the catalogue, which
// is a control that appears to work and does nothing.
func ValidateToolkitType(toolkitType string) (string, error) {
	trimmed := strings.TrimSpace(toolkitType)
	if trimmed == "" {
		return "", PolicyValidationError{Message: "a toolkit type is required"}
	}
	if len(trimmed) > maxToolkitTypeLength {
		return "", PolicyValidationError{
			Message: fmt.Sprintf("the toolkit type must be %d characters or fewer", maxToolkitTypeLength),
		}
	}
	return trimmed, nil
}

const (
	// maxToolkitTypeLength matches the migration's CHECK.
	maxToolkitTypeLength = 256
	// maxReasonLength bounds a free-text column the migration leaves unbounded.
	// The column is TEXT because a reason is prose; the bound is here so one
	// paste cannot put a megabyte into a row every catalogue listing reads.
	maxReasonLength = 4096
)
