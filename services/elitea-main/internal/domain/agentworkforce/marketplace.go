package agentworkforce

// Workforce managers on the Marketplace (ADR-0051 decision 6, issue 1632).
//
// A workforce manager (capabilities.can_review_agents) searches the
// Marketplace, reads a listing and its feedback, and leaves two kinds of
// proposal in the table of the dismissal ladder:
//
//   - hire_listing: hire a listing version. A person opens the hire wizard
//     filled in from it. Under the auto_hire policy the platform hires it
//     itself when CheckListingHire answers "".
//   - feedback_report: a report on the origin listing version of a hired
//     agent the workforce is dismissing or demoting. A person approves it, or
//     the auto_feedback policy publishes it when the stage is applied.
//
// An agent never decides either: the first is the person's hire, the second
// is published by a person (or by the policy a person set). Reviews stay
// people only.

// Kinds of proposal beyond the ladder's dismiss and demote.
const (
	KindHireListing    = "hire_listing"
	KindFeedbackReport = "feedback_report"
	// MaxMarketplaceItems bounds the feedback items a tool returns.
	MaxMarketplaceItems = 20
	// MaxOpenHires bounds the open hire proposals of one project, so a
	// runaway agent cannot fill the inbox.
	MaxOpenHires = 20
	// MaxReportChars bounds a report's text, in characters: the same bound
	// as marketplace.MaxReportBody.
	MaxReportChars = 500
)

// Tool outcome codes the Marketplace tools add to propose_dismissal's.
const (
	OutcomeUnknownListing  = "listing_unknown"
	OutcomeNotHireable     = "not_hireable"
	OutcomeNotHired        = "not_hired"
	OutcomeOwnListing      = "own_listing"
	OutcomeNoDismissal     = "no_dismissal"
	OutcomeAlreadyReported = "already_reported"
	OutcomeTooManyOpen     = "too_many_open"
)

// IsMarketplaceKind reports a proposal kind the Marketplace tools write.
func IsMarketplaceKind(kind string) bool {
	return kind == KindHireListing || kind == KindFeedbackReport
}

// Report outcomes (listing feedback outcome).
const (
	ReportKept      = "kept"
	ReportDemoted   = "demoted"
	ReportDismissed = "dismissed"
)

// ValidReportOutcome reports a known outcome.
func ValidReportOutcome(outcome string) bool {
	return outcome == ReportKept || outcome == ReportDemoted || outcome == ReportDismissed
}

// ListingHireEvidence is what CheckListingHire reads about one hire proposal.
type ListingHireEvidence struct {
	Mode string
	// Tier is the requirements' tier of the version.
	Tier string
	// HasWriteTools: some tool of the version writes, sends, pays or runs code.
	HasWriteTools bool
	// Unbound lists the requirement types no project toolkit can be bound to.
	Unbound []string
	// Reviews and RatingAvg are the visible reviews of this version.
	Reviews   int
	RatingAvg float64
	// DismissalRate is the listing's, nil when nothing was measured.
	DismissalRate *float64
	// OpenAutoHired counts the project's automatic hires nobody reviewed.
	OpenAutoHired int
	// Undone: an agent manager undid an automatic hire of the listing in the
	// last SuppressAfterObjection.
	Undone bool
}

// Reasons CheckListingHire gives for leaving a proposal to a person.
const (
	ListingNotInstall   = "not_install_mode"
	ListingNotLowTier   = "not_low_tier"
	ListingWriteTools   = "has_write_tools"
	ListingUnbound      = "requirement_not_bindable"
	ListingFewReviews   = "too_few_reviews"
	ListingLowRating    = "rating_below_minimum"
	ListingDismissal    = "dismissal_rate_not_under_maximum"
	ListingNoDismissals = "dismissal_rate_unmeasured"
)

// CheckListingHire answers "" when a hire proposal may be hired automatically
// (ADR-0051 decision 6): install mode, low tier, every tool read-only, every
// requirement bindable to an existing project toolkit, at least
// ListingMinReviews reviews of the version averaging ListingMinRating, and a
// dismissal rate under ListingMaxDismissalRate. Otherwise it answers why it
// waits for a person.
func CheckListingHire(policy Policy, e ListingHireEvidence) string {
	switch {
	case !policy.AutoHireEnabled:
		return HirePolicyDisable
	case e.Mode != "install":
		return ListingNotInstall
	case e.Tier != TierLow:
		return ListingNotLowTier
	case e.HasWriteTools:
		return ListingWriteTools
	case len(e.Unbound) > 0:
		return ListingUnbound
	case e.Reviews < policy.ListingMinReviews:
		return ListingFewReviews
	case e.RatingAvg < policy.ListingMinRating:
		return ListingLowRating
	case e.DismissalRate == nil:
		return ListingNoDismissals
	case *e.DismissalRate >= policy.ListingMaxDismissalRate:
		return ListingDismissal
	case e.OpenAutoHired >= policy.MaxAutoHired:
		return HireCapReached
	case e.Undone:
		return HireUndoneRecent
	}
	return ""
}
