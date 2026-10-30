package platformconfig

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SectionCredentialReview is the evidence for credential and access review
// (ADR-0044 decision 5, #1501): when a credential counts as stale, what the
// daily staleness routine does with a stale one, and the window of the
// permission trimming report.
//
// A key that is absent keeps the default. It is LIVE: the routines read it
// at every pass and the inventory and trimming reads at every request.
const SectionCredentialReview = "credential_review"

// Keys of SectionCredentialReview.
const (
	// KeyStaleAfterDays: a credential not used for this many days (or never
	// used since it was created that long ago) is stale. Default 90.
	KeyStaleAfterDays = "stale_after_days"
	// KeyStaleAction is flag (the default) or revoke. revoke revokes stale
	// personal access tokens and ADR-0039 credential use grants; toolkit
	// secrets, MCP OAuth tokens and credential references are flagged
	// only, because they are issued by an external system that rotates and
	// revokes them (ADR-0044 "Delegated outside the platform").
	KeyStaleAction = "stale_action"
	// KeyTrimmingWindowDays is the window of the permission trimming
	// report: a grant unused in this many days is a removal candidate.
	// Default 30.
	KeyTrimmingWindowDays = "trimming_window_days"
)

// StaleAction is what the staleness routine does with a stale credential.
type StaleAction string

const (
	StaleActionFlag   StaleAction = "flag"
	StaleActionRevoke StaleAction = "revoke"
)

// Valid reports whether the action is known.
func (a StaleAction) Valid() bool { return a == StaleActionFlag || a == StaleActionRevoke }

// Bounds and defaults of the section.
const (
	DefaultStaleAfterDays     = 90
	MinStaleAfterDays         = 1
	MaxStaleAfterDays         = 3650
	DefaultTrimmingWindowDays = 30
	MinTrimmingWindowDays     = 1
	MaxTrimmingWindowDays     = 365
)

// CredentialReviewPolicy is the section read with its defaults.
type CredentialReviewPolicy struct {
	StaleAfterDays     int
	StaleAction        StaleAction
	TrimmingWindowDays int
}

// DefaultCredentialReviewPolicy is the ADR default: flag after 90 days,
// a 30-day trimming window.
func DefaultCredentialReviewPolicy() CredentialReviewPolicy {
	return CredentialReviewPolicy{
		StaleAfterDays:     DefaultStaleAfterDays,
		StaleAction:        StaleActionFlag,
		TrimmingWindowDays: DefaultTrimmingWindowDays,
	}
}

// CredentialReviewPolicyFromValues overlays the section on the defaults. A
// number out of range is clamped; an unknown action keeps flag.
func CredentialReviewPolicyFromValues(values Values) CredentialReviewPolicy {
	policy := DefaultCredentialReviewPolicy()
	if n, ok := values.Int(KeyStaleAfterDays); ok {
		policy.StaleAfterDays = int(min(max(n, MinStaleAfterDays), MaxStaleAfterDays))
	}
	if action := StaleAction(values.String(KeyStaleAction, "")); action.Valid() {
		policy.StaleAction = action
	}
	if n, ok := values.Int(KeyTrimmingWindowDays); ok {
		policy.TrimmingWindowDays = int(min(max(n, MinTrimmingWindowDays), MaxTrimmingWindowDays))
	}
	return policy
}

// LoadCredentialReviewPolicy reads the section. A database without the
// table never saved it and answers the defaults.
func LoadCredentialReviewPolicy(ctx context.Context, pool *pgxpool.Pool) (CredentialReviewPolicy, error) {
	values, err := Load(ctx, pool, SectionCredentialReview)
	if UndefinedTable(err) {
		return DefaultCredentialReviewPolicy(), nil
	}
	if err != nil {
		return CredentialReviewPolicy{}, err
	}
	return CredentialReviewPolicyFromValues(values), nil
}
